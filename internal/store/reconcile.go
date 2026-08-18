package store

// Reconciler read surface (ticket 4.4): the periodic healer's scans and
// its fleet-wide sweep lock. Everything here is read-only diagnostics over
// durable state — the heals the reconciler composes from these reads
// (Outbox().Create re-outboxes, and since 4.5 the TakeoverStep transition)
// live in repos.go/transitions.go. All functions must run inside a WithTx
// callback: the staleness scan and the heal it feeds are only coherent as
// one atomic sweep, and an advisory *xact* lock outside a transaction is
// meaningless. Anything else fails fast with ErrNoTx, mirroring the
// transition functions.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// reconcileLockKey is the advisory-lock key serializing reconciler sweeps
// fleet-wide (pg_try_advisory_xact_lock). The value is an arbitrary
// project-reserved constant ("aglmrcn1" as ASCII); it only has to be
// stable and never shared with another advisory-lock use in this database.
const reconcileLockKey int64 = 0x61676c6d_72636e31

// StepRef identifies one step of one run — the reconciler's scan results.
type StepRef struct {
	RunID  uuid.UUID
	StepID string
	// UpdatedAt is the step's last transition time — how stale it is.
	UpdatedAt time.Time
}

// StaleRunningStep is a running step whose updated_at exceeded the
// reconciler's threshold — ADR-005 R1(c), healed by takeover + re-outbox
// (ticket 4.5).
type StaleRunningStep struct {
	StepRef
	// ClaimID is the (presumed dead) holder's fencing token — the observed
	// claim the takeover CAS is fenced on.
	ClaimID *uuid.UUID
	// HasPendingOutbox reports an undrained task_outbox row for this step
	// (the P1 shape sustained past the threshold). The healer still takes
	// the step over but skips its re-outbox: the pending row already
	// carries the dispatch (post-M4 audit).
	HasPendingOutbox bool
}

// reconcileQueries is transitionQueries minus the clock requirement: the
// in-transaction guard shared by the reconciler reads.
func reconcileQueries(ctx context.Context, q Querier, op string) (*gen.Queries, error) {
	if ctx.Value(txMarker{}) == nil {
		return nil, fmt.Errorf("store: %s: %w", op, ErrNoTx)
	}
	r, ok := q.(repos)
	if !ok {
		return nil, fmt.Errorf("store: %s: %w", op, ErrNoTx)
	}
	return r.q, nil
}

// TryReconcileLock attempts the fleet-wide reconciler sweep lock, held
// until the surrounding transaction ends. False means another worker's
// sweep is in flight — the caller skips its own sweep instead of queueing
// behind the winner (rate-bounding: at most one sweep at a time, ever).
func TryReconcileLock(ctx context.Context, q Querier) (bool, error) {
	const op = "try reconcile lock"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return false, err
	}
	acquired, err := gq.TryAdvisoryXactLock(ctx, reconcileLockKey)
	return acquired, wrapErr(op, err)
}

// ListStaleReadySteps returns up to limit steps stuck in ready since
// before staleBefore with no pending task_outbox row — steps whose
// dispatch was lost (ADR-005 P2/R1(a)) and that need a re-outbox. The
// anti-join makes repeated sweeps idempotent: once re-outboxed, a step
// stops matching until the new row is drained.
func ListStaleReadySteps(ctx context.Context, q Querier, staleBefore time.Time, limit int32) ([]StepRef, error) {
	const op = "list stale ready steps"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return nil, err
	}
	rows, err := gq.ListStaleReadySteps(ctx, gen.ListStaleReadyStepsParams{
		StaleBefore: staleBefore, RowLimit: limit,
	})
	if err != nil {
		return nil, wrapErr(op, err)
	}
	refs := make([]StepRef, len(rows))
	for i, r := range rows {
		refs[i] = StepRef{RunID: r.RunID, StepID: r.StepID, UpdatedAt: r.UpdatedAt}
	}
	return refs, nil
}

// ListStaleRunningSteps returns up to limit steps running since before
// staleBefore — dead-worker suspects with no reclaimable lease (ADR-005
// R1(c)). The threshold must be ≫ the lease TTL: updated_at moves on
// transitions, not heartbeats, so a healthy long-running step looks stale
// on any tighter bound — the threshold is effectively a cap on step
// wall-clock execution time. The reconciler heals each hit with
// TakeoverStep + a reconcile_running outbox row (ticket 4.5); a false
// positive keeps state correct — the live holder's completion is fenced —
// but re-runs the step's side effects, so "safe" here means durable
// state, not effect-once (that is M5.5's side-effect journal).
func ListStaleRunningSteps(ctx context.Context, q Querier, staleBefore time.Time, limit int32) ([]StaleRunningStep, error) {
	const op = "list stale running steps"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return nil, err
	}
	rows, err := gq.ListStaleRunningSteps(ctx, gen.ListStaleRunningStepsParams{
		StaleBefore: staleBefore, RowLimit: limit,
	})
	if err != nil {
		return nil, wrapErr(op, err)
	}
	steps := make([]StaleRunningStep, len(rows))
	for i, r := range rows {
		steps[i] = StaleRunningStep{
			StepRef:          StepRef{RunID: r.RunID, StepID: r.StepID, UpdatedAt: r.UpdatedAt},
			ClaimID:          r.ClaimID,
			HasPendingOutbox: r.HasPendingOutbox,
		}
	}
	return steps, nil
}

// OverdueRetryingStep is a retrying step whose next_attempt_at passed the
// reconciler's threshold with no pending outbox row — the failure-commit/
// delayed-schedule crash gap (ticket 5.2, ADR-006).
type OverdueRetryingStep struct {
	RunID  uuid.UUID
	StepID string
	// NextAttemptAt is when the attempt was due — how overdue it is.
	NextAttemptAt time.Time
}

// ListOverdueRetryingSteps returns up to limit steps in retrying whose
// next_attempt_at is before staleBefore with no pending task_outbox row —
// steps whose delayed re-dispatch was lost (the worker died, or its
// Schedule call failed, between the retry commit and the delayed-ZSET
// write). The anti-join makes repeated sweeps idempotent, exactly like
// ListStaleReadySteps. The heal is an outbox row alone (reason
// reconcile_retry): the step stays retrying, and the claim CAS accepts a
// due retrying step directly.
func ListOverdueRetryingSteps(ctx context.Context, q Querier, staleBefore time.Time, limit int32) ([]OverdueRetryingStep, error) {
	const op = "list overdue retrying steps"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return nil, err
	}
	rows, err := gq.ListOverdueRetryingSteps(ctx, gen.ListOverdueRetryingStepsParams{
		StaleBefore: staleBefore, RowLimit: limit,
	})
	if err != nil {
		return nil, wrapErr(op, err)
	}
	steps := make([]OverdueRetryingStep, len(rows))
	for i, r := range rows {
		steps[i] = OverdueRetryingStep{RunID: r.RunID, StepID: r.StepID}
		if r.NextAttemptAt != nil {
			steps[i].NextAttemptAt = *r.NextAttemptAt
		}
	}
	return steps, nil
}

// OverdueApproval is a pending approval whose timeout passed with no policy
// applied — the delayed-expiry crash gap (ticket 15.4, ADR-005 P3 analogue):
// the post-commit Schedule call was never reached, or the delayed member was
// lost. The reconciler re-outboxes an approval_timeout envelope for it.
type OverdueApproval struct {
	RunID  uuid.UUID
	StepID string
}

// ListOverdueApprovals returns up to limit pending approvals whose timeout is
// before the threshold and whose policy has not been applied (expired_at IS
// NULL) — approvals whose delayed expiry was never scheduled or was lost. The
// heal is an approval_timeout outbox row alone (the reconcile safety net): the
// engine's timeout handler applies the on_timeout policy through the same CAS
// as a human decision, so a duplicate delivery (delayed + healed) is arbitrated
// there.
func ListOverdueApprovals(ctx context.Context, q Querier, before time.Time, limit int32) ([]OverdueApproval, error) {
	const op = "list overdue approvals"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return nil, err
	}
	rows, err := gq.ListOverdueApprovals(ctx, gen.ListOverdueApprovalsParams{Before: before, RowLimit: limit})
	if err != nil {
		return nil, wrapErr(op, err)
	}
	out := make([]OverdueApproval, len(rows))
	for i, r := range rows {
		out[i] = OverdueApproval{RunID: r.RunID, StepID: r.StepID}
	}
	return out, nil
}

// ListStaleRunningStepsInCancellingRuns returns up to limit steps still
// running in a *cancelling* run since before staleBefore — ticket 5.6's
// crash cell: the in-flight worker died before settling its step, and
// ListStaleRunningSteps deliberately skips non-running runs. The heal is
// TakeoverStep (attempt closed `lost`) + CancelStep + the cancel rollup —
// never a re-outbox: cancelled work is not re-dispatched.
func ListStaleRunningStepsInCancellingRuns(ctx context.Context, q Querier, staleBefore time.Time, limit int32) ([]StaleRunningStep, error) {
	const op = "list stale running steps in cancelling runs"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return nil, err
	}
	rows, err := gq.ListStaleRunningStepsInCancellingRuns(ctx, gen.ListStaleRunningStepsInCancellingRunsParams{
		StaleBefore: staleBefore, RowLimit: limit,
	})
	if err != nil {
		return nil, wrapErr(op, err)
	}
	steps := make([]StaleRunningStep, len(rows))
	for i, r := range rows {
		steps[i] = StaleRunningStep{
			StepRef:          StepRef{RunID: r.RunID, StepID: r.StepID, UpdatedAt: r.UpdatedAt},
			ClaimID:          r.ClaimID,
			HasPendingOutbox: r.HasPendingOutbox,
		}
	}
	return steps, nil
}

// DeadlineExceededRun is a run past its materialized wall-clock deadline
// (ticket 5.6) — still running or parked with deadline_at behind now. The
// heal is the run-cancel sweep with reason deadline_exceeded.
type DeadlineExceededRun struct {
	RunID uuid.UUID
	// DeadlineAt is the materialized deadline — how overdue the run is.
	DeadlineAt time.Time
	// StartedAt is when the run began (ticket 14.4): the guard event's
	// elapsed-vs-cap seconds derive from it. Zero if the run never started.
	StartedAt time.Time
}

// ListDeadlineExceededRuns returns up to limit unfinished runs whose
// deadline_at is before now (served by the partial deadline index).
func ListDeadlineExceededRuns(ctx context.Context, q Querier, now time.Time, limit int32) ([]DeadlineExceededRun, error) {
	const op = "list deadline-exceeded runs"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return nil, err
	}
	rows, err := gq.ListDeadlineExceededRuns(ctx, gen.ListDeadlineExceededRunsParams{
		Now: now, RowLimit: limit,
	})
	if err != nil {
		return nil, wrapErr(op, err)
	}
	runs := make([]DeadlineExceededRun, len(rows))
	for i, r := range rows {
		runs[i] = DeadlineExceededRun{RunID: r.ID}
		if r.DeadlineAt != nil {
			runs[i].DeadlineAt = *r.DeadlineAt
		}
		if r.StartedAt != nil {
			runs[i].StartedAt = *r.StartedAt
		}
	}
	return runs, nil
}

// ListStalledRuns returns up to limit runs stuck in an impossible state:
// still running with no live (pending/ready/running/retrying) step, or —
// since 5.6 — cancelling with no running step. Both rollups are atomic
// with the transition terminalizing the last step, so observing either
// means corrupt state or an engine bug; the reconciler flags it loudly
// and touches nothing.
func ListStalledRuns(ctx context.Context, q Querier, limit int32) ([]uuid.UUID, error) {
	const op = "list stalled runs"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return nil, err
	}
	ids, err := gq.ListStalledRuns(ctx, limit)
	return ids, wrapErr(op, err)
}
