package contextmgr

// Summarization compaction (ADR-014, ticket 12.5). The summarize strategy is
// the one non-deterministic compaction strategy: it replaces the oldest evicted
// non-pinned span with a cheap-model summary, written to the run blackboard so
// other steps can read it and so successive summarizations chain through the
// key's version history. Determinism is restored operationally — the summary
// prompt is fixed and issued at temperature 0, and the engine caches the call
// (ADR-011) — and the audit records the exact blackboard key@version the
// summary landed at, so the pre-execution decision stays reconstructable.
//
// A summarizer failure NEVER blocks the step: Compact falls back to the next
// deterministic strategy in the pipeline and records the failure on the
// strategy's context_revision (the warning event). The one exception is a
// caller-context cancellation, which passes through unwrapped so the engine
// keeps its timeout/cancelled judgment.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// SummarizerVersion is the summarizer plugin's behavioral version — a component
// of the ADR-011 cache key the engine builds for a summarizer call, so a change
// to the summary prompt or framing invalidates cached summaries.
const SummarizerVersion = "1.0.0"

// summarySystemPrompt is the fixed system instruction for every summarizer call
// — part of what keeps a temperature-0 summarizer call deterministic and
// cacheable. It never embeds the span, so the same span produces the same
// request bytes.
const summarySystemPrompt = "You are a summarization assistant. Produce a faithful, concise summary of the material the user provides. Preserve concrete decisions, facts, names, and numbers. Do not invent information that is not present. Output only the summary text."

// summaryInstruction is the fixed trailing user turn asking for the summary. It
// follows the span material so the span is the model's primary input; on the
// deterministic mock provider (which echoes the last user turn) this short
// instruction becomes the "summary", so the canonical offline fixture shrinks
// without scripting.
const summaryInstruction = "Summarize the material above into a concise digest that preserves the key decisions and facts."

// SummaryValueSchemaVersion versions the JSON value a summarize strategy writes
// to the blackboard.
const SummaryValueSchemaVersion = 1

// kindSummary is the synthetic source kind of a summary entry in the assembly.
// It is not an authorable context source kind (Validate rejects it); it exists
// only so a summary renders as a labeled <context kind="summary"> block and is
// distinguishable in the audit.
const kindSummary = "summary"

// SummaryValue is the JSON value a summarize strategy writes to the blackboard
// (ticket 12.5): the summary text plus the provenance that makes the chained
// summary auditable — which named span it replaced, the tokens before/after,
// the model and resource, whether it was cache-served, and the prior version it
// chained from (0 for the first summary under the key). The key's version
// history is the chain.
type SummaryValue struct {
	SchemaVersion int      `json:"schema_version"`
	Text          string   `json:"text"`
	SpanNames     []string `json:"span_names"`
	SpanTokens    int      `json:"span_tokens"`
	SummaryTokens int      `json:"summary_tokens"`
	Model         string   `json:"model"`
	Resource      string   `json:"resource"`
	CacheHit      bool     `json:"cache_hit"`
	ParentVersion int      `json:"parent_version,omitempty"`
}

// SummaryAction records one summarization for the context_revision audit and
// the engine's overhead pricing (ticket 12.5): the blackboard key@version the
// summary landed at, the model/resource billed, the span it folded, and the
// provider usage the engine ledgers as compaction overhead.
type SummaryAction struct {
	Key           string   `json:"key"`
	Version       int      `json:"version"`
	ParentVersion int      `json:"parent_version,omitempty"`
	Model         string   `json:"model"`
	Resource      string   `json:"resource"`
	SpanNames     []string `json:"span_names"`
	SpanTokens    int      `json:"span_tokens"`
	SummaryTokens int      `json:"summary_tokens"`
	CacheHit      bool     `json:"cache_hit"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
}

// SummarizeRequest is the input to a Summarizer: the model to call, the
// rendered span to summarize, the summary completion bound, and the per-call
// deadline (0 = none).
type SummarizeRequest struct {
	Model     string
	SpanText  string
	MaxTokens int
	Timeout   time.Duration
}

// SummaryUsage is a summarizer call's token accounting (a copy of llm.Usage so
// callers that only handle summaries need not import internal/llm).
type SummaryUsage struct {
	InputTokens  int64
	OutputTokens int64
}

// SummaryResult is a Summarizer's product: the summary text, the resolved
// pricing resource (`<provider>:<model>`), the provider token usage, and
// whether it was served from cache (set by a caching decorator; false from a
// bare summarizer).
type SummaryResult struct {
	Text     string
	Resource string
	Usage    *SummaryUsage
	CacheHit bool
}

// Summarizer summarizes an evicted context span into shorter text (ticket
// 12.5). The engine supplies the implementation (LLMSummarizer, optionally
// wrapped in a response-cache decorator). A summarizer error is never a step
// failure — Compact falls back to the next deterministic strategy — except a
// caller-context cancellation, which the implementation returns unwrapped so
// the engine keeps the timeout/cancelled judgment.
type Summarizer interface {
	Summarize(ctx context.Context, req SummarizeRequest) (SummaryResult, error)
}

// SummaryChatRequest builds the deterministic summarizer request for a span:
// the fixed system prompt, the span material as a leading user turn, the fixed
// summary instruction as the trailing user turn, temperature 0, and the summary
// completion bound. Pure and deterministic — the same (model, span, maxTokens)
// yields byte-identical bytes, so the engine's cache key over it is stable.
func SummaryChatRequest(model, spanText string, maxTokens int) llm.ChatRequest {
	zero := 0.0
	return llm.ChatRequest{
		Model:  model,
		System: summarySystemPrompt,
		Messages: []llm.Message{
			llm.UserText(spanText),
			llm.UserText(summaryInstruction),
		},
		MaxTokens:   maxTokens,
		Temperature: &zero,
	}
}

// LLMSummarizer is the production Summarizer: it routes the strategy's model
// through the llm registry and makes exactly one Chat call per summarization
// (retries and caching are the engine's concern). A resolution failure, a
// provider error of any class, an empty summary, or a per-call timeout is an
// ordinary error the caller falls back on; a caller-context cancellation passes
// through unwrapped.
type LLMSummarizer struct {
	providers *llm.Registry
}

// NewLLMSummarizer builds an LLMSummarizer over the model registry. A nil
// registry makes every Summarize a (fallback-able) error.
func NewLLMSummarizer(providers *llm.Registry) *LLMSummarizer {
	return &LLMSummarizer{providers: providers}
}

// Summarize resolves req.Model and makes one temperature-0 Chat call over the
// span under an optional per-call timeout. On success it returns the summary
// text, the resolved pricing resource, and the provider usage. A per-call
// timeout (the strategy's deadline) is an ordinary error (fall back); a
// parent-context cancellation passes through unwrapped.
func (s *LLMSummarizer) Summarize(ctx context.Context, req SummarizeRequest) (SummaryResult, error) {
	if s.providers == nil {
		return SummaryResult{}, fmt.Errorf("no model providers configured for summarizer model %q", req.Model)
	}
	provider, model, err := s.providers.Resolve("", req.Model)
	if err != nil {
		return SummaryResult{}, fmt.Errorf("resolving summarizer model %q: %w", req.Model, err)
	}
	callCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	resp, err := provider.Chat(callCtx, SummaryChatRequest(model, req.SpanText, req.MaxTokens))
	if err != nil {
		// A parent-context cancellation is the engine's timeout/cancelled
		// judgment — pass it through unwrapped rather than swallowing it as a
		// summarizer failure. A per-call timeout (callCtx expired but ctx live)
		// is an ordinary summarizer failure the caller falls back on.
		if ctx.Err() != nil {
			return SummaryResult{}, ctx.Err()
		}
		return SummaryResult{}, fmt.Errorf("summarizer call: %w", err)
	}
	text := responseText(resp)
	if strings.TrimSpace(text) == "" {
		return SummaryResult{}, fmt.Errorf("summarizer returned an empty summary")
	}
	return SummaryResult{
		Text:     text,
		Resource: provider.Manifest().Name + ":" + model,
		Usage:    &SummaryUsage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
	}, nil
}

// responseText concatenates a chat response's text blocks (a native structured
// payload is not summary text).
func responseText(resp llm.ChatResponse) string {
	var b strings.Builder
	for _, blk := range resp.Blocks {
		if blk.Type == llm.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
