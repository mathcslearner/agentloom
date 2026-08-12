package engine

// Run-level controls (ticket 5.6, ADR-006): cancel, park/unpark, and the
// in-flight cancellation watch. All three ops are engine-internal until
// M6.5 exposes them via the API, mirroring 5.4's Requeue.
//
// Cancel is cooperative, converging through three mechanisms:
//
//  1. The request itself (cancelRunTx): one transaction moving the run
//     running|parked → cancelling, sweeping every claimless non-terminal
//     step (pending/ready/retrying) to cancelled, and attempting the
//     cancelling → cancelled finalization — which passes immediately when
//     nothing was in flight.
//  2. In-flight workers: the cancellation watch (watchRunCancel) polls the
//     run's status while an executor runs and cancels its context; either
//     way, the completion transaction re-checks the run status under the
//     run lock (complete.go) and settles the step as cancelled instead of
//     routing its outcome. The watch is pure latency — the in-transaction
//     check alone is correct.
//  3. The reconciler: a worker that dies mid-cancel leaves its step
//     running in a cancelling run; the cancelling-run stale scan heals it
//     with takeover + cancel + rollup (reconcile.go).
//
// Park pauses dispatch and nothing else: the claim path's run-status
// guard (5.2) refuses a parked run's steps, while in-flight steps settle
// normally — fan-out proceeds, their newly-ready successors' deliveries
// bounce off the guard and are consumed, and unpark re-outboxes every
// ready step without a pending dispatch row (the 5.4 requeue machinery).
// Rollups fire from parked (ADR-006 "Park semantics"), so a parked run
// whose in-flight work finishes terminalizes honestly; overdue retrying
// steps whose delayed deliveries were consumed while parked are healed by
// the reconciler's ordinary overdue-retrying scan once the run is running
// again.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// CancelResult is what Cancel did.
type CancelResult struct {
	// Run is the run row as of the cancel request (status cancelling).
	Run gen.Run
	// Cancelled lists the claimless non-terminal steps the request's sweep
	// wrote off (pending/ready/retrying → cancelled).
	Cancelled []string
	// Finalized reports the run reached the terminal cancelled state in
	// the same transaction — nothing was in flight.
	Finalized bool
}

// Cancel is the run-cancel op (ticket 5.6; M6.5 exposes it via the API):
// one transaction that requests the cancel, sweeps the claimless steps,
// and finalizes when nothing is in flight. A typed *store.TransitionError
// surfaces when the run is not running or parked (already cancelling, or
// terminal). In-flight steps settle as their workers notice — the
// cancellation watch, the completion transaction's run-status check, or
// the reconciler's cancelling-run heal.
func (e *Engine) Cancel(ctx context.Context, runID uuid.UUID, reason string) (CancelResult, error) {
	ctx = log.With(ctx, log.RunID(runID.String()))
	now := e.now()
	var res CancelResult
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		res, err = cancelRunTx(ctx, q, runID, reason, now)
		return err
	})
	if txErr != nil {
		return CancelResult{}, txErr
	}
	log.From(ctx).InfoContext(ctx, "run cancel requested",
		slog.String("reason", reason),
		slog.Int("steps_cancelled", len(res.Cancelled)),
		slog.Bool("finalized", res.Finalized))
	return res, nil
}

// cancelRunTx is the cancel core, shared by Cancel (its own transaction)
// and the reconciler's deadline sweep (inside the sweep transaction).
// Must run inside WithTx.
func cancelRunTx(ctx context.Context, q store.Querier, runID uuid.UUID, reason string, now time.Time) (CancelResult, error) {
	var res CancelResult
	run, err := store.CancelRun(ctx, q, store.CancelRunArgs{RunID: runID, Reason: reason, Now: now})
	if err != nil {
		return CancelResult{}, err
	}
	res.Run = run

	// The sweep: every claimless non-terminal step is written off in the
	// request transaction, so the only steps left unsettled are running
	// ones (their in-flight workers hold the claims). CancelRun holds the
	// run lock, so the read is the current graph state and no completion
	// can interleave.
	steps, err := q.Steps().ListByRun(ctx, runID)
	if err != nil {
		return CancelResult{}, err
	}
	for _, st := range steps {
		switch st.Status {
		case store.StepStatusPending, store.StepStatusReady, store.StepStatusRetrying:
		default:
			continue
		}
		if _, err := store.CancelStep(ctx, q, store.CancelStepArgs{
			RunID: runID, StepID: st.StepID,
			Reason: store.CancelReasonRunCancelled, Now: now,
		}); err != nil {
			return CancelResult{}, err
		}
		res.Cancelled = append(res.Cancelled, st.StepID)
	}

	// Finalize when nothing was in flight; otherwise the last in-flight
	// completion (or the reconciler heal) attempts the same rollup.
	res.Finalized, err = attemptCancelRollup(ctx, q, runID, now)
	if err != nil {
		return CancelResult{}, err
	}
	return res, nil
}

// attemptCancelRollup tries the cancelling → cancelled finalization and
// drops the conflict when the guard does not (yet) hold — the exact shape
// of attemptRunRollup's members.
func attemptCancelRollup(ctx context.Context, q store.Querier, runID uuid.UUID, now time.Time) (bool, error) {
	_, err := store.CancelRunRollup(ctx, q, store.FailRunArgs{RunID: runID, Now: now})
	if err == nil {
		return true, nil
	}
	var te *store.TransitionError
	if errors.As(err, &te) {
		return false, nil
	}
	return false, err
}

// healCancellingStep settles one stale running step of a cancelling run
// (the reconciler's 5.6 heal): the lease-expiry takeover closes the dead
// holder's dangling attempt `lost` and clears its fence, the cancel
// writes the now-ready step off, and the rollup finalizes the run when
// that was its last unsettled step. Runs inside the sweep transaction; a
// typed conflict from either transition means the step moved on since the
// scan snapshot (the caller drops the heal, never the sweep).
func healCancellingStep(ctx context.Context, q store.Querier, st store.StaleRunningStep, now time.Time) error {
	if _, err := store.TakeoverStep(ctx, q, store.TakeoverStepArgs{
		RunID: st.RunID, StepID: st.StepID, ClaimID: *st.ClaimID, Now: now,
	}); err != nil {
		return err
	}
	if _, err := store.CancelStep(ctx, q, store.CancelStepArgs{
		RunID: st.RunID, StepID: st.StepID,
		Reason: store.CancelReasonRunCancelled, Now: now,
	}); err != nil {
		return err
	}
	_, err := attemptCancelRollup(ctx, q, st.RunID, now)
	return err
}

// Park is the run-park op (ticket 5.6; M6.5 exposes it via the API):
// running → parked with a typed reason. Dispatch pauses via the claim
// path's run-status guard; nothing else changes — in-flight steps settle
// normally. A typed *store.TransitionError surfaces when the run is not
// running.
func (e *Engine) Park(ctx context.Context, runID uuid.UUID, reason string) (gen.Run, error) {
	ctx = log.With(ctx, log.RunID(runID.String()))
	now := e.now()
	var run gen.Run
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		run, err = store.ParkRun(ctx, q, store.ParkRunArgs{RunID: runID, Reason: reason, Now: now})
		return err
	})
	if txErr != nil {
		return gen.Run{}, txErr
	}
	log.From(ctx).InfoContext(ctx, "run parked", slog.String("reason", reason))
	return run, nil
}

// UnparkResult is what Unpark did.
type UnparkResult struct {
	// Run is the run row back in running.
	Run gen.Run
	// Dispatched lists the ready steps re-outboxed with reason unpark —
	// their deliveries were consumed by the run-status guard while the run
	// was parked.
	Dispatched []string
}

// Unpark is the run-unpark op (ticket 5.6; M6.5 exposes it via the API):
// one transaction moving the run parked → running and re-outboxing every
// ready step with no pending dispatch row. Overdue retrying steps are
// deliberately left to the reconciler's overdue-retrying scan, which
// admits them again once the run is running. Post-commit, the dispatcher
// nudge fires.
func (e *Engine) Unpark(ctx context.Context, runID uuid.UUID) (UnparkResult, error) {
	ctx = log.With(ctx, log.RunID(runID.String()))
	now := e.now()
	var res UnparkResult
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		run, err := store.UnparkRun(ctx, q, store.UnparkRunArgs{RunID: runID, Now: now})
		if err != nil {
			return err
		}
		res.Run = run
		toDispatch, err := q.Steps().ListReadyWithoutOutbox(ctx, runID)
		if err != nil {
			return err
		}
		for _, id := range toDispatch {
			if _, err := q.Outbox().Create(ctx, runID, id, store.OutboxReasonUnpark); err != nil {
				return err
			}
		}
		res.Dispatched = toDispatch
		return nil
	})
	if txErr != nil {
		return UnparkResult{}, txErr
	}
	log.From(ctx).InfoContext(ctx, "run unparked",
		slog.Int("steps_dispatched", len(res.Dispatched)))
	if len(res.Dispatched) > 0 && e.nudge != nil {
		e.nudge()
	}
	return res, nil
}

// errRunCancelled is the cancellation watch's cancel cause: the run
// entered cancelling while the executor ran. Distinguishable from a
// shutdown (context.Canceled with no cause) and from a step timeout
// (context.DeadlineExceeded on the timeout child).
var errRunCancelled = errors.New("engine: run is cancelling — executor context cancelled")

// watchRunCancel starts the in-flight cancellation watch for one executor
// invocation: a goroutine polling the run's status every
// cancelPollInterval (an unlocked point read — a hint, never the
// authority) that cancels the returned context with errRunCancelled when
// the run turns cancelling. The returned stop joins the goroutine, so a
// completed invocation leaves nothing behind (the watchdogLog contract).
// A disabled watch (interval 0) returns the caller's context untouched.
//
// Poll failures are logged and skipped — the watch is a latency
// optimization; the completion transaction's run-status check under the
// run lock is what makes cancellation correct.
func (e *Engine) watchRunCancel(ctx context.Context, runID uuid.UUID) (context.Context, func()) {
	if e.cancelPollInterval <= 0 {
		return ctx, func() {}
	}
	watchCtx, cancel := context.WithCancelCause(ctx)
	stopped := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(e.cancelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-stopped:
				return
			case <-ticker.C:
			}
			status, err := e.store.Runs().GetStatus(watchCtx, runID)
			if err != nil {
				if watchCtx.Err() == nil {
					log.From(ctx).DebugContext(ctx, "cancellation watch poll failed; executor continues",
						slog.Any("error", err))
				}
				continue
			}
			if status == store.RunStatusCancelling || status == store.RunStatusCancelled {
				log.From(ctx).InfoContext(ctx, "run is cancelling — cancelling executor context")
				cancel(errRunCancelled)
				return
			}
		}
	}()
	return watchCtx, func() {
		close(stopped)
		<-finished
		// Release the context's resources; a no-op if the watch already
		// cancelled with errRunCancelled (the first cause wins).
		cancel(context.Canceled)
	}
}
