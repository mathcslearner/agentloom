package store

// Guarded state transitions (ticket 2.6): the only mutation surface for
// run/step status, dependency counters, and edge resolutions. Every
// function implements one row of the ADR-004 transition matrix as a
// conditional UPDATE, appends the transition's event in the same
// transaction, and returns a typed error — *TransitionError unwrapping
// ErrConflict — when the guard rejects it. M4 composes these into the
// worker's claim and completion transactions; CEL evaluation, the branch
// first-match rule, and outbox writes for newly-ready steps are the
// engine's (M4.3), which passes the verdicts in via ResolveEdge/ReadyStep.
//
// Callers must run inside WithTx and pass the Querier its callback
// received; anything else fails fast with ErrNoTx. Lock ordering: every
// function here — ResolveEdge included — first acquires the run-row lock
// (LockRun, a FOR UPDATE read), then touches step/edge rows, making
// composed transactions deadlock-free by uniform ordering (the per-run
// serialization this implies is an accepted ADR-004 trade). The event seq
// is allocated only after the guarded CAS succeeds, so a rejected
// transition writes nothing: callers may drop a typed conflict (the
// join-any late-firing absorption, the completion-tx ACK-and-drop) and
// commit the surrounding transaction without burning a seq — the event
// log stays gap-free by construction, not by rollback.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Transition event payloads (v1 minimal shapes; ADR-018 owns the formal
// envelope). step_ready/step_skipped use stepIDPayload (instantiate.go).
// stepClaimedPayload is shared by step_claimed (ClaimID = the fresh fence)
// and step_reclaimed (ClaimID = the displaced holder's cleared fence).
type stepClaimedPayload struct {
	StepID    string `json:"step_id"`
	ClaimID   string `json:"claim_id"`
	AttemptNo int32  `json:"attempt_no"`
}

type stepFinishedPayload struct {
	StepID    string `json:"step_id"`
	AttemptNo int32  `json:"attempt_no"`
}

// stepRetryPayload is the step_retry_scheduled event body (ticket 5.2):
// which attempt failed, how it was classed, and when the next is due.
type stepRetryPayload struct {
	StepID        string    `json:"step_id"`
	AttemptNo     int32     `json:"attempt_no"`
	Class         string    `json:"class"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

// stepThrottlePayload is the step_throttled event body (ticket 9.2): which
// attempt was deferred, the resource whose limit denied it, which bucket
// (requests/tokens/both) lacked capacity, the limiter's own retry_after,
// and when the re-dispatch is due.
type stepThrottlePayload struct {
	StepID        string    `json:"step_id"`
	AttemptNo     int32     `json:"attempt_no"`
	Resource      string    `json:"resource"`
	Bucket        string    `json:"bucket"`
	RetryAfter    string    `json:"retry_after"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

// stepDeadLetteredPayload is the step_dead_lettered event body (ticket
// 5.4): why the step died (source), the judged class (empty for poison),
// the attempt count at death, and the dead_letters seq the full record
// lives under.
type stepDeadLetteredPayload struct {
	StepID   string `json:"step_id"`
	Source   string `json:"source"`
	Class    string `json:"class,omitempty"`
	Attempts int32  `json:"attempts"`
	Seq      int32  `json:"seq"`
}

// stepCancelledPayload serves step_cancelled and step_revived (ticket
// 5.4): the step and why it was written off / revived.
type stepCancelledPayload struct {
	StepID string `json:"step_id"`
	Reason string `json:"reason"`
}

// ClaimStepArgs are the inputs to ClaimStep.
type ClaimStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Now is the injected current time. Required.
	Now time.Time
	// TraceSpan is the claiming attempt's span context in traceparent
	// format (ticket 7.3), stamped onto run_steps.trace_span by the claim
	// CAS. The value it overwrites — the previous attempt's span — is
	// surfaced on ClaimOrigin.PrevTraceSpan, which is how retries and
	// takeovers link back to the attempt they re-execute. Empty means no
	// context (tracing off).
	TraceSpan string
}

// ClaimOrigin describes the row a winning claim CAS consumed: the step's
// status and updated_at immediately before the transition (ticket 7.2).
// For a ready step, ChangedAt is the instant it turned ready — so
// claim-time minus ChangedAt is the ready→running scheduling latency the
// engine's histogram observes; for a retrying step the interval includes
// the deliberate backoff and is not a scheduling latency. Read under the
// run lock, so it is exact with respect to the claim.
type ClaimOrigin struct {
	// FromStatus is the pre-claim step status (ready or retrying).
	FromStatus string
	// ChangedAt is the pre-claim updated_at: when the step entered
	// FromStatus.
	ChangedAt time.Time
	// Known reports whether the pre-read succeeded; observers must skip
	// recording when false. Meaningful only when ClaimStepWithOrigin
	// returns nil error.
	Known bool
	// PrevTraceSpan is the trace_span the claim overwrote (ticket 7.3):
	// the previous attempt's span context, present when this claim
	// re-executes work — a due retry, or a step taken over after its
	// holder's lease expired. The engine links the new attempt span to it
	// (ADR-008: links, never parent-child). Empty for first attempts.
	PrevTraceSpan string
	// RunTrace is the run's durable root trace context, read from the run
	// row the claim already locks — the parent for re-dispatch envelopes
	// that descend from no live span (the delayed retry envelope).
	RunTrace TraceContext
	// BudgetNanoUSD is the run's spend budget in nano-USD (ticket 10.3), read
	// from the same locked run row — nil means unbudgeted. SpentNanoUSD is
	// the run's spend so far (exact as of the claim, since the claim holds
	// the run lock), and OnBudgetExceeded is the materialized park|fail
	// disposition. The budget middleware uses these to project spend without
	// a second read; a $0 (unbudgeted) run short-circuits the check.
	BudgetNanoUSD    *int64
	SpentNanoUSD     int64
	OnBudgetExceeded string
}

// ClaimStep transitions a step ready → running — or retrying → running
// once its next_attempt_at has passed (ticket 5.2; the backoff guard means
// an early duplicate delivery bounces instead of executing before its
// time). It stamps a fresh claim_id (the fencing token, carried on the
// returned row), increments the durable attempt counter, inserts the
// step_attempts row, and appends the step_claimed event. Exactly one of N
// racing claimers wins; the rest get ErrConflict — the substrate that
// turns at-least-once delivery into effectively-once execution (losers
// ACK-and-drop). Steps of non-running runs are refused with
// ConflictRunNotRunning (ADR-006: the claim path refuses terminal runs;
// 5.6's park/cancel reuses the guard).
func ClaimStep(ctx context.Context, q Querier, args ClaimStepArgs) (gen.RunStep, error) {
	step, _, err := ClaimStepWithOrigin(ctx, q, args)
	return step, err
}

// ClaimStepWithOrigin is ClaimStep plus the pre-claim row observation the
// scheduling-latency metric needs (ticket 7.2): one extra primary-key
// read under the run lock every transition already takes, so the origin
// is exact and race-free. The read is best-effort — a failure leaves
// origin.Known false and never changes ClaimStep's error contract.
func ClaimStepWithOrigin(ctx context.Context, q Querier, args ClaimStepArgs) (gen.RunStep, ClaimOrigin, error) {
	const op = "claim step"
	var origin ClaimOrigin
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, origin, err
	}
	run, err := lockRun(ctx, gq, op, args.RunID)
	if err != nil {
		return gen.RunStep{}, origin, err
	}
	if run.Status != RunStatusRunning {
		step, gerr := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.StepID})
		if gerr != nil {
			return gen.RunStep{}, origin, wrapErr(op, gerr)
		}
		return gen.RunStep{}, origin, fmt.Errorf("store: %s: %w", op, &TransitionError{
			Entity: "step", RunID: args.RunID, StepID: args.StepID,
			From: run.Status, To: StepStatusRunning,
			Reason: ConflictRunNotRunning, CurrentClaimID: step.ClaimID,
		})
	}
	if prev, perr := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.StepID}); perr == nil {
		origin = ClaimOrigin{FromStatus: prev.Status, ChangedAt: prev.UpdatedAt, Known: true}
		if prev.TraceSpan != nil {
			origin.PrevTraceSpan = *prev.TraceSpan
		}
	}
	origin.RunTrace = TraceContext{Parent: textOrEmpty(run.TraceParent), State: textOrEmpty(run.TraceState)}
	origin.BudgetNanoUSD = run.BudgetNanoUsd
	origin.SpentNanoUSD = run.SpentNanoUsd
	origin.OnBudgetExceeded = run.OnBudgetExceeded
	claimID := uuid.New()
	step, err := gq.ClaimRunStep(ctx, gen.ClaimRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &claimID, Now: args.Now,
		TraceSpan: nullableText(args.TraceSpan),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, origin, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusReady, to: StepStatusRunning,
		})
	}
	if err != nil {
		return gen.RunStep{}, origin, wrapErr(op, err)
	}
	_, err = gq.CreateStepAttempt(ctx, gen.CreateStepAttemptParams{
		RunID: args.RunID, StepID: args.StepID, AttemptNo: step.AttemptCount,
		ClaimID: claimID, StartedAt: &args.Now,
	})
	if err != nil {
		return gen.RunStep{}, origin, wrapErr(op+": insert attempt", err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepClaimed, stepClaimedPayload{
		StepID: args.StepID, ClaimID: claimID.String(), AttemptNo: step.AttemptCount,
	}); err != nil {
		return gen.RunStep{}, origin, err
	}
	log.From(ctx).DebugContext(ctx, "step claimed",
		log.RunID(args.RunID.String()), log.StepID(args.StepID), log.Attempt(int(step.AttemptCount)))
	return step, origin, nil
}

// TakeoverStepArgs are the inputs to TakeoverStep.
type TakeoverStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the observed holder's fencing token — from the claim
	// conflict's CurrentClaimID (worker path) or the staleness scan
	// (reconciler). The CAS is fenced on it: if the step was already taken
	// over and re-claimed by a live worker, the observation is stale and
	// the takeover is rejected instead of stealing the live claim.
	ClaimID uuid.UUID
	// Now is the injected current time. Required.
	Now time.Time
}

// TakeoverStep transitions a step running → ready after its holder's lease
// expired (ADR-005 lease-expiry takeover; ADR-004's M4 running → ready
// row): it clears claim_id — the moment the zombie loses its fence — closes
// the holder's dangling attempt row with the administrative outcome `lost`,
// and appends the step_reclaimed event. Re-dispatch is the caller's move:
// the worker path claims in the same transaction (its reclaimed entry is in
// hand, no message needed); the reconciler writes an outbox row instead.
func TakeoverStep(ctx context.Context, q Querier, args TakeoverStepArgs) (gen.RunStep, error) {
	const op = "takeover step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.TakeoverRunStep(ctx, gen.TakeoverRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusReady, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := finishAttempt(ctx, gq, op, step, AttemptOutcomeLost, nil, nil, nil, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepReclaimed, stepClaimedPayload{
		StepID: args.StepID, ClaimID: args.ClaimID.String(), AttemptNo: step.AttemptCount,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step taken over",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// SucceedStepArgs are the inputs to SucceedStep.
type SucceedStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller; a stale
	// one (the step was reclaimed) is rejected with a claim_mismatch
	// TransitionError.
	ClaimID uuid.UUID
	// Output is the step's result; nil stores NULL.
	Output json.RawMessage
	// Usage is the attempt's token accounting on a successful llm call
	// (ticket 8.6), stored on the attempt row for M10's cost ledger; nil
	// (the common case — every non-llm step) stores NULL.
	Usage json.RawMessage
	// Verdict is the output-validation chain verdict (ticket 11.1, ADR-013)
	// on a validated step whose output passed; nil (the common case — no
	// chain) stores NULL.
	Verdict json.RawMessage
	// Repair is the structured-output provenance (ticket 11.3, ADR-013) on an
	// llm step that declared an output_format; nil (the common case) stores
	// NULL.
	Repair json.RawMessage
	// Now is the injected current time. Required.
	Now time.Time
}

// SucceedStep transitions a step running → succeeded, fenced by ClaimID:
// it persists the output, closes the attempt row, bumps the run's
// steps_succeeded aggregate, and appends the step_succeeded event.
// Resolving the step's out-edges is the caller's next move (ResolveEdge),
// inside the same transaction.
func SucceedStep(ctx context.Context, q Querier, args SucceedStepArgs) (gen.RunStep, error) {
	const op = "succeed step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.SucceedRunStep(ctx, gen.SucceedRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID,
		Output: args.Output, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusSucceeded, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := finishAttempt(ctx, gq, op, step, StepStatusSucceeded, nil, args.Usage, args.Verdict, args.Repair, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DSucceeded: 1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepSucceeded, stepFinishedPayload{
		StepID: args.StepID, AttemptNo: step.AttemptCount,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step succeeded",
		log.RunID(args.RunID.String()), log.StepID(args.StepID), log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// DeadLetterStepArgs are the inputs to DeadLetterStep.
type DeadLetterStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller.
	ClaimID uuid.UUID
	// Source is why the step dead-letters: DeadLetterSourceRetriesExhausted
	// or DeadLetterSourcePermanent (the poison source has its own unfenced
	// transition, PoisonDeadLetterStep). Required.
	Source string
	// Outcome is the attempt's ADR-006 error class (transient / permanent /
	// timeout / cancelled) — what the judged failure *was*, even when the
	// disposition is terminal (an exhausted transient records `transient`).
	// Required; the retired bare `failed` is rejected by the schema CHECK.
	Outcome string
	// Error is the failure summary, stored on the step (last failure), the
	// attempt, and the dead_letters record; nil stores NULL.
	Error json.RawMessage
	// Usage is the attempt's token accounting when the dead-lettering
	// attempt spent money — a validation_failed dead-letter (ticket 11.1):
	// the provider call happened and billed even though the output was
	// rejected (ADR-012 rule 5 amended). nil for every mechanical failure
	// (no successful provider call to meter).
	Usage json.RawMessage
	// Verdict is the output-validation chain verdict on a validation_failed
	// dead-letter (ticket 11.1, ADR-013) — the evidence of why the output
	// was rejected, carried onto the attempt row. nil otherwise.
	Verdict json.RawMessage
	// Repair is the structured-output provenance (ticket 11.3, ADR-013) on a
	// validation_failed dead-letter of an llm step that declared an
	// output_format — the shaping that produced the rejected output. nil
	// otherwise.
	Repair json.RawMessage
	// Now is the injected current time. Required.
	Now time.Time
}

// DeadLetterStep transitions a step running → dead_lettered, fenced by
// ClaimID — the terminal failure path (ticket 5.4, ADR-006; the conceptual
// `failed` routing state is passed through inside this one transaction,
// like RetryStep's): it records the error on the step and its attempt row
// (outcome = the ADR-006 class), bumps the run's steps_failed aggregate,
// inserts the dead_letters record (source, class, attempt count at death),
// and appends the step_dead_lettered event. The dead step's out-edges stay
// unresolved (ADR-004) — the run disposition (FailRun or the write-off
// walk) is the caller's next move, inside the same transaction.
func DeadLetterStep(ctx context.Context, q Querier, args DeadLetterStepArgs) (gen.RunStep, error) {
	const op = "dead-letter step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if args.Outcome == "" {
		return gen.RunStep{}, fmt.Errorf("store: %s: empty Outcome — pass the attempt's ADR-006 error class", op)
	}
	if args.Source != DeadLetterSourceRetriesExhausted && args.Source != DeadLetterSourcePermanent {
		return gen.RunStep{}, fmt.Errorf(
			"store: %s: source %q is not a fenced dead-letter source (retries_exhausted/permanent)", op, args.Source)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.DeadLetterRunStep(ctx, gen.DeadLetterRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID,
		Error: args.Error, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusDeadLettered, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := finishAttempt(ctx, gq, op, step, args.Outcome, args.Error, args.Usage, args.Verdict, args.Repair, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DFailed: 1}); err != nil {
		return gen.RunStep{}, err
	}
	class := args.Outcome
	dl, err := gq.CreateDeadLetter(ctx, gen.CreateDeadLetterParams{
		RunID: args.RunID, StepID: args.StepID, Source: args.Source, Class: &class,
		Error: args.Error, AttemptsAtDeath: step.AttemptCount, CreatedAt: args.Now,
	})
	if err != nil {
		return gen.RunStep{}, wrapErr(op+": insert dead letter", err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepDeadLettered, stepDeadLetteredPayload{
		StepID: args.StepID, Source: args.Source, Class: args.Outcome,
		Attempts: step.AttemptCount, Seq: dl.Seq,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step dead-lettered",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// PoisonDeadLetterStepArgs are the inputs to PoisonDeadLetterStep.
type PoisonDeadLetterStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Error is the poison summary (delivery count, threshold); nil stores
	// NULL.
	Error json.RawMessage
	// Payload preserves the raw stream-entry contents (ADR-005 keeps them
	// on the poison message for exactly this); nil stores NULL.
	Payload json.RawMessage
	// Now is the injected current time. Required.
	Now time.Time
}

// PoisonDeadLetterStep transitions a step from any non-terminal status —
// pending, ready, running, or retrying — to dead_lettered with no claim
// fence (ticket 5.4, ADR-006 source `poison`): the poison path holds no
// claim, because the entry's handlers kept dying without ever recording a
// judgment. Clearing claim_id defences any zombie holder (its completion
// CAS then rejects on claim_mismatch and abandons), and a running step's
// dangling attempt closes with the administrative `lost` — nothing judged
// it, matching the takeover convention. The dead_letters record carries a
// NULL class and the raw envelope payload. The run disposition is the
// caller's next move, inside the same transaction.
func PoisonDeadLetterStep(ctx context.Context, q Querier, args PoisonDeadLetterStepArgs) (gen.RunStep, error) {
	const op = "poison dead-letter step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	// The pre-CAS read (race-free under the run lock) decides whether an
	// open attempt needs closing: only a running step has one.
	prior, err := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.StepID})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, fmt.Errorf("store: %s: step %q of run %s: %w", op, args.StepID, args.RunID, ErrNotFound)
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	wasRunning := prior.Status == StepStatusRunning
	step, err := gq.PoisonDeadLetterRunStep(ctx, gen.PoisonDeadLetterRunStepParams{
		RunID: args.RunID, StepID: args.StepID, Error: args.Error, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The pre-CAS read pins the diagnosis: the step is terminal.
		return gen.RunStep{}, fmt.Errorf("store: %s: %w", op, &TransitionError{
			Entity: "step", RunID: args.RunID, StepID: args.StepID,
			From: prior.Status, To: StepStatusDeadLettered,
			Reason: ConflictWrongStatus, CurrentClaimID: prior.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if wasRunning {
		if err := finishAttempt(ctx, gq, op, step, AttemptOutcomeLost, args.Error, nil, nil, nil, args.Now); err != nil {
			return gen.RunStep{}, err
		}
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DFailed: 1}); err != nil {
		return gen.RunStep{}, err
	}
	dl, err := gq.CreateDeadLetter(ctx, gen.CreateDeadLetterParams{
		RunID: args.RunID, StepID: args.StepID, Source: DeadLetterSourcePoison,
		Error: args.Error, Payload: args.Payload,
		AttemptsAtDeath: step.AttemptCount, CreatedAt: args.Now,
	})
	if err != nil {
		return gen.RunStep{}, wrapErr(op+": insert dead letter", err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepDeadLettered, stepDeadLetteredPayload{
		StepID: args.StepID, Source: DeadLetterSourcePoison,
		Attempts: step.AttemptCount, Seq: dl.Seq,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step dead-lettered by poison",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// CancelStepArgs are the inputs to CancelStep.
type CancelStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Reason records why the step was written off — the step_cancelled
	// event payload (CancelReasonUpstreamDeadLettered for the 5.4
	// write-off, CancelReasonRunCancelled for 5.6's run-cancel sweep).
	// Required.
	Reason string
	// Now is the injected current time. Required.
	Now time.Time
}

// CancelStep transitions a step pending|ready|retrying → cancelled: the
// 5.4 write-off of a step whose readiness a dead-lettered upstream step
// made impossible (always from pending — ADR-006
// continue_independent_branches), and 5.6's run-cancel sweep, which also
// cancels ready and retrying steps — every claimless non-terminal status,
// none of which has an open attempt to close (a retrying step's schedule
// clears with the CAS). Bumps the run's steps_cancelled aggregate and
// appends the step_cancelled event. Dependency counters and the step's
// own out-edges stay untouched — that is what makes ReviveStep a pure
// status flip. Running steps take CancelRunningStep instead.
func CancelStep(ctx context.Context, q Querier, args CancelStepArgs) (gen.RunStep, error) {
	const op = "cancel step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if args.Reason == "" {
		return gen.RunStep{}, fmt.Errorf("store: %s: empty Reason — pass the write-off reason", op)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.CancelRunStep(ctx, gen.CancelRunStepParams{
		RunID: args.RunID, StepID: args.StepID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The CAS guards only on the from-status set, so zero rows always
		// diagnoses wrong_status (the multi-from analogue of stepConflict).
		row, gerr := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.StepID})
		if errors.Is(gerr, pgx.ErrNoRows) {
			return gen.RunStep{}, fmt.Errorf("store: %s: step %q of run %s: %w", op, args.StepID, args.RunID, ErrNotFound)
		}
		if gerr != nil {
			return gen.RunStep{}, wrapErr(op, gerr)
		}
		return gen.RunStep{}, fmt.Errorf("store: %s: %w", op, &TransitionError{
			Entity: "step", RunID: args.RunID, StepID: args.StepID,
			From: row.Status, To: StepStatusCancelled,
			Reason: ConflictWrongStatus, CurrentClaimID: row.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DCancelled: 1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepCancelled, stepCancelledPayload{
		StepID: args.StepID, Reason: args.Reason,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step cancelled",
		log.RunID(args.RunID.String()), log.StepID(args.StepID))
	return step, nil
}

// CancelRunningStepArgs are the inputs to CancelRunningStep.
type CancelRunningStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller.
	ClaimID uuid.UUID
	// Reason records why the step was cancelled — the step_cancelled event
	// payload (CancelReasonRunCancelled). Required.
	Reason string
	// Error records what the executor returned when the cancellation
	// interrupted it, stored on the step and its attempt; nil stores NULL
	// (the executor was cancelled clean, or its result was discarded).
	Error json.RawMessage
	// Now is the injected current time. Required.
	Now time.Time
}

// CancelRunningStep transitions a step running → cancelled, fenced by
// ClaimID (ticket 5.6, ADR-006 taxonomy row 8): the in-flight worker
// noticed its run is cancelling and settles the step as cancelled instead
// of routing the outcome through retry/DLQ — the attempt closes with the
// administrative outcome `cancelled` (never counted against the retry
// budget: nothing was judged about the step's content), the run's
// steps_cancelled aggregate bumps, and the step_cancelled event appends.
// The reconciler's cancelling-run heal composes TakeoverStep + CancelStep
// instead when the worker crashed (attempt then closes `lost`).
func CancelRunningStep(ctx context.Context, q Querier, args CancelRunningStepArgs) (gen.RunStep, error) {
	const op = "cancel running step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if args.Reason == "" {
		return gen.RunStep{}, fmt.Errorf("store: %s: empty Reason — pass the cancel reason", op)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.CancelRunningRunStep(ctx, gen.CancelRunningRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID,
		Error: args.Error, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusCancelled, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := finishAttempt(ctx, gq, op, step, AttemptOutcomeCancelled, args.Error, nil, nil, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DCancelled: 1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepCancelled, stepCancelledPayload{
		StepID: args.StepID, Reason: args.Reason,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "running step cancelled",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// RequeueStepArgs are the inputs to RequeueStep.
type RequeueStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Now is the injected current time. Required.
	Now time.Time
}

// RequeueStep transitions a step dead_lettered → ready (ticket 5.4,
// ADR-006 "Requeue"): error and schedule state clear, the run's
// steps_failed aggregate un-bumps (the step is no longer a terminal
// failure — a cured run must pass the SucceedRun guard), and the
// step_requeued event appends. Attempt history is immutable: the retry
// budget re-arms because CountCountedFailures counts from the latest
// dead_letters baseline, not because anything resets. Re-dispatch (the
// dlq_requeue outbox row) and run revival are the caller's — the engine's
// Requeue op composes them in the same transaction.
func RequeueStep(ctx context.Context, q Querier, args RequeueStepArgs) (gen.RunStep, error) {
	const op = "requeue step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.RequeueRunStep(ctx, gen.RequeueRunStepParams{
		RunID: args.RunID, StepID: args.StepID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusDeadLettered, to: StepStatusReady,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DFailed: -1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepRequeued, stepIDPayload{StepID: args.StepID}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step requeued from DLQ",
		log.RunID(args.RunID.String()), log.StepID(args.StepID))
	return step, nil
}

// ReviveStepArgs are the inputs to ReviveStep.
type ReviveStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Reason records what made the revival possible — the step_revived
	// event payload (dlq_requeue in 5.4). Required.
	Reason string
	// Now is the injected current time. Required.
	Now time.Time
}

// ReviveStep transitions a step cancelled → pending (ticket 5.4): the
// requeue op's undo of a write-off whose blocking dead-lettered step was
// requeued. A pure status flip — the write-off never touched dependency
// counters — plus the steps_cancelled un-bump and the step_revived event.
func ReviveStep(ctx context.Context, q Querier, args ReviveStepArgs) (gen.RunStep, error) {
	const op = "revive step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if args.Reason == "" {
		return gen.RunStep{}, fmt.Errorf("store: %s: empty Reason — pass the revival reason", op)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.ReviveRunStep(ctx, gen.ReviveRunStepParams{
		RunID: args.RunID, StepID: args.StepID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusCancelled, to: StepStatusPending,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DCancelled: -1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepRevived, stepCancelledPayload{
		StepID: args.StepID, Reason: args.Reason,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step revived",
		log.RunID(args.RunID.String()), log.StepID(args.StepID))
	return step, nil
}

// RetryStepArgs are the inputs to RetryStep.
type RetryStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller.
	ClaimID uuid.UUID
	// Outcome is the attempt's ADR-006 error class. Only the counted,
	// retryable classes may route here: transient or timeout.
	Outcome string
	// Error is the failure summary, stored on both the step (last failure)
	// and the attempt; nil stores NULL.
	Error json.RawMessage
	// NextAttemptAt is when the next attempt is due — now plus the computed
	// backoff delay. Required.
	NextAttemptAt time.Time
	// Now is the injected current time. Required.
	Now time.Time
}

// RetryStep transitions a step running → retrying, fenced by ClaimID
// (ticket 5.2, ADR-006 "Step failure lifecycle" — the conceptual `failed`
// routing state is passed through inside this one transaction, never left
// resting): it records the error on the step and closes its attempt row
// with the judged class, clears claim_id (a retrying step holds no claim
// and no lease), stamps next_attempt_at, and appends the
// step_retry_scheduled event. The run's steps_failed aggregate is NOT
// bumped — the step is not terminal, and the rollup guard must still pass
// when the retry eventually succeeds. Scheduling the delayed re-dispatch
// is the caller's post-commit move; a crash before it is healed by the
// reconciler's overdue-retrying scan.
func RetryStep(ctx context.Context, q Querier, args RetryStepArgs) (gen.RunStep, error) {
	const op = "retry step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if args.Outcome != AttemptOutcomeTransient && args.Outcome != AttemptOutcomeTimeout {
		return gen.RunStep{}, fmt.Errorf(
			"store: %s: outcome %q is not a counted retryable class (transient/timeout)", op, args.Outcome)
	}
	if args.NextAttemptAt.IsZero() {
		return gen.RunStep{}, fmt.Errorf("store: %s: zero NextAttemptAt — pass the computed due time", op)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.RetryRunStep(ctx, gen.RetryRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID,
		Error: args.Error, NextAttemptAt: args.NextAttemptAt, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusRetrying, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := finishAttempt(ctx, gq, op, step, args.Outcome, args.Error, nil, nil, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepRetryScheduled, stepRetryPayload{
		StepID: args.StepID, AttemptNo: step.AttemptCount,
		Class: args.Outcome, NextAttemptAt: args.NextAttemptAt,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step retry scheduled",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// ThrottleStepArgs are the inputs to ThrottleStep.
type ThrottleStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller.
	ClaimID uuid.UUID
	// Resource is the ADR-010 resource whose limit denied the step (e.g.
	// "anthropic:claude-sonnet-5"); recorded on the event for diagnostics.
	Resource string
	// Bucket names the denying dimension (requests/tokens/both) for the
	// event.
	Bucket string
	// RetryAfter is the limiter's own computed time-to-tokens, recorded on
	// the event (the pre-jitter, pre-clamp figure); its human string form.
	RetryAfter string
	// NextAttemptAt is when the re-dispatch is due — now plus the clamped,
	// jittered delay the engine computed. Required.
	NextAttemptAt time.Time
	// Now is the injected current time. Required.
	Now time.Time
}

// ThrottleStep routes a rate-limited step running → retrying without
// executing it (ticket 9.2, ADR-010): the fleet-wide limiter denied the
// step's provider call, so it is deferred, not failed. It reuses 5.2's
// retry CAS wholesale (RetryRunStep — the claim-fenced running → retrying
// transition, claim cleared, next_attempt_at stamped) but records the
// attempt with the administrative outcome `throttled` (never a counted
// class, so CountCountedFailures ignores it — the retry budget is
// untouched) and appends step_throttled rather than step_retry_scheduled.
// Like RetryStep it does NOT bump steps_failed — a throttle is not a
// failure. Scheduling the delayed re-dispatch (reason `throttle`) is the
// caller's post-commit move; a crash before it is healed by the same
// overdue-retrying reconciler scan 5.2's retries rely on.
func ThrottleStep(ctx context.Context, q Querier, args ThrottleStepArgs) (gen.RunStep, error) {
	const op = "throttle step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if args.NextAttemptAt.IsZero() {
		return gen.RunStep{}, fmt.Errorf("store: %s: zero NextAttemptAt — pass the computed due time", op)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	// The throttle event's cause is recorded on the step's error column too,
	// so GET /v1/runs shows why a step is waiting; JSON marshaling a small
	// fixed map cannot fail.
	errPayload, _ := json.Marshal(map[string]string{
		"outcome": AttemptOutcomeThrottled, "resource": args.Resource, "bucket": args.Bucket,
	})
	step, err := gq.RetryRunStep(ctx, gen.RetryRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID,
		Error: errPayload, NextAttemptAt: args.NextAttemptAt, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusRetrying, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := finishAttempt(ctx, gq, op, step, AttemptOutcomeThrottled, errPayload, nil, nil, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepThrottled, stepThrottlePayload{
		StepID: args.StepID, AttemptNo: step.AttemptCount, Resource: args.Resource,
		Bucket: args.Bucket, RetryAfter: args.RetryAfter, NextAttemptAt: args.NextAttemptAt,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step throttled",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// ResolveEdgeArgs are the inputs to ResolveEdge.
type ResolveEdgeArgs struct {
	RunID uuid.UUID
	// Ordinal identifies the edge (its position in the definition's edges
	// array).
	Ordinal int32
	// Fired records how the edge resolved: true = fired (source succeeded
	// and its predicate selected this edge), false = skipped. The verdict
	// is the caller's — CEL evaluation and the branch first-match rule are
	// engine logic (M4.3).
	Fired bool
	// Now is the injected current time. Required.
	Now time.Time
}

// ResolveEdgeResult is what ResolveEdge returns.
type ResolveEdgeResult struct {
	// Resolved is false when the edge had already resolved to the same
	// verdict — the idempotent no-op that makes retried completion
	// transactions safe (nothing was updated, counters untouched).
	Resolved bool
	Edge     gen.RunEdge
	// ToStep is the edge's target with its dependency counters updated;
	// zero-valued when Resolved is false. The caller inspects it to decide
	// whether ReadyStep/SkipStep applies next.
	ToStep gen.RunStep
}

// ResolveEdge records how a normal edge resolved and applies the counter
// side to its target step: remaining_deps decrements exactly once, and
// fired_deps increments iff the edge fired (ADR-004 dependency
// bookkeeping). It is not a status transition and appends no event, but it
// follows the same run-lock-first ordering as every other function here.
// Re-resolving to the same verdict is a no-op; a conflicting verdict, a
// loop edge, or a missing edge is an error.
func ResolveEdge(ctx context.Context, q Querier, args ResolveEdgeArgs) (ResolveEdgeResult, error) {
	const op = "resolve edge"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return ResolveEdgeResult{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return ResolveEdgeResult{}, err
	}
	resolution := EdgeResolutionSkipped
	if args.Fired {
		resolution = EdgeResolutionFired
	}
	edge, err := gq.ResolveRunEdge(ctx, gen.ResolveRunEdgeParams{
		RunID: args.RunID, Ordinal: args.Ordinal, Resolution: resolution,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return resolveEdgeNoop(ctx, gq, op, args, resolution)
	}
	if err != nil {
		return ResolveEdgeResult{}, wrapErr(op, err)
	}
	firedDelta := int32(0)
	if args.Fired {
		firedDelta = 1
	}
	toStep, err := gq.ApplyEdgeResolution(ctx, gen.ApplyEdgeResolutionParams{
		RunID: args.RunID, StepID: edge.ToStep, FiredDelta: firedDelta, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Zero rows is a graph-integrity bug either way, never a caller
		// race: the target is missing (no FK guards edge endpoints,
		// ADR-004) or its remaining_deps already drained to 0 while this
		// edge was still unresolved.
		if _, gerr := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: edge.ToStep}); gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return ResolveEdgeResult{}, fmt.Errorf("store: %s: target step %q of edge %d: %w",
					op, edge.ToStep, args.Ordinal, ErrNotFound)
			}
			return ResolveEdgeResult{}, wrapErr(op, gerr)
		}
		return ResolveEdgeResult{}, fmt.Errorf(
			"store: %s: target step %q of edge %d has remaining_deps 0 with this edge unresolved — dependency bookkeeping corrupted",
			op, edge.ToStep, args.Ordinal)
	}
	if err != nil {
		return ResolveEdgeResult{}, wrapErr(op+": apply counters", err)
	}
	return ResolveEdgeResult{Resolved: true, Edge: edge, ToStep: toStep}, nil
}

// resolveEdgeNoop diagnoses a ResolveRunEdge that matched nothing: absent
// edge, loop edge, idempotent re-resolution, or a conflicting verdict.
func resolveEdgeNoop(ctx context.Context, gq *gen.Queries, op string, args ResolveEdgeArgs, resolution string) (ResolveEdgeResult, error) {
	edge, err := gq.GetRunEdge(ctx, gen.GetRunEdgeParams{RunID: args.RunID, Ordinal: args.Ordinal})
	if err != nil {
		return ResolveEdgeResult{}, wrapErr(op, err)
	}
	switch {
	case edge.EdgeType == EdgeTypeLoop:
		return ResolveEdgeResult{}, fmt.Errorf(
			"store: %s: edge %d is a loop edge — loop edges never resolve (iteration accounting is expansion's, M14)",
			op, args.Ordinal)
	case edge.Resolution == resolution:
		return ResolveEdgeResult{Edge: edge}, nil
	default:
		// A retried transaction must re-derive the same verdict; a
		// different one means non-deterministic edge evaluation upstream.
		return ResolveEdgeResult{}, fmt.Errorf(
			"store: %s: edge %d already resolved %q, cannot re-resolve %q",
			op, args.Ordinal, edge.Resolution, resolution)
	}
}

// ReadyStepArgs are the inputs to ReadyStep.
type ReadyStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// JoinAny is true when the step is a `join any` step (per its
	// definition config, which the caller holds): readiness then requires
	// only one fired edge, not full resolution. False for everything else.
	JoinAny bool
	// Now is the injected current time. Required.
	Now time.Time
}

// ReadyStep transitions a step pending → ready when the ADR-004 counter
// guard holds — fired_deps ≥ 1 and (remaining_deps = 0 or JoinAny) — and
// appends the step_ready event. It does not write the outbox row: dispatch
// is composed by the caller (M4.3), matching instantiation (2.5), unpark
// (M5.6), and the reconciler (M4.4), which all outbox ready steps outside
// any transition. Later firings into an already-ready `join any` step are
// absorbed as ErrConflict (wrong_status) — the caller drops them, which is
// safe inside a composed transaction: a rejected transition writes nothing
// (see the package comment).
func ReadyStep(ctx context.Context, q Querier, args ReadyStepArgs) (gen.RunStep, error) {
	const op = "ready step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.ReadyRunStep(ctx, gen.ReadyRunStepParams{
		RunID: args.RunID, StepID: args.StepID, JoinAny: args.JoinAny, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusPending, to: StepStatusReady,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepReady, stepIDPayload{StepID: args.StepID}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step ready",
		log.RunID(args.RunID.String()), log.StepID(args.StepID))
	return step, nil
}

// SkipStepArgs are the inputs to SkipStep.
type SkipStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Now is the injected current time. Required.
	Now time.Time
}

// SkipStep transitions a step pending → skipped when every incoming normal
// edge resolved and none fired (skip propagation), bumps the run's
// steps_skipped aggregate, and appends the step_skipped event. Resolving
// the skipped step's own out-edges (all skipped) is the caller's next
// move, propagating the skip downstream.
func SkipStep(ctx context.Context, q Querier, args SkipStepArgs) (gen.RunStep, error) {
	const op = "skip step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.SkipRunStep(ctx, gen.SkipRunStepParams{
		RunID: args.RunID, StepID: args.StepID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusPending, to: StepStatusSkipped,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DSkipped: 1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepSkipped, stepIDPayload{StepID: args.StepID}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step skipped",
		log.RunID(args.RunID.String()), log.StepID(args.StepID))
	return step, nil
}

// SucceedRunArgs are the inputs to SucceedRun.
type SucceedRunArgs struct {
	RunID uuid.UUID
	// Now is the injected current time. Required.
	Now time.Time
}

// SucceedRun transitions a run running → succeeded when every step is
// terminal and none failed (steps_succeeded + steps_skipped = steps_total
// and steps_failed = 0, per the aggregates the step transitions maintain),
// and appends the run_succeeded event. Calling it before the guard holds
// returns ErrConflict (guard_failed) — the completion transaction simply
// attempts it after its last step lands and drops the conflict.
func SucceedRun(ctx context.Context, q Querier, args SucceedRunArgs) (gen.Run, error) {
	const op = "succeed run"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Run{}, err
	}
	run, err := gq.SucceedRun(ctx, gen.SucceedRunParams{RunID: args.RunID, Now: args.Now})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusRunning, RunStatusSucceeded)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunSucceeded, struct{}{}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).DebugContext(ctx, "run succeeded", log.RunID(args.RunID.String()))
	return run, nil
}

// FailRunArgs are the inputs to FailRun and FailRunRollup.
type FailRunArgs struct {
	RunID uuid.UUID
	// Now is the injected current time. Required.
	Now time.Time
}

// FailRun transitions a run running → failed immediately — the fail_fast
// disposition (ADR-006): the guard requires only that some step failed
// terminally. Appends the run_failed event.
func FailRun(ctx context.Context, q Querier, args FailRunArgs) (gen.Run, error) {
	const op = "fail run"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Run{}, err
	}
	run, err := gq.FailRun(ctx, gen.FailRunParams{RunID: args.RunID, Now: args.Now})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusRunning, RunStatusFailed)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunFailed, struct{}{}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).DebugContext(ctx, "run failed", log.RunID(args.RunID.String()))
	return run, nil
}

// FailRunRollup transitions a run running → failed once every step is
// terminal and at least one failed terminally (ticket 5.4, ADR-006
// continue_independent_branches: succeeded + failed + skipped + cancelled
// = total). Completion transactions attempt it after SucceedRun's guard
// rejects and drop the conflict, exactly like SucceedRun — the guard
// passes exactly once, on the transaction that terminalizes the last step
// of a partially-failed run. Appends the run_failed event.
func FailRunRollup(ctx context.Context, q Querier, args FailRunArgs) (gen.Run, error) {
	const op = "fail run rollup"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Run{}, err
	}
	run, err := gq.FailRunRollup(ctx, gen.FailRunRollupParams{RunID: args.RunID, Now: args.Now})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusRunning, RunStatusFailed)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunFailed, struct{}{}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).DebugContext(ctx, "run failed (rollup)", log.RunID(args.RunID.String()))
	return run, nil
}

// ResumeRunArgs are the inputs to ResumeRun.
type ResumeRunArgs struct {
	RunID uuid.UUID
	// Now is the injected current time. Required.
	Now time.Time
}

// ResumeRun transitions a run failed → running (ticket 5.4): the requeue
// op re-opens a failed run so the claim path admits its steps again.
// finished_at clears and the run_resumed event appends. M5.6's unpark is
// a different transition — this one exists solely for DLQ requeue.
func ResumeRun(ctx context.Context, q Querier, args ResumeRunArgs) (gen.Run, error) {
	const op = "resume run"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Run{}, err
	}
	run, err := gq.ResumeRun(ctx, args.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusFailed, RunStatusRunning)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunResumed, struct{}{}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).DebugContext(ctx, "run resumed", log.RunID(args.RunID.String()))
	return run, nil
}

// runReasonPayload is the event body of the reasoned run transitions
// (ticket 5.6): run_parked and run_cancelling.
type runReasonPayload struct {
	Reason string `json:"reason"`
}

// ParkRunArgs are the inputs to ParkRun.
type ParkRunArgs struct {
	RunID uuid.UUID
	// Reason is the typed park reason: ParkReasonManual (5.6); the
	// budget_exceeded and awaiting_human values are reserved for M10/M15.
	// Required.
	Reason string
	// Now is the injected current time. Required.
	Now time.Time
}

// ParkRun transitions a run running → parked (ticket 5.6): dispatch
// pauses — the claim path refuses the run's steps — while in-flight steps
// keep executing and settle normally (their deliveries were claimed
// before the park; completions carry no run-status guard). Appends the
// run_parked event with the typed reason.
func ParkRun(ctx context.Context, q Querier, args ParkRunArgs) (gen.Run, error) {
	const op = "park run"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	switch args.Reason {
	case ParkReasonManual, ParkReasonBudgetExceeded, ParkReasonAwaitingHuman:
	default:
		return gen.Run{}, fmt.Errorf("store: %s: unknown park reason %q", op, args.Reason)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Run{}, err
	}
	run, err := gq.ParkRun(ctx, gen.ParkRunParams{RunID: args.RunID, Reason: &args.Reason})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusRunning, RunStatusParked)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunParked, runReasonPayload{Reason: args.Reason}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).DebugContext(ctx, "run parked", log.RunID(args.RunID.String()))
	return run, nil
}

// UnparkRunArgs are the inputs to UnparkRun.
type UnparkRunArgs struct {
	RunID uuid.UUID
	// Now is the injected current time. Required.
	Now time.Time
}

// UnparkRun transitions a run parked → running (ticket 5.6), clearing the
// park reason and appending the run_unparked event. Re-outboxing the
// run's ready steps — their deliveries were consumed by the run-status
// guard while parked — is the caller's move, in the same transaction (the
// engine's Unpark op composes it).
func UnparkRun(ctx context.Context, q Querier, args UnparkRunArgs) (gen.Run, error) {
	const op = "unpark run"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Run{}, err
	}
	run, err := gq.UnparkRun(ctx, args.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusParked, RunStatusRunning)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunUnparked, struct{}{}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).DebugContext(ctx, "run unparked", log.RunID(args.RunID.String()))
	return run, nil
}

// CancelRunArgs are the inputs to CancelRun.
type CancelRunArgs struct {
	RunID uuid.UUID
	// Reason is the typed cancel reason: RunCancelReasonManual or
	// RunCancelReasonDeadlineExceeded. Required.
	Reason string
	// Now is the injected current time. Required.
	Now time.Time
}

// CancelRun transitions a run running|parked → cancelling (ticket 5.6):
// the cooperative cancel request. The claim path refuses the run's steps
// from here on; sweeping the claimless non-terminal steps to cancelled
// and attempting the finalization rollup are the caller's next moves,
// inside the same transaction (the engine's cancel op composes them).
// Appends the run_cancelling event with the typed reason.
func CancelRun(ctx context.Context, q Querier, args CancelRunArgs) (gen.Run, error) {
	const op = "cancel run"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	switch args.Reason {
	case RunCancelReasonManual, RunCancelReasonDeadlineExceeded:
	default:
		return gen.Run{}, fmt.Errorf("store: %s: unknown cancel reason %q", op, args.Reason)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Run{}, err
	}
	run, err := gq.CancelRun(ctx, gen.CancelRunParams{RunID: args.RunID, Reason: &args.Reason})
	if errors.Is(err, pgx.ErrNoRows) {
		// The CAS guards only on the from-status set {running, parked}, so
		// zero rows always diagnoses wrong_status; From carries the actual
		// status (terminal, or already cancelling).
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusRunning, RunStatusCancelling)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunCancelling, runReasonPayload{Reason: args.Reason}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).DebugContext(ctx, "run cancelling", log.RunID(args.RunID.String()))
	return run, nil
}

// CancelRunRollup transitions a run cancelling → cancelled once every
// step is terminal (ticket 5.6) — the cancel finalizer, same shape as
// FailRunRollup: attempted (and its conflict dropped) by the cancel
// request itself and by every transaction that terminalizes a step of a
// possibly-cancelling run; the guard passes exactly once. Appends the
// run_cancelled event.
func CancelRunRollup(ctx context.Context, q Querier, args FailRunArgs) (gen.Run, error) {
	const op = "cancel run rollup"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Run{}, err
	}
	run, err := gq.CancelRunRollup(ctx, gen.CancelRunRollupParams{RunID: args.RunID, Now: args.Now})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusCancelling, RunStatusCancelled)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunCancelled, struct{}{}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).DebugContext(ctx, "run cancelled (rollup)", log.RunID(args.RunID.String()))
	return run, nil
}

// LockRunStatus acquires the run-row lock and returns the run's current
// status — the engine's in-transaction run-status check (ticket 5.6): a
// completion transaction reads it first to decide between the normal
// completion and the cancel settlement, serialized against CancelRun/
// ParkRun by the same lock every transition takes first. Must run inside
// WithTx, like every transition.
func LockRunStatus(ctx context.Context, q Querier, runID uuid.UUID, now time.Time) (string, error) {
	const op = "lock run status"
	gq, err := transitionQueries(ctx, q, op, now)
	if err != nil {
		return "", err
	}
	row, err := lockRun(ctx, gq, op, runID)
	if err != nil {
		return "", err
	}
	return row.Status, nil
}

// transitionQueries validates a transition call and unwraps the generated
// queries from the WithTx Querier. Only the handle WithTx passes its
// callback qualifies: it is the sole guarantee the transition's statements
// share one transaction. It also enforces the injected-clock invariant.
func transitionQueries(ctx context.Context, q Querier, op string, now time.Time) (*gen.Queries, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("store: %s: zero Now — pass the injected current time", op)
	}
	if ctx.Value(txMarker{}) == nil {
		return nil, fmt.Errorf("store: %s: %w", op, ErrNoTx)
	}
	r, ok := q.(repos)
	if !ok {
		return nil, fmt.Errorf("store: %s: %w", op, ErrNoTx)
	}
	return r.q, nil
}

// lockRun acquires the run-row lock — the first statement of every
// transition (uniform run → step → edge ordering; see the package
// comment) — and surfaces a missing run as ErrNotFound before any other
// work. A FOR UPDATE read, not a write: a transition rejected after it
// leaves no trace. The returned row carries the run's status for the
// transitions that guard on it (ClaimStep since 5.2); everyone else
// ignores it.
func lockRun(ctx context.Context, gq *gen.Queries, op string, runID uuid.UUID) (gen.LockRunRow, error) {
	row, err := gq.LockRun(ctx, runID)
	if err != nil {
		return gen.LockRunRow{}, wrapErr(op+": lock run", err)
	}
	return row, nil
}

// appendEvent allocates the transition's event seq and writes the event
// row. Called only after the guarded CAS succeeded, so next_seq advances
// iff an event lands under it — gap-free even when callers swallow
// conflicts from other transitions in the same transaction.
func appendEvent(ctx context.Context, gq *gen.Queries, op string, runID uuid.UUID, typ string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: %s: marshaling %s payload: %w", op, typ, err)
	}
	seq, err := gq.AllocateEventSeq(ctx, runID)
	if err != nil {
		return wrapErr(op+": allocate event seq", err)
	}
	_, err = gq.AppendEvent(ctx, gen.AppendEventParams{RunID: runID, Seq: seq, Type: typ, Payload: body})
	return wrapErr(op+": append event", err)
}

// finishAttempt closes the step's current attempt row with its outcome.
// The attempt must exist — ClaimStep created it — so zero rows is a
// data-integrity error, not a race. usage is the provider's token
// accounting on a successful llm attempt (ticket 8.6); verdict is the
// output-validation chain verdict (ticket 11.1); repair is the
// structured-output provenance (ticket 11.3); all nil for every outcome and
// step type that has none.
func finishAttempt(ctx context.Context, gq *gen.Queries, op string, step gen.RunStep, outcome string, errPayload, usage, verdict, repair json.RawMessage, now time.Time) error {
	rows, err := gq.FinishStepAttempt(ctx, gen.FinishStepAttemptParams{
		RunID: step.RunID, StepID: step.StepID, AttemptNo: step.AttemptCount,
		Outcome: &outcome, Error: errPayload, Usage: usage, Verdict: verdict, Repair: repair, FinishedAt: now,
	})
	if err != nil {
		return wrapErr(op+": finish attempt", err)
	}
	if rows != 1 {
		return fmt.Errorf("store: %s: attempt %d of step %q: %w", op, step.AttemptCount, step.StepID, ErrNotFound)
	}
	return nil
}

// bumpCounters applies a step transition's aggregate delta to the run row.
// The row is known to exist (lockRun found it), so zero rows cannot
// happen short of a concurrent delete, which the row lock excludes.
func bumpCounters(ctx context.Context, gq *gen.Queries, op string, runID uuid.UUID, params gen.BumpRunStepCountersParams) error {
	params.RunID = runID
	rows, err := gq.BumpRunStepCounters(ctx, params)
	if err != nil {
		return wrapErr(op+": bump run counters", err)
	}
	if rows != 1 {
		return fmt.Errorf("store: %s: bump run counters: run %s: %w", op, runID, ErrNotFound)
	}
	return nil
}

// stepConflictArgs parameterize the diagnosis of a rejected step CAS.
type stepConflictArgs struct {
	// want is the transition's required from-status; to its target.
	want, to string
	// claim, when set, is the fencing token the caller presented (the
	// transition was claim-guarded).
	claim *uuid.UUID
}

// stepConflict re-reads a step whose conditional UPDATE matched nothing
// and returns the typed rejection: ErrNotFound for a missing row,
// otherwise a *TransitionError classifying wrong status, claim mismatch,
// or a failed guard predicate. The re-read runs on the same transaction;
// the caller's error return rolls everything back, which is correct — the
// transition failed.
func stepConflict(ctx context.Context, gq *gen.Queries, op string, runID uuid.UUID, stepID string, args stepConflictArgs) error {
	row, err := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: runID, StepID: stepID})
	if err != nil {
		return wrapErr(op, err)
	}
	// The row's claim at rejection time is always reported: a rejected
	// ClaimStep on a running step reads it as the observed holder to fence
	// the 4.5 takeover on, and a fenced completion logs it as the claim
	// that displaced the caller's. CallerClaimID only exists when the
	// transition was claim-guarded.
	te := &TransitionError{
		Entity: "step", RunID: runID, StepID: stepID, From: row.Status, To: args.to,
		CallerClaimID: args.claim, CurrentClaimID: row.ClaimID,
	}
	switch {
	case row.Status != args.want:
		te.Reason = ConflictWrongStatus
	case args.claim != nil && (row.ClaimID == nil || *row.ClaimID != *args.claim):
		te.Reason = ConflictClaimMismatch
	default:
		te.Reason = ConflictGuardFailed
	}
	return fmt.Errorf("store: %s: %w", op, te)
}

// runConflict is stepConflict for run transitions: no claim guard,
// counter guards diagnosed as guard_failed when the from-status matched.
func runConflict(ctx context.Context, gq *gen.Queries, op string, runID uuid.UUID, want, to string) error {
	row, err := gq.GetRun(ctx, runID)
	if err != nil {
		return wrapErr(op, err)
	}
	te := &TransitionError{Entity: "run", RunID: runID, From: row.Status, To: to}
	if row.Status != want {
		te.Reason = ConflictWrongStatus
	} else {
		te.Reason = ConflictGuardFailed
	}
	return fmt.Errorf("store: %s: %w", op, te)
}
