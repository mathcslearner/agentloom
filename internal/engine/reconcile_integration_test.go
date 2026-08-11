//go:build integration

package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 4.4's reconciler suite: the lost-dispatch heal (ADR-005 P2 and
// R1(a) — "kill between commit and XADD"), sweep idempotency and
// rate-bounding, and the impossible-state flags.

// reconcilerAt builds a Reconciler whose clock is frozen at now — sweeps
// judge staleness against a controlled instant, never the wall clock.
func reconcilerAt(t *testing.T, s *store.Store, now time.Time) *engine.Reconciler {
	t.Helper()
	r, err := engine.NewReconciler(s, engine.ReconcilerConfig{
		Interval:     time.Hour, // manual ReconcileOnce only
		ReadyStale:   time.Minute,
		RunningStale: 5 * time.Minute,
		Limit:        10,
	}, engine.WithReconcilerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	return r
}

// reconcileOnce sweeps once, failing the test on error.
func reconcileOnce(t *testing.T, r *engine.Reconciler) engine.ReconcileResult {
	t.Helper()
	res, err := r.ReconcileOnce(t.Context())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	return res
}

// loseDispatch drains the outbox through a lostEnqueuer: the drain
// transaction commits (rows deleted) but no XADD ever happened — the
// crash-between-commit-and-dispatch shape the reconciler exists to heal.
func loseDispatch(t *testing.T, s *store.Store) int {
	t.Helper()
	d, err := engine.NewDispatcher(s, lostEnqueuer{}, engine.DispatcherConfig{
		Interval: time.Hour, Batch: 16,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return drainOnce(t, d)
}

// TestLostDispatchHealedByReconciler is the ticket's headline acceptance:
// a step's dispatch is lost after the drain committed (no outbox row, no
// stream entry, step stuck ready), the reconciler re-outboxes it with
// reason reconcile_ready, and the run then completes normally with
// exactly one readiness event for the healed step.
func TestLostDispatchHealedByReconciler(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, linearDef)

	// Lose the entry step's dispatch.
	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("lost drain dispatched %d rows, want 1", n)
	}
	requireOutboxEmpty(t, s)
	if envs := h.StreamEnvelopes(ctx); len(envs) != 0 {
		t.Fatalf("stream has %d entries after a lost dispatch, want none", len(envs))
	}
	stepA, err := s.Steps().Get(ctx, runID, "a")
	if err != nil {
		t.Fatalf("reading step a: %v", err)
	}
	if stepA.Status != store.StepStatusReady {
		t.Fatalf("step a status = %q, want stuck ready", stepA.Status)
	}

	// Before the staleness threshold the sweep must not touch it.
	early := reconcileOnce(t, reconcilerAt(t, s, testNow.Add(30*time.Second)))
	if len(early.Requeued) != 0 {
		t.Fatalf("sweep before the threshold requeued %+v, want nothing", early.Requeued)
	}

	// Past the threshold: exactly one re-outbox, with the reconciler's
	// reason; an immediate second sweep is a no-op (anti-join idempotency).
	rec := reconcilerAt(t, s, testNow.Add(10*time.Minute))
	res := reconcileOnce(t, rec)
	if len(res.Requeued) != 1 || res.Requeued[0].RunID != runID || res.Requeued[0].StepID != "a" {
		t.Fatalf("sweep requeued %+v, want exactly step a", res.Requeued)
	}
	if res.Skipped || res.LimitHit || len(res.StaleRunning) != 0 || len(res.StalledRuns) != 0 {
		t.Errorf("sweep result %+v, want a pure requeue", res)
	}
	again := reconcileOnce(t, rec)
	if len(again.Requeued) != 0 {
		t.Errorf("second sweep requeued %+v, want nothing while the row is pending", again.Requeued)
	}
	tasks, err := s.Outbox().List(ctx, 10)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	if len(tasks) != 1 || tasks[0].StepID != "a" || tasks[0].Reason != store.OutboxReasonReconcileReady {
		t.Fatalf("outbox after sweep = %+v, want one reconcile_ready row for step a", tasks)
	}

	// With the real dispatcher and workers, the healed run completes.
	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"a": store.StepStatusSucceeded, "b": store.StepStatusSucceeded, "c": store.StepStatusSucceeded,
	})
	requireSingleAttempts(t, s, runID, []string{"a", "b", "c"})
	requireOutboxEmpty(t, s)
	// Healing re-dispatches; it never re-transitions: one step_ready each.
	for _, stepID := range []string{"a", "b", "c"} {
		if got := countStepReadyEvents(t, s, runID, stepID); got != 1 {
			t.Errorf("step_ready(%s) events = %d, want exactly 1", stepID, got)
		}
	}
	// The healed dispatch is visible on the wire by its reason.
	reasons := map[string]string{}
	for _, env := range h.StreamEnvelopes(ctx) {
		reasons[env.StepID] = env.Reason
	}
	if reasons["a"] != store.OutboxReasonReconcileReady {
		t.Errorf("step a dispatched with reason %q, want %q", reasons["a"], store.OutboxReasonReconcileReady)
	}
}

// TestReconcilerSkipsWhileSweepLockHeld: a sweep that cannot take the
// fleet-wide advisory lock does nothing and reports Skipped — the
// no-thundering-herd rule, deterministically.
func TestReconcilerSkipsWhileSweepLockHeld(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, _ := setup(t, singleNoop)
	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("lost drain dispatched %d rows, want 1", n)
	}
	rec := reconcilerAt(t, s, testNow.Add(10*time.Minute))

	err := s.WithTx(ctx, func(txCtx context.Context, q store.Querier) error {
		locked, err := store.TryReconcileLock(txCtx, q)
		if err != nil || !locked {
			t.Fatalf("taking the sweep lock: locked=%v err=%v", locked, err)
		}
		// The sweep runs in its own transaction (fresh ctx, not nested)
		// and must lose the lock and skip.
		res, err := rec.ReconcileOnce(ctx)
		if err != nil {
			return err
		}
		if !res.Skipped || len(res.Requeued) != 0 {
			t.Errorf("sweep under a held lock = %+v, want Skipped with no work", res)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	// Lock released with the transaction: the sweep heals normally.
	res := reconcileOnce(t, rec)
	if res.Skipped || len(res.Requeued) != 1 {
		t.Errorf("sweep after release = %+v, want one requeue", res)
	}
}

// TestReconcilerFlagsWithoutMutating: dead-worker suspects (stale
// running) and impossible run states are reported and logged but never
// touched — the takeover heal is ticket 4.5's.
func TestReconcilerFlagsWithoutMutating(t *testing.T) {
	t.Parallel()
	pool := storetest.NewDB(t)
	s := store.NewFromPool(pool)
	ctx := t.Context()

	def, err := dag.Decode([]byte(singleNoop))
	if err != nil {
		t.Fatalf("decoding definition: %v", err)
	}
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	// Drop the instantiation dispatch so the outbox stays empty — the
	// flag assertions below include "the sweep wrote no rows".
	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("lost drain dispatched %d rows, want 1", n)
	}

	// A claimed step whose worker went silent: running, updated_at old.
	err = s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: "only", Now: testNow})
		return err
	})
	if err != nil {
		t.Fatalf("claiming step: %v", err)
	}

	rec := reconcilerAt(t, s, testNow.Add(10*time.Minute))
	swept := reconcileOnce(t, rec)
	if len(swept.StaleRunning) != 1 || swept.StaleRunning[0].StepID != "only" || swept.StaleRunning[0].RunID != runID {
		t.Errorf("StaleRunning = %+v, want exactly the silent step", swept.StaleRunning)
	}
	if len(swept.Requeued) != 0 || len(swept.StalledRuns) != 0 {
		t.Errorf("sweep = %+v, want flags only", swept)
	}
	step, err := s.Steps().Get(ctx, runID, "only")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if step.Status != store.StepStatusRunning || step.ClaimID == nil {
		t.Errorf("step = %s claim=%v, want running with its claim untouched", step.Status, step.ClaimID)
	}
	requireOutboxEmpty(t, s)

	// Fabricated impossible state: every step terminal, run still running.
	err = s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.SucceedStep(ctx, q, store.SucceedStepArgs{
			RunID: runID, StepID: "only", ClaimID: *step.ClaimID, Now: testNow,
		})
		return err
	})
	if err != nil {
		t.Fatalf("succeeding step: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'running' WHERE id = $1`, runID); err != nil {
		t.Fatalf("fabricating corrupt run status: %v", err)
	}
	swept = reconcileOnce(t, rec)
	if len(swept.StalledRuns) != 1 || swept.StalledRuns[0] != runID {
		t.Errorf("StalledRuns = %v, want exactly the corrupted run", swept.StalledRuns)
	}
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.Status != store.RunStatusRunning {
		t.Errorf("run status = %q — the reconciler must flag, never mutate", run.Status)
	}
}
