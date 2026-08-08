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
// transition's first statement is its event-seq allocation, an UPDATE on
// the run row — so all per-run transitions acquire the run-row lock first
// and then step/edge rows, making composed transactions deadlock-free by
// uniform ordering (the per-run serialization this implies is an accepted
// ADR-004 trade).

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
type stepClaimedPayload struct {
	StepID    string `json:"step_id"`
	ClaimID   string `json:"claim_id"`
	AttemptNo int32  `json:"attempt_no"`
}

type stepFinishedPayload struct {
	StepID    string `json:"step_id"`
	AttemptNo int32  `json:"attempt_no"`
}

// ClaimStepArgs are the inputs to ClaimStep.
type ClaimStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Now is the injected current time. Required.
	Now time.Time
}

// ClaimStep transitions a step ready → running: it stamps a fresh claim_id
// (the fencing token, carried on the returned row), increments the durable
// attempt counter, inserts the step_attempts row, and appends the
// step_claimed event. Exactly one of N racing claimers wins; the rest get
// ErrConflict — the substrate that turns at-least-once delivery into
// effectively-once execution (losers ACK-and-drop).
func ClaimStep(ctx context.Context, q Querier, args ClaimStepArgs) (gen.RunStep, error) {
	const op = "claim step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	seq, err := allocateSeq(ctx, gq, op, args.RunID)
	if err != nil {
		return gen.RunStep{}, err
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
	if err := appendEvent(ctx, gq, op, args.RunID, seq, EventStepClaimed, stepClaimedPayload{
		StepID: args.StepID, ClaimID: claimID.String(), AttemptNo: step.AttemptCount,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step claimed",
		log.RunID(args.RunID.String()), log.StepID(args.StepID), log.Attempt(int(step.AttemptCount)))
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
	seq, err := allocateSeq(ctx, gq, op, args.RunID)
	if err != nil {
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
	if err := appendEvent(ctx, gq, op, args.RunID, seq, EventStepSucceeded, stepFinishedPayload{
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
	// Error is the failure summary, stored on both the step (last failure)
	// and the attempt; nil stores NULL.
	Error json.RawMessage
	// Now is the injected current time. Required.
	Now time.Time
}

// FailStep transitions a step running → failed, fenced by ClaimID: it
// records the error on the step and its attempt row, bumps the run's
// steps_failed aggregate, and appends the step_failed event. The failed
// step's out-edges stay unresolved (ADR-004), permanently blocking
// dependents until retry/DLQ semantics arrive (M5).
func FailStep(ctx context.Context, q Querier, args FailStepArgs) (gen.RunStep, error) {
	const op = "fail step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	seq, err := allocateSeq(ctx, gq, op, args.RunID)
	if err != nil {
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
	if err := finishAttempt(ctx, gq, op, step, StepStatusFailed, args.Error, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DFailed: 1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, seq, EventStepFailed, stepFinishedPayload{
		StepID: args.StepID, AttemptNo: step.AttemptCount,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "step failed",
		log.RunID(args.RunID.String()), log.StepID(args.StepID), log.Attempt(int(step.AttemptCount)))
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
// bookkeeping). It is not a status transition and appends no event.
// Re-resolving to the same verdict is a no-op; a conflicting verdict, a
// loop edge, or a missing edge is an error.
func ResolveEdge(ctx context.Context, q Querier, args ResolveEdgeArgs) (ResolveEdgeResult, error) {
	const op = "resolve edge"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
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
		// No FK guards edge endpoints (ADR-004); a dangling target is a
		// graph-integrity bug, not a caller race.
		return ResolveEdgeResult{}, fmt.Errorf("store: %s: target step %q of edge %d: %w",
			op, edge.ToStep, args.Ordinal, ErrNotFound)
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
// absorbed as ErrConflict (wrong_status) — the caller drops them.
func ReadyStep(ctx context.Context, q Querier, args ReadyStepArgs) (gen.RunStep, error) {
	const op = "ready step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	seq, err := allocateSeq(ctx, gq, op, args.RunID)
	if err != nil {
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
	if err := appendEvent(ctx, gq, op, args.RunID, seq, EventStepReady, stepIDPayload{StepID: args.StepID}); err != nil {
		return gen.RunStep{}, err
	}
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
	seq, err := allocateSeq(ctx, gq, op, args.RunID)
	if err != nil {
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
	if err := appendEvent(ctx, gq, op, args.RunID, seq, EventStepSkipped, stepIDPayload{StepID: args.StepID}); err != nil {
		return gen.RunStep{}, err
	}
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
	seq, err := allocateSeq(ctx, gq, op, args.RunID)
	if err != nil {
		return gen.Run{}, err
	}
	run, err := gq.SucceedRun(ctx, gen.SucceedRunParams{RunID: args.RunID, Now: args.Now})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusSucceeded)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, seq, EventRunSucceeded, struct{}{}); err != nil {
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
	seq, err := allocateSeq(ctx, gq, op, args.RunID)
	if err != nil {
		return gen.Run{}, err
	}
	run, err := gq.FailRun(ctx, gen.FailRunParams{RunID: args.RunID, Now: args.Now})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Run{}, runConflict(ctx, gq, op, args.RunID, RunStatusFailed)
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, seq, EventRunFailed, struct{}{}); err != nil {
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

// allocateSeq reserves the transition's event sequence number. Being an
// UPDATE on the run row, it doubles as the run-row lock acquisition every
// transition performs first (see the package comment on lock ordering) and
// surfaces a missing run as ErrNotFound before any other work.
func allocateSeq(ctx context.Context, gq *gen.Queries, op string, runID uuid.UUID) (int64, error) {
	seq, err := gq.AllocateEventSeq(ctx, runID)
	if err != nil {
		return 0, wrapErr(op+": allocate event seq", err)
	}
	return seq, nil
}

// appendEvent writes the transition's event row under a pre-allocated seq.
func appendEvent(ctx context.Context, gq *gen.Queries, op string, runID uuid.UUID, seq int64, typ string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: %s: marshaling %s payload: %w", op, typ, err)
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
// The row is known to exist (allocateSeq found it), so zero rows cannot
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
	te := &TransitionError{Entity: "step", RunID: runID, StepID: stepID, From: row.Status, To: args.to}
	switch {
	case row.Status != args.want:
		te.Reason = ConflictWrongStatus
	case args.claim != nil && (row.ClaimID == nil || *row.ClaimID != *args.claim):
		te.Reason = ConflictClaimMismatch
		te.CallerClaimID = args.claim
		te.CurrentClaimID = row.ClaimID
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
