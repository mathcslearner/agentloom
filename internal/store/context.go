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
	// dropped) in declaration (precedence) order — the final state after any
	// compaction.
	Sources []ContextSourceRecord `json:"sources"`
	// ContextTokens is the (post-compaction) assembled context's counted size.
	ContextTokens int `json:"context_tokens"`
	// PreflightTokens is the counted size of the whole assembled request
	// (system, messages, tools, response format) — the number the window
	// guardrail (12.6) compares against the model context window.
	PreflightTokens int `json:"preflight_tokens"`
	// BudgetTokens is the context budget in force, or 0 when the step declared
	// none (12.3 behavior — no compaction).
	BudgetTokens int `json:"budget_tokens,omitempty"`
	// RawContextTokens / RawPreflightTokens are the pre-compaction totals; equal
	// to the final totals when no compaction ran.
	RawContextTokens   int `json:"raw_context_tokens,omitempty"`
	RawPreflightTokens int `json:"raw_preflight_tokens,omitempty"`
	// Revisions is the number of compaction strategies that ran (each also its
	// own context_revision event); 0 when the assembly fit the budget.
	Revisions int `json:"revisions,omitempty"`
}

// ContextRevisionActionRecord is one entry's drop/truncate action in a
// compaction strategy (ticket 12.4).
type ContextRevisionActionRecord struct {
	SourceIndex  int    `json:"source_index"`
	Name         string `json:"name"`
	Action       string `json:"action"` // "dropped" | "truncated"
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
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
	// Actions is the per-entry drop/truncate detail.
	Actions []ContextRevisionActionRecord `json:"actions,omitempty"`
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
	log.From(ctx).InfoContext(ctx, "context assembled for step",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)), slog.Int("revisions", len(args.Revisions)))
	return nil
}
