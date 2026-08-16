package store

// Context assembly's durable audit record (ticket 12.3, ADR-014). The
// pre-execution assembly stage builds an llm step's request from its
// materialized `context` spec and records what it did — which sources were
// included, skipped (missing under `on_missing: skip`), or truncated (over a
// per-source cap), the counter fingerprint, and the pre-flight token total —
// as a context_assembled event, so the whole pre-execution context decision
// is reconstructable from durable state. It is not a state transition (the
// assembled content becomes durable on the attempt via the request the
// executor sends), so this mirrors RecordModelDowngrade: it fences on the
// caller's live claim and appends the event under the run lock.

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// ContextSourceRecord is one source's disposition in a context_assembled
// event: what it was, how it resolved, and its token contribution.
type ContextSourceRecord struct {
	// Index is the source's position in the declared spec (precedence order).
	Index int `json:"index"`
	// Kind is the source kind (step_output | blackboard | retrieval | literal).
	Kind string `json:"kind"`
	// Name is the source's label (authored or derived).
	Name string `json:"name"`
	// Ref is a human-readable identifier of what was pulled (the step id, the
	// blackboard key/tag selector, the retriever query, or "literal").
	Ref string `json:"ref,omitempty"`
	// Status is the disposition: included | truncated | skipped.
	Status string `json:"status"`
	// Reason explains a skip (missing under on_missing:skip) or a truncation.
	Reason string `json:"reason,omitempty"`
	// Tokens is the source's counted contribution to the assembled context
	// (after any truncation); 0 for a skipped source.
	Tokens int `json:"tokens"`
	// Pinned reports whether the source is exempt from compaction (12.4).
	Pinned bool `json:"pinned,omitempty"`
}

// ContextAssembledEvent is the context_assembled event payload (ticket 12.3,
// ADR-014): the assembled sources' final dispositions (after any 12.4
// compaction), the counter that measured them, and the token totals. The
// Sources and *Tokens fields describe what is actually sent — a compacted
// assembly reports the shrunk numbers, and the raw (pre-compaction) figures
// ride alongside for the audit.
type ContextAssembledEvent struct {
	StepID    string `json:"step_id"`
	AttemptNo int32  `json:"attempt"`
	// CounterID is the tokens.Counter fingerprint used for every count here
	// (e.g. "mock/estimate@1", "fallback/chars4@1").
	CounterID string `json:"counter_id"`
	// Sources is the per-source disposition (included | truncated | skipped |
	// dropped | summarized) in declaration (precedence) order — the final state
	// after any compaction. A summarize strategy appends a synthetic
	// "summary"-kind source for the summary it produced.
	Sources []ContextSourceRecord `json:"sources"`
	// ContextTokens is the (post-compaction) assembled context's counted size.
	ContextTokens int `json:"context_tokens"`
	// PreflightTokens is the counted size of the whole assembled request
	// (system, messages, tools, response format) — the number the window
	// guardrail (12.6) compares against the model context window.
	PreflightTokens int `json:"preflight_tokens"`
	// BudgetTokens is the context budget in force, or 0 when the step declared
	// none and no window defaulted one (12.3 behavior — no compaction).
	BudgetTokens int `json:"budget_tokens,omitempty"`
	// BudgetSource records how BudgetTokens was chosen (ticket 12.6): "explicit"
	// (author's budget_tokens), "window" (defaulted from the model context
	// window), "explicit_capped" (author's budget tightened down to the window
	// default), or "" (no budget). Together with ContextWindow it makes the
	// window-guardrail decision auditable.
	BudgetSource string `json:"budget_source,omitempty"`
	// ContextWindow is the model's context window in tokens (ticket 12.6), 0 when
	// the model is unguarded (no catalog window). The guardrail guarantees
	// PreflightTokens + max_tokens ≤ ContextWindow for a guarded step.
	ContextWindow int `json:"context_window,omitempty"`
	// RawContextTokens / RawPreflightTokens are the pre-compaction totals; equal
	// to the final totals when no compaction ran.
	RawContextTokens   int `json:"raw_context_tokens,omitempty"`
	RawPreflightTokens int `json:"raw_preflight_tokens,omitempty"`
	// Revisions is the number of compaction strategies that ran (each also its
	// own context_revision event); 0 when the assembly fit the budget.
	Revisions int `json:"revisions,omitempty"`
	// Summaries is the number of summarizations that produced a summary
	// (ticket 12.5), each priced as compaction overhead; 0 when none ran.
	Summaries int `json:"summaries,omitempty"`
}

// ContextRevisionActionRecord is one entry's drop/truncate/summarize action in
// a compaction strategy (tickets 12.4/12.5).
type ContextRevisionActionRecord struct {
	SourceIndex  int    `json:"source_index"`
	Name         string `json:"name"`
	Action       string `json:"action"` // "dropped" | "truncated" | "summarized"
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
}

// ContextRevisionSummaryRecord is one summarization's provenance in a summarize
// strategy's context_revision (ticket 12.5): the blackboard key@version the
// summary landed at, the model/resource billed as overhead, and the span it
// folded.
type ContextRevisionSummaryRecord struct {
	Key           string   `json:"key"`
	Version       int      `json:"version"`
	ParentVersion int      `json:"parent_version,omitempty"`
	Model         string   `json:"model"`
	Resource      string   `json:"resource"`
	SpanNames     []string `json:"span_names,omitempty"`
	SpanTokens    int      `json:"span_tokens"`
	SummaryTokens int      `json:"summary_tokens"`
	CacheHit      bool     `json:"cache_hit,omitempty"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
}

// ContextRevisionEvent is the context_revision event payload (ticket 12.4,
// ADR-014): one deterministic compaction strategy's application to shrink an
// over-budget assembled context — what ran, its parameters, the framed-request
// tokens before/after, and the per-entry actions. Every strategy that runs
// writes one, before the final context_assembled event, so the whole
// compaction decision is reconstructable per step attempt.
type ContextRevisionEvent struct {
	StepID    string `json:"step_id"`
	AttemptNo int32  `json:"attempt"`
	// Index is the strategy's position in the pipeline.
	Index int `json:"index"`
	// Strategy is the strategy that ran (drop_lowest_priority | truncate_oldest
	// | sliding_window).
	Strategy string `json:"strategy"`
	// N / MinTokens echo the strategy's parameters when set.
	N         *int `json:"n,omitempty"`
	MinTokens *int `json:"min_tokens,omitempty"`
	// Budget is the token budget in force.
	Budget int `json:"budget"`
	// TokensBefore / TokensAfter are the framed-request totals around the
	// strategy — the numbers compared to Budget.
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
	// Changed reports whether the strategy altered the assembly.
	Changed bool `json:"changed"`
	// Actions is the per-entry drop/truncate/summarize detail.
	Actions []ContextRevisionActionRecord `json:"actions,omitempty"`
	// Summaries is the summarization detail for a summarize strategy (12.5).
	Summaries []ContextRevisionSummaryRecord `json:"summaries,omitempty"`
	// Error, when set, is why a summarize strategy fell back to the next
	// strategy (ticket 12.5) — the warning content: the summarizer was
	// unavailable, errored, timed out, or could not shrink the span. It is the
	// audit record of a summarizer failure that did not block the step.
	Error string `json:"error,omitempty"`
	// Kept is the names of the entries still present after this strategy.
	Kept []string `json:"kept,omitempty"`
}

// RecordContextAssembledArgs are the inputs to RecordContextAssembled.
type RecordContextAssembledArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller; the record
	// is rejected if the step is no longer running under it (a zombie must not
	// record a misleading assembly manifest).
	ClaimID uuid.UUID
	// Event is the assembly detail recorded on the context_assembled event.
	Event ContextAssembledEvent
	// Revisions are the compaction strategies that ran (ticket 12.4), each
	// appended as its own context_revision event before the assembled event, in
	// order. Empty when the assembly fit its budget (or the step declared none).
	Revisions []ContextRevisionEvent
	// CostRows are the summarize compaction overhead charges (ticket 12.5),
	// ledgered inside this fenced transaction — under the same claim fence as
	// the events, before the provider call — so a summarizer's spend is metered
	// even if the step later fails or retries (the 11.5 judge-overhead
	// discipline). Empty when no summarization billed.
	CostRows []AttemptCostArgs
	// Now is the injected current time. Required.
	Now time.Time
}

// RecordContextAssembled appends the context_assembled event for a claim whose
// context spec the assembly stage built into the request (ticket 12.3,
// ADR-014). Like RecordModelDowngrade it is not a state transition — no status
// changes — so it only fences the record on the caller's live claim (the step
// must still be running under it) and appends the event under the run lock. A
// fenced caller gets a *TransitionError so the engine abandons like any other
// fenced write.
func RecordContextAssembled(ctx context.Context, q Querier, args RecordContextAssembledArgs) error {
	const op = "record context assembled"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return err
	}
	step, err := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.StepID})
	if err != nil {
		return wrapErr(op, err)
	}
	if step.Status != StepStatusRunning || step.ClaimID == nil || *step.ClaimID != args.ClaimID {
		return stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusRunning, claim: &args.ClaimID,
		})
	}
	// Compaction revisions first (ticket 12.4): each strategy that ran is its
	// own event, appended before the final assembled event so the log reads
	// raw → revision* → assembled in seq order — the whole compaction decision
	// under one claim fence, in one transaction.
	for i := range args.Revisions {
		args.Revisions[i].StepID = args.StepID
		args.Revisions[i].AttemptNo = step.AttemptCount
		if err := appendEvent(ctx, gq, op, args.RunID, EventContextRevision, args.Revisions[i]); err != nil {
			return err
		}
	}
	args.Event.StepID = args.StepID
	args.Event.AttemptNo = step.AttemptCount
	if err := appendEvent(ctx, gq, op, args.RunID, EventContextAssembled, args.Event); err != nil {
		return err
	}
	// Summarize compaction overhead (ticket 12.5): ledger each summarizer call's
	// spend under the same claim fence and run lock as the events, before the
	// provider call — so a summary's cost is metered even if the step later
	// fails or retries (the 11.5 judge-overhead discipline). The rows carry
	// their own step/attempt/entry; the cost_updated event ApplyAttemptCost
	// appends rides the same monotonic seq.
	for _, row := range args.CostRows {
		if _, err := ApplyAttemptCost(ctx, q, row); err != nil {
			return err
		}
	}
	log.From(ctx).InfoContext(ctx, "context assembled for step",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)),
		slog.Int("revisions", len(args.Revisions)), slog.Int("cost_rows", len(args.CostRows)))
	return nil
}

// RecordContextRevisions appends the compaction revision events and ledgers the
// summarize overhead rows for a claim whose compaction pipeline COULD NOT fit
// the budget (ticket 12.5). Unlike RecordContextAssembled it appends no
// context_assembled event — there is no assembly to send, the step fails
// permanently before the provider call — but a summarizer that ran and billed
// before the pipeline gave up must still be audited and metered, so the
// revisions and the overhead ledger rows are written under the same claim fence
// and run lock. Like RecordContextAssembled it fences on the caller's live
// claim; a fenced caller gets a *TransitionError and abandons.
func RecordContextRevisions(ctx context.Context, q Querier, args RecordContextAssembledArgs) error {
	const op = "record context revisions"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return err
	}
	step, err := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.StepID})
	if err != nil {
		return wrapErr(op, err)
	}
	if step.Status != StepStatusRunning || step.ClaimID == nil || *step.ClaimID != args.ClaimID {
		return stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusRunning, claim: &args.ClaimID,
		})
	}
	for i := range args.Revisions {
		args.Revisions[i].StepID = args.StepID
		args.Revisions[i].AttemptNo = step.AttemptCount
		if err := appendEvent(ctx, gq, op, args.RunID, EventContextRevision, args.Revisions[i]); err != nil {
			return err
		}
	}
	for _, row := range args.CostRows {
		if _, err := ApplyAttemptCost(ctx, q, row); err != nil {
			return err
		}
	}
	log.From(ctx).InfoContext(ctx, "context compaction failed to fit budget; revisions recorded",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)),
		slog.Int("revisions", len(args.Revisions)), slog.Int("cost_rows", len(args.CostRows)))
	return nil
}
