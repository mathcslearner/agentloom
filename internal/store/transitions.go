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

// ClaimStepArgs are the inputs to ClaimStep.
type ClaimStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Now is the injected current time. Required.
	Now time.Time
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
	const op = "claim step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	run, err := lockRun(ctx, gq, op, args.RunID)
	if err != nil {
		return gen.RunStep{}, err
	}
	if run.Status != RunStatusRunning {
		step, gerr := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.StepID})
		if gerr != nil {
			return gen.RunStep{}, wrapErr(op, gerr)
		}
		return gen.RunStep{}, fmt.Errorf("store: %s: %w", op, &TransitionError{
			Entity: "step", RunID: args.RunID, StepID: args.StepID,
			From: run.Status, To: StepStatusRunning,
			Reason: ConflictRunNotRunning, CurrentClaimID: step.ClaimID,
		})
	}
	claimID := uuid.New()
	step, err := gq.ClaimRunStep(ctx, gen.ClaimRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &claimID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusReady, to: StepStatusRunning,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	_, err = gq.CreateStepAttempt(ctx, gen.CreateStepAttemptParams{
		RunID: args.RunID, StepID: args.StepID, AttemptNo: step.AttemptCount,
		ClaimID: claimID, StartedAt: &args.Now,
	})
	if err != nil {
		return gen.RunStep{}, wrapErr(op+": insert attempt", err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepClaimed, stepClaimedPayload{
		StepID: args.StepID, ClaimID: claimID.String(), AttemptNo: step.AttemptCount,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step claimed",
		log.RunID(args.RunID.String()), log.StepID(args.StepID), log.Attempt(int(step.AttemptCount)))
	return step, nil
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
	if err := finishAttempt(ctx, gq, op, step, AttemptOutcomeLost, nil, args.Now); err != nil {
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
	if err := finishAttempt(ctx, gq, op, step, StepStatusSucceeded, nil, args.Now); err != nil {
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

// FailStepArgs are the inputs to FailStep.
type FailStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller.
	ClaimID uuid.UUID
	// Outcome is the attempt's ADR-006 error class (transient / permanent /
	// timeout / cancelled) — what the judged failure *was*, even when the
	// disposition is terminal (an exhausted transient records `transient`).
	// Required; the retired bare `failed` is rejected by the schema CHECK.
	Outcome string
	// Error is the failure summary, stored on both the step (last failure)
	// and the attempt; nil stores NULL.
	Error json.RawMessage
	// Now is the injected current time. Required.
	Now time.Time
}

// FailStep transitions a step running → failed, fenced by ClaimID: it
// records the error on the step and its attempt row (outcome = the ADR-006
// class), bumps the run's steps_failed aggregate, and appends the
// step_failed event. Since 5.2 this is the *terminal* failure path —
// budget exhausted or class not retryable; retryable failures route
// through RetryStep instead. The failed step's out-edges stay unresolved
// (ADR-004), blocking dependents until DLQ semantics arrive (5.4).
func FailStep(ctx context.Context, q Querier, args FailStepArgs) (gen.RunStep, error) {
	const op = "fail step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if args.Outcome == "" {
		return gen.RunStep{}, fmt.Errorf("store: %s: empty Outcome — pass the attempt's ADR-006 error class", op)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.FailRunStep(ctx, gen.FailRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID,
		Error: args.Error, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusFailed, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := finishAttempt(ctx, gq, op, step, args.Outcome, args.Error, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DFailed: 1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepFailed, stepFinishedPayload{
		StepID: args.StepID, AttemptNo: step.AttemptCount,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step failed",
		log.RunID(args.RunID.String()), log.StepID(args.StepID), log.Attempt(int(step.AttemptCount)))
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
	if err := finishAttempt(ctx, gq, op, step, args.Outcome, args.Error, args.Now); err != nil {
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
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusSucceeded)
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

// FailRunArgs are the inputs to FailRun.
type FailRunArgs struct {
	RunID uuid.UUID
	// Now is the injected current time. Required.
	Now time.Time
}

// FailRun transitions a run running → failed. The guard is the v1 minimum
// — at least one step failed; *when* a step failure halts a run is
// workflow failure policy (ADR-006, M5). Appends the run_failed event.
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
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusFailed)
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
// data-integrity error, not a race.
func finishAttempt(ctx context.Context, gq *gen.Queries, op string, step gen.RunStep, outcome string, errPayload json.RawMessage, now time.Time) error {
	rows, err := gq.FinishStepAttempt(ctx, gen.FinishStepAttemptParams{
		RunID: step.RunID, StepID: step.StepID, AttemptNo: step.AttemptCount,
		Outcome: &outcome, Error: errPayload, FinishedAt: now,
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

// runConflict is stepConflict for run transitions: from-status running,
// no claim guard, counter guards diagnosed as guard_failed.
func runConflict(ctx context.Context, gq *gen.Queries, op string, runID uuid.UUID, to string) error {
	row, err := gq.GetRun(ctx, runID)
	if err != nil {
		return wrapErr(op, err)
	}
	te := &TransitionError{Entity: "run", RunID: runID, From: row.Status, To: to}
	if row.Status != RunStatusRunning {
		te.Reason = ConflictWrongStatus
	} else {
		te.Reason = ConflictGuardFailed
	}
	return fmt.Errorf("store: %s: %w", op, te)
}
