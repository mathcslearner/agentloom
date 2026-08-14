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

// ProviderAnthropic is the Anthropic provider's plugin name.
const ProviderAnthropic = "anthropic"

// DefaultAnthropicBaseURL is the production Messages API origin.
const DefaultAnthropicBaseURL = "https://api.anthropic.com"

// anthropicAPIVersion is the pinned Messages API version header. Bumping
// it is a behavior change: it must ride a provider version bump (the
// manifest version feeds M9 cache keys).
const anthropicAPIVersion = "2023-06-01"

// anthropicMaxResponseBytes caps how much of a response body is read —
// a malfunctioning endpoint must not balloon worker memory. Generous:
// the largest legitimate completion is well under this.
const anthropicMaxResponseBytes = 32 << 20

// defaultHTTPTimeout bounds a call when the caller's context carries no
// deadline of its own. LLM completions are legitimately slow, so this
// is a backstop against a hung connection, not a step timeout — that is
// M5.3's, delivered through ctx.
const defaultHTTPTimeout = 10 * time.Minute

// AnthropicConfig configures the Anthropic provider. The API key comes
// from config/env only (never code or fixtures — CLAUDE.md invariant).
type AnthropicConfig struct {
	// APIKey is the Anthropic API key. Required.
	APIKey string
	// BaseURL overrides the API origin; empty means production. Tests
	// point it at an httptest server.
	BaseURL string
	// HTTPClient overrides the HTTP client; nil means a default client
	// with a backstop timeout.
	HTTPClient *http.Client
}

// Anthropic is the Anthropic Messages API implementation of Provider
// (non-streaming v1). One HTTP call per Chat; no internal retries; no
// logging.
type Anthropic struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewAnthropic builds the provider, rejecting a missing key at
// construction so a mis-wired deployment fails at boot, not on the
// first llm step.
func NewAnthropic(cfg AnthropicConfig) (*Anthropic, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("llm: anthropic: API key is required")
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultAnthropicBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Anthropic{apiKey: cfg.APIKey, baseURL: baseURL, client: client}, nil
}

// Manifest implements Provider (ADR-009). Cacheable and cost-bearing
// for the same reasons as the llm step executor's flags: reusing an
// expensive non-deterministic completion is exactly what M9's response
// cache exists for, and provider tokens are what M10 meters.
func (a *Anthropic) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindModelProvider,
		Name:         ProviderAnthropic,
		Version:      "1.0.0",
		Description:  "Anthropic Messages API (non-streaming).",
		Capabilities: plugin.Capabilities{Cacheable: true, CostBearing: true},
	}
}

// The Messages API wire shapes. Request and response reuse one block
// shape: the field sets are disjoint per type, and omitempty keeps each
// encoded block minimal.
type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use fields.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields.
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicResponse struct {
	Model      string           `json:"model"`
	StopReason string           `json:"stop_reason"`
	Content    []anthropicBlock `json:"content"`
	Usage      *anthropicUsage  `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type anthropicErrorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Chat implements Provider against POST /v1/messages.
func (a *Anthropic) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := req.Validate(); err != nil {
		return ChatResponse{}, &Error{
			Provider: ProviderAnthropic,
			Class:    dag.ClassPermanent,
			Code:     "client_invalid_request",
			Message:  err.Error(),
		}
	}

	body, err := json.Marshal(encodeAnthropicRequest(req))
	if err != nil {
		// Reachable only through an unmarshalable RawMessage that passed
		// validation; deterministic either way.
		return ChatResponse{}, &Error{
			Provider: ProviderAnthropic,
			Class:    dag.ClassPermanent,
			Code:     "client_invalid_request",
			Message:  "encoding request",
			cause:    err,
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, &Error{
			Provider: ProviderAnthropic,
			Class:    dag.ClassPermanent,
			Code:     "client_invalid_request",
			Message:  "building request",
			cause:    err,
		}
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		// Context errors pass through unclassified for the engine's
		// timeout/cancel judgment; the key travels in a header, so the
		// url.Error (method + URL + cause) cannot carry it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ChatResponse{}, fmt.Errorf("llm: anthropic: request aborted: %w", err)
		}
		return ChatResponse{}, &Error{
			Provider: ProviderAnthropic,
			Class:    dag.ClassTransient,
			Message:  "transport failure",
			cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing to do with the error

	raw, err := io.ReadAll(io.LimitReader(resp.Body, anthropicMaxResponseBytes))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ChatResponse{}, fmt.Errorf("llm: anthropic: response aborted: %w", err)
		}
		return ChatResponse{}, &Error{
			Provider: ProviderAnthropic,
			Class:    dag.ClassTransient,
			Status:   resp.StatusCode,
			Message:  "reading response body",
			cause:    err,
		}
	}

	requestID := resp.Header.Get("request-id")

	if resp.StatusCode != http.StatusOK {
		perr := &Error{
			Provider:   ProviderAnthropic,
			Class:      classifyStatus(resp.StatusCode),
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("retry-after")),
			RequestID:  requestID,
		}
		var envelope anthropicErrorEnvelope
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Type != "" {
			perr.Code = envelope.Error.Type
			perr.Message = envelope.Error.Message
		} else {
			perr.Message = "undecodable error body"
		}
		return ChatResponse{}, perr
	}

	var wire anthropicResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return ChatResponse{}, &Error{
			Provider:  ProviderAnthropic,
			Class:     dag.ClassTransient,
			Status:    resp.StatusCode,
			RequestID: requestID,
			Message:   "undecodable success body",
			cause:     err,
		}
	}
	out, err := decodeAnthropicResponse(wire)
	if err != nil {
		return ChatResponse{}, &Error{
			Provider:  ProviderAnthropic,
			Class:     dag.ClassTransient,
			Status:    resp.StatusCode,
			RequestID: requestID,
			Message:   "malformed success body",
			cause:     err,
		}
	}
	return out, nil
}

// encodeAnthropicRequest maps the unified request onto the wire shape.
// Shapes were validated already; this is pure translation.
func encodeAnthropicRequest(req ChatRequest) anthropicRequest {
	out := anthropicRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		System:      req.System,
		Messages:    make([]anthropicMessage, 0, len(req.Messages)),
		Temperature: req.Temperature,
	}
	for _, m := range req.Messages {
		wm := anthropicMessage{Role: string(m.Role), Content: make([]anthropicBlock, 0, len(m.Blocks))}
		for _, b := range m.Blocks {
			wm.Content = append(wm.Content, encodeAnthropicBlock(b))
		}
		out.Messages = append(out.Messages, wm)
	}
	for _, td := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool(td))
	}
	return out
}

func encodeAnthropicBlock(b Block) anthropicBlock {
	switch b.Type {
	case BlockToolUse:
		input := b.ToolUse.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return anthropicBlock{Type: string(BlockToolUse), ID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: input}
	case BlockToolResult:
		return anthropicBlock{
			Type:      string(BlockToolResult),
			ToolUseID: b.ToolResult.ToolUseID,
			Content:   b.ToolResult.Content,
			IsError:   b.ToolResult.IsError,
		}
	default:
		return anthropicBlock{Type: string(BlockText), Text: b.Text}
	}
}

// decodeAnthropicResponse maps the wire response onto the unified
// shape, enforcing the 8.3 contract that usage is present on every
// success — a 200 without it is a malformed response, not a lenient
// zero.
func decodeAnthropicResponse(wire anthropicResponse) (ChatResponse, error) {
	if wire.Usage == nil {
		return ChatResponse{}, errors.New("response carries no usage")
	}
	out := ChatResponse{
		Model:      wire.Model,
		StopReason: wire.StopReason,
		Usage:      Usage{InputTokens: wire.Usage.InputTokens, OutputTokens: wire.Usage.OutputTokens},
	}
	for i, wb := range wire.Content {
		switch wb.Type {
		case string(BlockText):
			out.Blocks = append(out.Blocks, TextBlock(wb.Text))
		case string(BlockToolUse):
			out.Blocks = append(out.Blocks, Block{
				Type:    BlockToolUse,
				ToolUse: &ToolUse{ID: wb.ID, Name: wb.Name, Input: wb.Input},
			})
		default:
			// An unknown block type means content this build cannot
			// represent (a newer API feature); dropping it silently would
			// corrupt the completion.
			return ChatResponse{}, fmt.Errorf("content[%d]: unsupported block type %q", i, wb.Type)
		}
	}
	return out, nil
}
