package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// ProviderOpenAI is the OpenAI provider's plugin name.
const ProviderOpenAI = "openai"

// DefaultOpenAIBaseURL is the production Chat Completions API origin.
const DefaultOpenAIBaseURL = "https://api.openai.com"

// openaiMaxResponseBytes caps how much of a response body is read — a
// malfunctioning endpoint must not balloon worker memory. Shared reason
// (and value) as the Anthropic cap.
const openaiMaxResponseBytes = 32 << 20

// OpenAIConfig configures the OpenAI provider. The API key comes from
// config/env only (never code or fixtures — CLAUDE.md invariant).
type OpenAIConfig struct {
	// APIKey is the OpenAI API key. Required.
	APIKey string
	// BaseURL overrides the API origin; empty means production. Tests
	// point it at an httptest server.
	BaseURL string
	// HTTPClient overrides the HTTP client; nil means a default client
	// with a backstop timeout.
	HTTPClient *http.Client
}

// OpenAI is the OpenAI Chat Completions implementation of Provider
// (non-streaming v1). One HTTP call per Chat; no internal retries; no
// logging. The newer Responses API is deliberately deferred to the
// backlog: Chat Completions is the stable shape and carries tools and
// usage in the same non-streaming form this milestone needs.
type OpenAI struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewOpenAI builds the provider, rejecting a missing key at construction
// so a mis-wired deployment fails at boot, not on the first llm step.
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("llm: openai: API key is required")
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultOpenAIBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &OpenAI{apiKey: cfg.APIKey, baseURL: baseURL, client: client}, nil
}

// Manifest implements Provider (ADR-009). Cacheable and cost-bearing for
// the same reasons as the Anthropic provider: reusing an expensive
// non-deterministic completion is what M9's response cache exists for,
// and provider tokens are what M10 meters.
func (o *OpenAI) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindModelProvider,
		Name:         ProviderOpenAI,
		Version:      "1.0.0",
		Description:  "OpenAI Chat Completions API (non-streaming).",
		Capabilities: plugin.Capabilities{Cacheable: true, CostBearing: true},
	}
}

// The Chat Completions wire shapes. The message model differs from the
// unified (Anthropic) one in two ways handled by the encoder below: the
// system prompt is a leading message, and a user turn's tool_result
// blocks become separate `tool`-role messages.
type openaiRequest struct {
	Model string `json:"model"`
	// MaxCompletionTokens is the current field name; the older max_tokens
	// is deprecated and rejected by o-series reasoning models.
	MaxCompletionTokens int                   `json:"max_completion_tokens"`
	Messages            []openaiMessage       `json:"messages"`
	Tools               []openaiTool          `json:"tools,omitempty"`
	Temperature         *float64              `json:"temperature,omitempty"`
	ResponseFormat      *openaiResponseFormat `json:"response_format,omitempty"`
}

// openaiResponseFormat is the Chat Completions structured-output request
// (ticket 11.3): type json_object for any JSON, or json_schema with a named,
// strict schema for a specific shape.
type openaiResponseFormat struct {
	Type       string                `json:"type"`
	JSONSchema *openaiJSONSchemaSpec `json:"json_schema,omitempty"`
}

type openaiJSONSchemaSpec struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type openaiMessage struct {
	Role string `json:"role"`
	// Content is the text of the turn. A pointer so an assistant message
	// that is pure tool_calls can omit it (OpenAI allows null content
	// there) while an explicit empty string stays representable.
	Content   *string          `json:"content,omitempty"`
	ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set only on a `tool`-role message answering a call.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiFunctionCall `json:"function"`
}

type openaiFunctionCall struct {
	Name string `json:"name"`
	// Arguments is a JSON-encoded string (OpenAI's shape), not an object.
	Arguments string `json:"arguments"`
}

type openaiTool struct {
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openaiResponse struct {
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage"`
}

type openaiChoice struct {
	FinishReason string                `json:"finish_reason"`
	Message      openaiResponseMessage `json:"message"`
}

type openaiResponseMessage struct {
	Content   *string          `json:"content"`
	Refusal   *string          `json:"refusal"`
	ToolCalls []openaiToolCall `json:"tool_calls"`
}

type openaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type openaiErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Chat implements Provider against POST /v1/chat/completions.
func (o *OpenAI) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := req.Validate(); err != nil {
		return ChatResponse{}, &Error{
			Provider: ProviderOpenAI,
			Class:    dag.ClassPermanent,
			Code:     "client_invalid_request",
			Message:  err.Error(),
		}
	}

	wireReq, err := encodeOpenAIRequest(req)
	if err != nil {
		return ChatResponse{}, &Error{
			Provider: ProviderOpenAI,
			Class:    dag.ClassPermanent,
			Code:     "client_invalid_request",
			Message:  err.Error(),
		}
	}
	body, err := json.Marshal(wireReq)
	if err != nil {
		// Reachable only through an unmarshalable RawMessage that passed
		// validation; deterministic either way.
		return ChatResponse{}, &Error{
			Provider: ProviderOpenAI,
			Class:    dag.ClassPermanent,
			Code:     "client_invalid_request",
			Message:  "encoding request",
			cause:    err,
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, &Error{
			Provider: ProviderOpenAI,
			Class:    dag.ClassPermanent,
			Code:     "client_invalid_request",
			Message:  "building request",
			cause:    err,
		}
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		// Context errors pass through unclassified for the engine's
		// timeout/cancel judgment; the key travels in a header, so the
		// url.Error (method + URL + cause) cannot carry it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ChatResponse{}, fmt.Errorf("llm: openai: request aborted: %w", err)
		}
		return ChatResponse{}, &Error{
			Provider: ProviderOpenAI,
			Class:    dag.ClassTransient,
			Message:  "transport failure",
			cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing to do with the error

	raw, err := io.ReadAll(io.LimitReader(resp.Body, openaiMaxResponseBytes))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ChatResponse{}, fmt.Errorf("llm: openai: response aborted: %w", err)
		}
		return ChatResponse{}, &Error{
			Provider: ProviderOpenAI,
			Class:    dag.ClassTransient,
			Status:   resp.StatusCode,
			Message:  "reading response body",
			cause:    err,
		}
	}

	requestID := resp.Header.Get("x-request-id")

	if resp.StatusCode != http.StatusOK {
		perr := &Error{
			Provider:   ProviderOpenAI,
			Class:      classifyStatus(resp.StatusCode),
			Status:     resp.StatusCode,
			RetryAfter: openaiRetryAfter(resp.Header),
			RequestID:  requestID,
		}
		var envelope openaiErrorEnvelope
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Message != "" {
			// OpenAI's specific `code` (e.g. context_length_exceeded) is
			// the useful one; fall back to the coarse `type`.
			perr.Code = envelope.Error.Code
			if perr.Code == "" {
				perr.Code = envelope.Error.Type
			}
			perr.Message = envelope.Error.Message
		} else {
			perr.Message = "undecodable error body"
		}
		return ChatResponse{}, perr
	}

	var wire openaiResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return ChatResponse{}, &Error{
			Provider:  ProviderOpenAI,
			Class:     dag.ClassTransient,
			Status:    resp.StatusCode,
			RequestID: requestID,
			Message:   "undecodable success body",
			cause:     err,
		}
	}
	out, err := decodeOpenAIResponse(wire, req.ResponseFormat != nil)
	if err != nil {
		return ChatResponse{}, &Error{
			Provider:  ProviderOpenAI,
			Class:     dag.ClassTransient,
			Status:    resp.StatusCode,
			RequestID: requestID,
			Message:   "malformed success body",
			cause:     err,
		}
	}
	return out, nil
}

// openaiRetryAfter reads OpenAI's retry hint, preferring the
// millisecond-precision retry-after-ms header over the whole-second
// Retry-After. Both parse clock-free (integer counts only).
func openaiRetryAfter(h http.Header) time.Duration {
	if ms, ok := parseRetryAfterMillis(h.Get("retry-after-ms")); ok {
		return ms
	}
	return parseRetryAfter(h.Get("retry-after"))
}

// encodeOpenAIRequest maps the unified request onto the Chat Completions
// wire shape. Shapes were validated already; the two structural
// mismatches with the unified (Anthropic) model are handled here:
//   - the system prompt becomes a leading `system` message;
//   - a user turn carrying tool_result blocks fans out into one
//     `tool`-role message per result (OpenAI has no per-block roles).
func encodeOpenAIRequest(req ChatRequest) (openaiRequest, error) {
	out := openaiRequest{
		Model:               req.Model,
		MaxCompletionTokens: req.MaxTokens,
		Temperature:         req.Temperature,
		Messages:            make([]openaiMessage, 0, len(req.Messages)+1),
	}
	if req.System != "" {
		sys := req.System
		out.Messages = append(out.Messages, openaiMessage{Role: "system", Content: &sys})
	}
	for i, m := range req.Messages {
		msgs, err := encodeOpenAIMessage(m)
		if err != nil {
			return openaiRequest{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}
	for _, td := range req.Tools {
		out.Tools = append(out.Tools, openaiTool{
			Type: "function",
			Function: openaiToolFunction{
				Name:        td.Name,
				Description: td.Description,
				Parameters:  td.InputSchema,
			},
		})
	}
	// Native structured output (ticket 11.3): json_object for any JSON, or a
	// named strict json_schema for a specific shape. The executor's implicit
	// validator enforces the schema regardless; response_format reduces
	// failures at the source.
	if req.ResponseFormat != nil {
		if req.ResponseFormat.HasSchema() {
			name := req.ResponseFormat.Name
			if name == "" {
				name = "structured_output"
			}
			out.ResponseFormat = &openaiResponseFormat{
				Type:       "json_schema",
				JSONSchema: &openaiJSONSchemaSpec{Name: name, Schema: req.ResponseFormat.Schema, Strict: true},
			}
		} else {
			out.ResponseFormat = &openaiResponseFormat{Type: "json_object"}
		}
	}
	return out, nil
}

// encodeOpenAIMessage maps one unified message onto one or more wire
// messages. Text and tool_use blocks collapse into a single message
// (content + tool_calls); each tool_result block becomes its own
// `tool`-role message, emitted after the collapsed one.
func encodeOpenAIMessage(m Message) ([]openaiMessage, error) {
	var (
		text        string
		hasText     bool
		toolCalls   []openaiToolCall
		toolResults []openaiMessage
	)
	for _, b := range m.Blocks {
		switch b.Type {
		case BlockText:
			text += b.Text
			hasText = true
		case BlockToolUse:
			args := string(b.ToolUse.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, openaiToolCall{
				ID:       b.ToolUse.ID,
				Type:     "function",
				Function: openaiFunctionCall{Name: b.ToolUse.Name, Arguments: args},
			})
		case BlockToolResult:
			// A tool result is the model's answer to a prior call: OpenAI
			// carries it as a standalone `tool`-role message, regardless of
			// the unified message's role.
			content := b.ToolResult.Content
			toolResults = append(toolResults, openaiMessage{
				Role:       "tool",
				Content:    &content,
				ToolCallID: b.ToolResult.ToolUseID,
			})
		default:
			return nil, fmt.Errorf("unsupported block type %q", b.Type)
		}
	}

	var out []openaiMessage
	// Emit the collapsed message only when it carries something: a turn
	// made up solely of tool_results has no primary message.
	if hasText || len(toolCalls) > 0 {
		msg := openaiMessage{Role: string(m.Role), ToolCalls: toolCalls}
		if hasText {
			msg.Content = &text
		}
		out = append(out, msg)
	}
	out = append(out, toolResults...)
	return out, nil
}

// decodeOpenAIResponse maps the wire response onto the unified shape,
// enforcing the 8.3 contract that usage is present on every success and
// normalizing the finish reason onto the Anthropic stop-reason
// vocabulary the unified type documents. structured reports whether the
// request carried a response_format, in which case a valid-JSON content
// string is lifted onto ChatResponse.Structured (ticket 11.3).
func decodeOpenAIResponse(wire openaiResponse, structured bool) (ChatResponse, error) {
	if wire.Usage == nil {
		return ChatResponse{}, errors.New("response carries no usage")
	}
	if len(wire.Choices) == 0 {
		return ChatResponse{}, errors.New("response carries no choices")
	}
	choice := wire.Choices[0]
	out := ChatResponse{
		Model:      wire.Model,
		StopReason: normalizeFinishReason(choice.FinishReason),
		Usage:      Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens},
	}
	// A refusal IS the completion — surface it as text rather than
	// dropping it and returning an empty response.
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		out.Blocks = append(out.Blocks, TextBlock(*choice.Message.Refusal))
	}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		out.Blocks = append(out.Blocks, TextBlock(*choice.Message.Content))
		// Native structured output (ticket 11.3): under a response_format
		// request, a content string that is already valid JSON is the
		// structured answer. A non-JSON content (e.g. a refusal folded into
		// content) leaves Structured nil for the executor's repair pass.
		if structured && json.Valid([]byte(*choice.Message.Content)) {
			out.Structured = json.RawMessage(*choice.Message.Content)
		}
	}
	for i, tc := range choice.Message.ToolCalls {
		if tc.Type != "" && tc.Type != "function" {
			// A tool-call type this build cannot represent; dropping it
			// silently would corrupt the completion.
			return ChatResponse{}, fmt.Errorf("tool_calls[%d]: unsupported type %q", i, tc.Type)
		}
		input := json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		if !json.Valid(input) {
			return ChatResponse{}, fmt.Errorf("tool_calls[%d]: arguments are not valid JSON", i)
		}
		out.Blocks = append(out.Blocks, Block{
			Type:    BlockToolUse,
			ToolUse: &ToolUse{ID: tc.ID, Name: tc.Function.Name, Input: input},
		})
	}
	return out, nil
}

// normalizeFinishReason maps an OpenAI finish_reason onto the Anthropic
// stop-reason vocabulary the unified ChatResponse documents, so the 8.6
// executor branches on one vocabulary. Unknown reasons (e.g.
// content_filter) pass through verbatim — additive, never an error.
func normalizeFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return reason
	}
}
