package store

// Reconciler read surface (ticket 4.4): the periodic healer's scans and
// its fleet-wide sweep lock. Everything here is read-only diagnostics over
// durable state — the reconciler re-outboxes (Outbox().Create) or flags;
// it never transitions state. All functions must run inside a WithTx
// callback: the staleness scan and the re-outbox insert are only coherent
// as one atomic sweep, and an advisory *xact* lock outside a transaction
// is meaningless. Anything else fails fast with ErrNoTx, mirroring the
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
// reconciler's threshold — ADR-005 R1(c), flag-only until 4.5's takeover.
type StaleRunningStep struct {
	StepRef
	// ClaimID is the (presumed dead) holder's fencing token, for logs.
	ClaimID *uuid.UUID
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
// on any tighter bound. Flag-only in 4.4 (a re-outbox would be
// ACK-dropped as a fresh-delivery duplicate); 4.5's lease-expiry takeover
// upgrades this to a heal.
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
			StepRef: StepRef{RunID: r.RunID, StepID: r.StepID, UpdatedAt: r.UpdatedAt},
			ClaimID: r.ClaimID,
		}
	}
	return steps, nil
}

// ListStalledRuns returns up to limit runs still running with no live
// (pending/ready/running) step — an impossible state, since the run
// rollup is atomic with the transition terminalizing the last step.
// Observing one means corrupt state or an engine bug; the reconciler
// flags it loudly and touches nothing.
func ListStalledRuns(ctx context.Context, q Querier, limit int32) ([]uuid.UUID, error) {
	const op = "list stalled runs"
	gq, err := reconcileQueries(ctx, q, op)
	if err != nil {
		return nil, err
	}
	ids, err := gq.ListStalledRuns(ctx, limit)
	return ids, wrapErr(op, err)
}
