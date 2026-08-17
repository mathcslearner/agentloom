package dag

// This file is the definition-contract half of ADR-014's context assembly:
// the optional per-step `context` block an author carries on the step
// envelope to declare the sources assembled into the provider request before
// the call, and the bounds Validate enforces on it. The runtime half — the
// assembly, per-source token caps, pinning, the missing-source policy, and
// the pre-flight token total — lives in internal/contextmgr and the engine
// (ticket 12.3); this package only defines and validates the authored field.
//
// 12.3 assembles the request from the sources; 12.4 compacts it when it
// exceeds the step's budget. Templating inside the block (`${{ ... }}`) is
// deliberately not supported — a dynamic query flows through a `retrieve`
// step and a `step_output` source, keeping this ticket out of the template
// engine (the 12.2 stance).

import "time"

// MaxContextSources bounds the sources one step may assemble.
const MaxContextSources = 32

// MaxContextTopK bounds a retrieval source's top_k, mirroring the retrieve
// executor's cap so a context retrieval and a `retrieve` step agree.
const MaxContextTopK = 100

// MaxCompactionStrategies bounds the strategy pipeline one step may declare
// (ticket 12.4). A pipeline longer than a handful is an authoring mistake.
const MaxCompactionStrategies = 8

// Summarize-strategy defaults and bounds (ticket 12.5). A summarize strategy
// replaces an evicted span with a cheap-model summary written to the
// blackboard; these fix its summary key, its completion bound, and its
// per-call deadline when the author leaves them unset.
const (
	// DefaultSummaryKey is the blackboard key a summarize strategy writes its
	// summary under when the author declares no `key`. Successive
	// summarizations of the same step append new versions, so the key's
	// version history is the chained-summary audit (ADR-014).
	DefaultSummaryKey = "context_summary"
	// DefaultSummaryMaxTokens is the summary completion bound (max_tokens for
	// the summarizer call) when unset — a summary must be materially smaller
	// than the span it replaces for compaction to make progress.
	DefaultSummaryMaxTokens = 256
	// DefaultSummaryTimeout is the per-summarizer-call deadline when unset. A
	// summarizer failure (including a timeout) falls back to the next
	// deterministic strategy and never blocks the step, so this is a
	// promptness bound, not a correctness one.
	DefaultSummaryTimeout = 60 * time.Second
	// MaxSummaryTimeout bounds an authored summarizer timeout.
	MaxSummaryTimeout = 10 * time.Minute
)

// ContextSourceKind is the discriminator of a context source.
type ContextSourceKind string

// Context source kinds (ADR-014). Order in a spec is precedence and message
// order: an earlier source wins a token-budget contest (12.4) and appears
// first in the assembled request.
const (
	// SourceStepOutput pulls a named upstream step's output (optionally a JSON
	// pointer into it) — the 8.2 templating ref, as a context source.
	SourceStepOutput ContextSourceKind = "step_output"
	// SourceBlackboard selects run-scoped blackboard entries by key (the head)
	// or by tag (every head carrying all listed tags).
	SourceBlackboard ContextSourceKind = "blackboard"
	// SourceRetrieval runs a retriever query and includes the ranked results.
	SourceRetrieval ContextSourceKind = "retrieval"
	// SourceLiteral is an inline constant (a system-prompt fragment,
	// instructions) included verbatim.
	SourceLiteral ContextSourceKind = "literal"
	// SourceThread is an agent handoff thread (ticket 14.2, ADR-016): the full
	// version History of a thread key (default "thread"), rendered as ordered
	// messages carrying author/role/iteration — the "conversation view" a role's
	// default context assembles so a downstream agent sees the prior turns. An
	// optional Role filters the messages to one role. Unlike SourceBlackboard
	// (which reads heads), a thread source reads every version of its key.
	SourceThread ContextSourceKind = "thread"
)

var contextSourceKinds = []ContextSourceKind{
	SourceStepOutput, SourceBlackboard, SourceRetrieval, SourceLiteral, SourceThread,
}

// ContextMissingPolicy governs what happens when a source resolves to nothing
// (an upstream step that did not succeed, a blackboard key with no head, a
// tag matching no heads, a retriever returning no results).
type ContextMissingPolicy string

// Missing-source policies (ADR-014). The default is Error — the 8.2
// strict-reference stance: a source an author names is expected to exist, and
// a silent omission would ship a subtly wrong prompt.
const (
	// MissingError fails the step permanently before any provider call.
	MissingError ContextMissingPolicy = "error"
	// MissingSkip omits the source and records it in the context_assembled
	// audit event, letting the step proceed.
	MissingSkip ContextMissingPolicy = "skip"
)

var contextMissingPolicies = []ContextMissingPolicy{MissingError, MissingSkip}

// CompactionStrategyKind is one deterministic compaction strategy (ADR-014,
// ticket 12.4). When an assembled context exceeds the step's budget, the
// step's ordered pipeline is applied, each strategy only while still over
// budget, until the assembly fits or a typed ContextOverBudget failure fires
// before any provider call.
type CompactionStrategyKind string

const (
	// DropLowestPriority evicts whole non-pinned sources by ascending priority
	// (declaration order breaks ties, later-declared first) until under budget.
	DropLowestPriority CompactionStrategyKind = "drop_lowest_priority"
	// TruncateOldest middle-out-truncates the oldest non-pinned sources (an
	// explicit elision marker) down to an optional per-strategy min_tokens floor.
	TruncateOldest CompactionStrategyKind = "truncate_oldest"
	// SlidingWindow keeps only the last N non-pinned sources in message order
	// (pinned sources are always kept and do not count toward N).
	SlidingWindow CompactionStrategyKind = "sliding_window"
	// SummarizeStrategy replaces the oldest evicted non-pinned span with a
	// cheap-model summary written to the run blackboard (ticket 12.5). The
	// summary is a synthetic entry in the assembly and, once durable, a
	// versioned blackboard entry other steps can read; chained summaries fold
	// an earlier summary with newer turns. A summarizer failure falls back to
	// the next deterministic strategy and never blocks the step.
	SummarizeStrategy CompactionStrategyKind = "summarize"
)

var compactionStrategyKinds = []CompactionStrategyKind{
	DropLowestPriority, TruncateOldest, SlidingWindow, SummarizeStrategy,
}

// CompactionStrategy is one entry in a step's ordered compaction pipeline. The
// per-strategy parameters below are meaningful only for the strategy that owns
// them; Validate rejects a parameter set on a strategy it does not belong to.
type CompactionStrategy struct {
	// Strategy selects which compaction to apply. Required.
	Strategy CompactionStrategyKind `json:"strategy"`

	// N is the window size for sliding_window (the count of most-recent
	// non-pinned sources to keep). Required for sliding_window (must be >= 1),
	// forbidden for every other strategy.
	N *int `json:"n,omitempty"`

	// MinTokens is the floor a truncate_oldest source is truncated down to
	// (truncate_oldest only, optional, >= 0). A source at its floor is not
	// truncated further; nil means truncate toward empty. Forbidden for every
	// other strategy.
	MinTokens *int `json:"min_tokens,omitempty"`

	// Model is the cheap model the summarize strategy calls to summarize an
	// evicted span (summarize only). Required for summarize (routed through
	// the llm registry like an llm step's model), forbidden for every other
	// strategy.
	Model string `json:"model,omitempty"`

	// Key is the blackboard key the summarize strategy writes its summary
	// under (summarize only, optional). Defaults to DefaultSummaryKey; must be
	// a valid blackboard key. Successive summarizations append new versions,
	// so the key's history is the chained-summary audit. Forbidden for every
	// other strategy.
	Key string `json:"key,omitempty"`

	// MaxTokens is the summary completion bound — the max_tokens of the
	// summarizer call (summarize only, optional, >= 1). Defaults to
	// DefaultSummaryMaxTokens. Forbidden for every other strategy.
	MaxTokens *int `json:"max_tokens,omitempty"`

	// Timeout is the per-summarizer-call deadline as a Go duration string
	// (summarize only, optional). Defaults to DefaultSummaryTimeout, at most
	// MaxSummaryTimeout. A timeout is a summarizer failure (fall back to the
	// next strategy), never a step failure. Forbidden for every other strategy.
	Timeout string `json:"timeout,omitempty"`
}

// EffectiveSummaryKey resolves a summarize strategy's blackboard key to its
// default when unset.
func (c CompactionStrategy) EffectiveSummaryKey() string {
	if c.Key == "" {
		return DefaultSummaryKey
	}
	return c.Key
}

// EffectiveSummaryMaxTokens resolves a summarize strategy's summary completion
// bound to its default when unset.
func (c CompactionStrategy) EffectiveSummaryMaxTokens() int {
	if c.MaxTokens == nil {
		return DefaultSummaryMaxTokens
	}
	return *c.MaxTokens
}

// EffectiveSummaryTimeout resolves a summarize strategy's per-call deadline to
// its default when unset. The value is validated at submit time, so a parse
// error here falls back to the default rather than propagating.
func (c CompactionStrategy) EffectiveSummaryTimeout() time.Duration {
	if c.Timeout == "" {
		return DefaultSummaryTimeout
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 {
		return DefaultSummaryTimeout
	}
	return d
}

// ContextSource is one source in a step's ordered context spec. Exactly one
// kind's fields are meaningful; Validate rejects fields that do not belong to
// the declared kind.
type ContextSource struct {
	// Kind selects which of the fields below apply. Required.
	Kind ContextSourceKind `json:"kind"`

	// Name is the label the assembled request wraps this source under (a
	// stable header the model and the audit event see). Optional; when empty
	// the assembler derives "<kind>#<index>". When set it must be unique
	// within the spec.
	Name string `json:"name,omitempty"`

	// Step is the upstream step id whose output this source pulls
	// (step_output only). Required for step_output; must be a normal-edge
	// ancestor of the declaring step (the 8.2 upstream rule).
	Step string `json:"step,omitempty"`

	// Path is an RFC 6901 JSON pointer into the referenced step's output
	// (step_output only); empty ("" — the default) selects the whole output.
	// "/text" is the natural choice for an upstream llm step.
	Path string `json:"path,omitempty"`

	// Key selects a single blackboard entry's head by key (blackboard only), or
	// the thread key whose History a thread source renders (thread only,
	// optional — defaults to blackboard.DefaultThreadKey). Mutually exclusive
	// with Tags for a blackboard source; exactly one of Key/Tags is required
	// there.
	Key string `json:"key,omitempty"`

	// Role filters a thread source's messages to a single role (thread only,
	// optional — empty renders every turn). Forbidden for every other kind.
	Role string `json:"role,omitempty"`

	// Tags selects every blackboard head carrying all listed tags (blackboard
	// only, AND semantics — the store's ListFilter). Mutually exclusive with
	// Key.
	Tags []string `json:"tags,omitempty"`

	// Retriever names the retriever to query (retrieval only). Required for
	// retrieval.
	Retriever string `json:"retriever,omitempty"`

	// Query is the (static) query text (retrieval only). Required for
	// retrieval. A dynamic query flows through a `retrieve` step and a
	// step_output source — templating in the context block is not supported.
	Query string `json:"query,omitempty"`

	// TopK bounds the retrieval results (retrieval only); nil defaults to the
	// retrieve executor's default (5), capped at its maximum (100).
	TopK *int `json:"top_k,omitempty"`

	// Text is the inline literal (literal only). Required for literal.
	Text string `json:"text,omitempty"`

	// MaxTokens caps this source's contribution; nil means no per-source cap.
	// When set, the assembler truncates the source's rendered text to the cap
	// with an explicit elision marker. Mutually exclusive with Pinned (a
	// pinned source is never truncated).
	MaxTokens *int `json:"max_tokens,omitempty"`

	// Pinned, when true, exempts this source from every compaction strategy
	// (12.4) — it is always included, never truncated. Mutually exclusive with
	// MaxTokens.
	Pinned bool `json:"pinned,omitempty"`

	// Priority orders a source for the drop_lowest_priority strategy (12.4):
	// lower drops first. nil defaults to 0. Ties break on declaration order
	// (later-declared drops first). Ignored for a pinned source (never dropped)
	// and by the other strategies (which order by declaration position).
	Priority *int `json:"priority,omitempty"`

	// OnMissing governs a source that resolves to nothing; empty defaults to
	// "error".
	OnMissing ContextMissingPolicy `json:"on_missing,omitempty"`
}

// EffectivePriority resolves Priority to its default (0) when unset.
func (s ContextSource) EffectivePriority() int {
	if s.Priority == nil {
		return 0
	}
	return *s.Priority
}

// EffectiveMissingPolicy resolves OnMissing to its default (error) when unset.
func (s ContextSource) EffectiveMissingPolicy() ContextMissingPolicy {
	if s.OnMissing == "" {
		return MissingError
	}
	return s.OnMissing
}

// ContextSpec is a step's authored context block (ADR-014); nil on the step
// envelope means the `context` key was absent (the request is built from the
// config alone). Uniform across the llm family, so it lives on the step
// envelope, not in the per-type config — like cache and validation.
type ContextSpec struct {
	// Sources is the ordered list assembled into the provider request. Order
	// is precedence and message order. Empty/absent means the block declares
	// no sources (an authoring mistake — Validate rejects an empty block, like
	// an empty blackboard block).
	Sources []ContextSource `json:"sources,omitempty"`

	// BudgetTokens caps the assembled+framed request tokens (ticket 12.4).
	// When set (> 0) and the assembled request exceeds it, the Compaction
	// pipeline is applied; if the pipeline cannot fit the budget the step fails
	// with a typed error before any provider call. nil means no explicit budget
	// (12.3 behavior — no compaction); 12.6 defaults it from the model context
	// window when unset.
	BudgetTokens *int `json:"budget_tokens,omitempty"`

	// Compaction is the ordered strategy pipeline applied when the assembled
	// request exceeds BudgetTokens (ticket 12.4), each strategy running only
	// while still over budget. Empty means no compaction — an over-budget
	// assembly is then a typed failure (a pure pre-flight guardrail). A
	// pipeline with no BudgetTokens is inert in 12.4 (nothing to compare
	// against) and fires once 12.6 defaults the budget.
	Compaction []CompactionStrategy `json:"compaction,omitempty"`
}
