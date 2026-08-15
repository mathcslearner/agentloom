// Package tokens counts the tokens a model request will cost, per model
// family, before the request is sent (ticket 12.1, ADR-014). It is the
// primitive M12's context budgets (12.3), compaction (12.4/12.5), and
// provider-window guardrails (12.6) rest on: you cannot assemble-to-a-cap,
// compact-until-under, or guard a window without a trustworthy pre-flight
// token count.
//
// The package is a leaf: it imports internal/llm (for the unified
// ChatRequest/Message/Block/ToolDef shapes it counts) plus stdlib and the
// tiktoken library, and no other agentloom package — so exec/engine and the
// coming internal/contextmgr can depend on it without a cycle (the
// internal/cost, internal/cache, internal/limits precedent). Counters are
// pure functions of their input and their embedded tables: no clock, no
// network, no logging on the count path (selection logs a fallback once).
//
// A Counter is selected per resolved (provider, model) — the same identity
// ADR-010's limiter and ADR-012's catalog key on — because model families
// tokenize differently: OpenAI uses a published BPE (tiktoken, exact),
// Anthropic publishes none (a calibrated estimate over the closest public
// BPE), the mock provider tokenizes however its estimator says, and an
// unknown model falls back to chars/4. Every counter carries a stable ID()
// fingerprint so a stored token_count (12.2's blackboard column, 12.3's
// assembly totals) is valid only against the counter that produced it.
package tokens

import (
	"github.com/mathcslearner/agentloom/internal/llm"
)

// Counter counts tokens for one model family's tokenization. Implementations
// are deterministic and side-effect-free: the same input always yields the
// same count, and no method touches the clock, the network, or a logger.
type Counter interface {
	// ID is the counter's stable fingerprint, e.g. "openai/o200k_base@1",
	// "anthropic/estimate@1;factor=1.15", "mock/estimate@1",
	// "fallback/chars4@1". It changes only when the tokenization changes
	// (encoding swap, calibration re-derivation, or a framing-constant change
	// bumps the @N suffix). A stored (token_count, counter_id) pair is valid
	// iff counter_id equals the current counter's ID; on a mismatch the
	// consumer recomputes rather than trusting a stale number.
	ID() string

	// Count returns the token count of a bare string in this counter's
	// tokenization.
	Count(text string) int

	// CountRequest returns the token count of a fully-framed request: the
	// system prompt as a framed message, each message's role framing and
	// content blocks (text, tool_use input JSON, tool_result content), each
	// tool definition (name + description + input-schema JSON + a per-tool
	// constant), the structured-output schema when present, and the fixed
	// reply-priming overhead. This is the number the provider bills and the
	// context window is spent on — counting bare concatenated text would miss
	// the per-message framing and drift a multi-turn conversation past ±5%.
	CountRequest(req llm.ChatRequest) int
}

// framing holds a model family's chat-accounting constants: the per-message
// overhead (role token + message delimiters), the per-tool-definition
// overhead, and the fixed reply-priming overhead added once per request. The
// values follow the documented OpenAI chat-completion accounting generalized
// to the unified request; they are what the ±5% accuracy fixtures calibrate
// and guard, and a change to any of them bumps the counter's ID @N suffix.
type framing struct {
	perMessage int
	perTool    int
	reply      int
}

// openAIFraming is the chat-accounting for OpenAI chat models (o200k and
// cl100k families alike): +3 tokens per message for role and delimiters, +3
// once for reply priming (the "<|start|>assistant<|message|>" the model is
// primed with), and a modest per-tool-definition overhead on top of the
// tool's own name/description/schema token cost.
var openAIFraming = framing{perMessage: 3, perTool: 8, reply: 3}

// countRequest is the shared framing walk: it counts a fully-formed request
// using count for every text component and the given framing constants. Both
// the OpenAI (exact BPE) and Anthropic (calibrated BPE) counters share it,
// differing only in count and framing; the mock and fallback counters use it
// too with their own count.
func countRequest(req llm.ChatRequest, f framing, count func(string) int) int {
	total := f.reply
	if req.System != "" {
		total += f.perMessage + count(req.System)
	}
	for _, m := range req.Messages {
		total += f.perMessage
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockText:
				total += count(b.Text)
			case llm.BlockToolUse:
				if b.ToolUse != nil {
					total += count(b.ToolUse.Name) + count(string(b.ToolUse.Input))
				}
			case llm.BlockToolResult:
				if b.ToolResult != nil {
					total += count(b.ToolResult.Content)
				}
			}
		}
	}
	for _, t := range req.Tools {
		total += f.perTool + count(t.Name) + count(t.Description) + count(string(t.InputSchema))
	}
	if req.ResponseFormat != nil {
		total += count(string(req.ResponseFormat.Schema))
		if req.ResponseFormat.Name != "" {
			total += count(req.ResponseFormat.Name)
		}
	}
	return total
}
