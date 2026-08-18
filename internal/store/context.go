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
		args.Revisions[i].Attempt = step.AttemptCount
		if err := appendEvent(ctx, gq, op, args.RunID, args.Revisions[i]); err != nil {
			return err
		}
	}
	args.Event.StepID = args.StepID
	args.Event.Attempt = step.AttemptCount
	if err := appendEvent(ctx, gq, op, args.RunID, args.Event); err != nil {
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
		args.Revisions[i].Attempt = step.AttemptCount
		if err := appendEvent(ctx, gq, op, args.RunID, args.Revisions[i]); err != nil {
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
