// Package llm defines the unified model-provider interface (ticket 8.3,
// ADR-009 kind `model_provider`): one ChatRequest/ChatResponse contract
// that every provider implementation maps onto its own wire format, so
// the llm step executor (8.6) and the provider registry (8.4) speak one
// shape regardless of vendor.
//
// The package is deliberately a leaf like internal/ratelimit: it imports
// internal/dag for the ADR-006 error-class vocabulary and internal/plugin
// for the manifest, never internal/exec or the engine. Providers perform
// exactly one call per Chat invocation — no internal retries (retry
// policy is the M5 engine's job, informed by the classes providers put
// on their errors) — and never log (the caller owns observability).
//
// v1 is non-streaming (streaming is backlogged); usage extraction is
// mandatory on every success (M10's cost ledger meters it).
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// Provider is the model-provider SPI: one non-streaming chat completion
// per call. Implementations map the unified request onto their wire
// format, extract usage on every success, and classify every failure
// onto the ADR-006 vocabulary via *Error — except context cancellation
// and deadline expiry, which pass through unclassified (wrapped, never
// as *Error) because the engine judges timeout vs. cancelled from
// context state itself (ADR-006 rows 3/8).
type Provider interface {
	// Chat performs one chat completion. On success the response always
	// carries Usage; on failure the error is either a classified *Error
	// or a pass-through context error.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// Manifest is the provider's ADR-009 self-description
	// (kind model_provider). The 8.4 registry validates and serves it.
	Manifest() plugin.Manifest
}

// Role is a chat message's author role.
type Role string

// The two conversational roles. The system prompt is not a message —
// it rides on ChatRequest.System, matching the Anthropic shape (8.4
// folds it into a system message for providers that want one).
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleUser || r == RoleAssistant }

// BlockType discriminates a content Block.
type BlockType string

// The three block types of the unified content model — the Anthropic
// shapes, which OpenAI's message/tool_calls model maps onto in 8.4.
const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// Block is one content block of a message. Exactly the field matching
// Type is set: Text for text, ToolUse for tool_use, ToolResult for
// tool_result; Validate enforces the discrimination.
type Block struct {
	Type       BlockType
	Text       string
	ToolUse    *ToolUse
	ToolResult *ToolResult
}

// TextBlock builds a text Block.
func TextBlock(text string) Block { return Block{Type: BlockText, Text: text} }

// ToolUse is a model-issued tool call: the model asks the caller to run
// tool Name with Input and report back via a ToolResult carrying ID.
type ToolUse struct {
	// ID correlates the eventual ToolResult with this call.
	ID string
	// Name is the tool's name, matching a ToolDef.Name from the request.
	Name string
	// Input is the model-produced arguments object, verbatim JSON.
	Input json.RawMessage
}

// ToolResult reports a tool call's outcome back to the model in a
// subsequent user message.
type ToolResult struct {
	// ToolUseID is the ID of the ToolUse this result answers.
	ToolUseID string
	// Content is the result rendered as text.
	Content string
	// IsError marks the result as a tool-side failure the model should
	// react to (not a provider error).
	IsError bool
}

// Message is one conversational turn: a role and its content blocks.
type Message struct {
	Role   Role
	Blocks []Block
}

// UserText builds a single-text-block user message, the common case.
func UserText(text string) Message {
	return Message{Role: RoleUser, Blocks: []Block{TextBlock(text)}}
}

// AssistantText builds a single-text-block assistant message.
func AssistantText(text string) Message {
	return Message{Role: RoleAssistant, Blocks: []Block{TextBlock(text)}}
}

// ToolDef declares one tool the model may call.
type ToolDef struct {
	// Name identifies the tool.
	Name string
	// Description tells the model when to use it.
	Description string
	// InputSchema is the JSON Schema object for the tool's arguments,
	// embedded verbatim in the provider request. Required.
	InputSchema json.RawMessage
}

// ChatRequest is the unified chat-completion request.
type ChatRequest struct {
	// Model is the provider's model identifier. Required.
	Model string
	// System is the optional system prompt.
	System string
	// Messages is the conversation, oldest first. Required, non-empty.
	Messages []Message
	// Tools are the tool definitions offered to the model, if any.
	Tools []ToolDef
	// MaxTokens bounds the completion length. Required, positive
	// (Anthropic requires it; a mandatory bound also keeps M10 budgets
	// estimable before the call).
	MaxTokens int
	// Temperature is the sampling temperature; nil leaves the provider
	// default. Range checking is the provider API's (ranges differ per
	// vendor; an out-of-range value comes back as a permanent error).
	Temperature *float64
	// ResponseFormat requests provider-native structured output (ticket
	// 11.3, ADR-013): nil leaves the completion as free text; non-nil asks
	// the provider for a JSON (or schema-conforming JSON) answer through its
	// native mechanism (Anthropic forced tool-use, OpenAI response_format).
	// A provider that honors it sets ChatResponse.Structured; the llm
	// executor's deterministic JSON-repair pass is the fallback when it does
	// not.
	ResponseFormat *ResponseFormat
}

// ResponseFormat is a request for provider-native structured output (ticket
// 11.3). Schema nil requests a plain JSON object (no shape constraint);
// Schema non-nil requests JSON matching that JSON Schema. Name labels the
// schema for providers that require one (OpenAI's json_schema mode); the
// executor supplies a stable default.
type ResponseFormat struct {
	// Schema is the JSON Schema (2020-12) the output should match; nil
	// requests a plain JSON object.
	Schema json.RawMessage
	// Name is the schema label some providers require. Empty is tolerated
	// (providers substitute a default).
	Name string
}

// HasSchema reports whether the format constrains the output to a specific
// schema (json_schema mode) rather than any JSON object (json mode).
func (f *ResponseFormat) HasSchema() bool {
	return f != nil && len(f.Schema) > 0
}

// Validate checks the request's shape client-side: the rules every
// provider shares, caught before spending a network round-trip.
// Providers wrap the returned error as a permanent *Error — an invalid
// request fails identically on every retry.
func (r ChatRequest) Validate() error {
	if r.Model == "" {
		return errors.New("model is required")
	}
	if r.MaxTokens < 1 {
		return fmt.Errorf("max_tokens must be positive, got %d", r.MaxTokens)
	}
	if len(r.Messages) == 0 {
		return errors.New("messages must be non-empty")
	}
	for i, m := range r.Messages {
		if !m.Role.Valid() {
			return fmt.Errorf("messages[%d]: role %q is not %q or %q", i, m.Role, RoleUser, RoleAssistant)
		}
		if len(m.Blocks) == 0 {
			return fmt.Errorf("messages[%d]: blocks must be non-empty", i)
		}
		for j, b := range m.Blocks {
			if err := b.validate(); err != nil {
				return fmt.Errorf("messages[%d].blocks[%d]: %w", i, j, err)
			}
		}
	}
	for i, td := range r.Tools {
		if td.Name == "" {
			return fmt.Errorf("tools[%d]: name is required", i)
		}
		if !isJSONObject(td.InputSchema) {
			return fmt.Errorf("tools[%d] (%s): input schema must be a JSON object", i, td.Name)
		}
	}
	return nil
}

// validate checks one block's discrimination: the field matching Type
// set and well-formed, the others nil.
func (b Block) validate() error {
	switch b.Type {
	case BlockText:
		if b.ToolUse != nil || b.ToolResult != nil {
			return errors.New("text block carries non-text fields")
		}
		if b.Text == "" {
			return errors.New("text block is empty")
		}
	case BlockToolUse:
		if b.Text != "" || b.ToolResult != nil {
			return errors.New("tool_use block carries non-tool_use fields")
		}
		if b.ToolUse == nil {
			return errors.New("tool_use block has no tool use")
		}
		if b.ToolUse.ID == "" || b.ToolUse.Name == "" {
			return errors.New("tool use requires id and name")
		}
		if len(b.ToolUse.Input) > 0 && !json.Valid(b.ToolUse.Input) {
			return errors.New("tool use input is not valid JSON")
		}
	case BlockToolResult:
		if b.Text != "" || b.ToolUse != nil {
			return errors.New("tool_result block carries non-tool_result fields")
		}
		if b.ToolResult == nil {
			return errors.New("tool_result block has no tool result")
		}
		if b.ToolResult.ToolUseID == "" {
			return errors.New("tool result requires tool_use_id")
		}
	default:
		return fmt.Errorf("unknown block type %q", b.Type)
	}
	return nil
}

// isJSONObject reports whether raw is a JSON object (the shape provider
// APIs require for schemas and tool inputs).
func isJSONObject(raw json.RawMessage) bool {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return json.Valid(raw)
		default:
			return false
		}
	}
	return false
}

// Usage is the token accounting of one successful call, extracted on
// every success (M10's cost ledger meters it). Provider-side cache
// token counts are a deliberate omission until M10 needs them.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// ChatResponse is the unified chat-completion response.
type ChatResponse struct {
	// Model is the model that actually served the call, as reported by
	// the provider.
	Model string
	// StopReason is the provider's stop reason, passed through verbatim
	// (Anthropic vocabulary: end_turn, max_tokens, tool_use,
	// stop_sequence; 8.4 maps OpenAI finish reasons onto it).
	StopReason string
	// Blocks is the completion's content.
	Blocks []Block
	// Structured is the provider's native structured-output payload (ticket
	// 11.3), set only when the request carried a ResponseFormat the provider
	// honored (Anthropic forced tool-use JSON, OpenAI response_format). It is
	// the verbatim JSON the model produced; nil when the provider answered in
	// free text (the executor then repairs Blocks' text instead). It is not
	// included in Blocks, so it never appears as a user-visible tool call.
	Structured json.RawMessage
	// Usage is the call's token accounting. Always set on success.
	Usage Usage
}

// Text concatenates the response's text blocks — the completion as
// plain text.
func (r ChatResponse) Text() string {
	var out string
	for _, b := range r.Blocks {
		if b.Type == BlockText {
			out += b.Text
		}
	}
	return out
}

// ToolUses collects the response's tool calls in order.
func (r ChatResponse) ToolUses() []ToolUse {
	var out []ToolUse
	for _, b := range r.Blocks {
		if b.Type == BlockToolUse && b.ToolUse != nil {
			out = append(out, *b.ToolUse)
		}
	}
	return out
}
