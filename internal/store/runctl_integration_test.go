//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 5.6's store-level suite: the run-control transitions (park,
// unpark, cancel request, cancel rollup), the step-cancel broadening, the
// claim-fenced running-step cancel, the parked rollup guards, the
// materialized deadline, and the reconciler's two new scans.

// runctlPair is a two-step chain: `a` entry, `b` pending behind it.
const runctlPair = `{
	"schema_version": 1,
	"name": "runctl-pair",
	"steps": [{"id": "a", "type": "noop"}, {"id": "b", "type": "noop"}],
	"edges": [{"from": "a", "to": "b"}]
}`

// Single-transition helpers, mirroring transitions_integration_test.go.

func parkRun(t *testing.T, s *store.Store, runID uuid.UUID, reason string) (gen.Run, error) {
	t.Helper()
	var run gen.Run
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		run, err = store.ParkRun(ctx, q, store.ParkRunArgs{RunID: runID, Reason: reason, Now: testNow})
		return err
	})
	return run, err
}

func unparkRun(t *testing.T, s *store.Store, runID uuid.UUID) (gen.Run, error) {
	t.Helper()
	var run gen.Run
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		run, err = store.UnparkRun(ctx, q, store.UnparkRunArgs{RunID: runID, Now: testNow})
		return err
	})
	return run, err
}

func cancelRun(t *testing.T, s *store.Store, runID uuid.UUID, reason string) (gen.Run, error) {
	t.Helper()
	var run gen.Run
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		run, err = store.CancelRun(ctx, q, store.CancelRunArgs{RunID: runID, Reason: reason, Now: testNow})
		return err
	})
	return run, err
}

func cancelRunRollup(t *testing.T, s *store.Store, runID uuid.UUID) (gen.Run, error) {
	t.Helper()
	var run gen.Run
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		run, err = store.CancelRunRollup(ctx, q, store.FailRunArgs{RunID: runID, Now: testNow})
		return err
	})
	return run, err
}

func cancelStepFor(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.CancelStep(ctx, q, store.CancelStepArgs{
			RunID: runID, StepID: stepID, Reason: store.CancelReasonRunCancelled, Now: testNow,
		})
		return err
	})
	return step, err
}

func cancelRunningStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, claim uuid.UUID, errJSON json.RawMessage) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.CancelRunningStep(ctx, q, store.CancelRunningStepArgs{
			RunID: runID, StepID: stepID, ClaimID: claim,
			Reason: store.CancelReasonRunCancelled, Error: errJSON, Now: testNow,
		})
		return err
	})
	return step, err
}

// hasEvent reports whether the eventTypes slice (the shared helper in
// transitions_integration_test.go) contains want.
func hasEvent(types []string, want string) bool {
	for _, ty := range types {
		if ty == want {
			return true
		}
	}
	return false
}

// TestParkUnparkRun: the park/unpark round trip — statuses, the typed
// reason set and cleared, both events, and the wrong-status conflicts on
// double park / unpark of a running run.
func TestParkUnparkRun(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, runctlPair))

	if _, err := parkRun(t, s, run.ID, "coffee_break"); err == nil {
		t.Fatal("ParkRun with an unknown reason: want error, got nil")
	}

	parked, err := parkRun(t, s, run.ID, store.ParkReasonManual)
	if err != nil {
		t.Fatalf("ParkRun: %v", err)
	}
	if parked.Status != store.RunStatusParked || parked.ParkReason == nil || *parked.ParkReason != store.ParkReasonManual {
		t.Fatalf("parked run = status %q, reason %v; want parked/manual", parked.Status, parked.ParkReason)
	}
	_, err = parkRun(t, s, run.ID, store.ParkReasonManual)
	if te := conflictError(t, err, store.ConflictWrongStatus); te.From != store.RunStatusParked {
		t.Errorf("double park From = %q, want parked", te.From)
	}

	resumed, err := unparkRun(t, s, run.ID)
	if err != nil {
		t.Fatalf("UnparkRun: %v", err)
	}
	if resumed.Status != store.RunStatusRunning || resumed.ParkReason != nil {
		t.Fatalf("unparked run = status %q, reason %v; want running/nil", resumed.Status, resumed.ParkReason)
	}
	_, err = unparkRun(t, s, run.ID)
	if te := conflictError(t, err, store.ConflictWrongStatus); te.From != store.RunStatusRunning {
		t.Errorf("unpark of running From = %q, want running", te.From)
	}

	types := eventTypes(t, s, run.ID)
	if !hasEvent(types, store.EventRunParked) || !hasEvent(types, store.EventRunUnparked) {
		t.Errorf("events = %v, want run_parked and run_unparked", types)
	}
}

// TestParkedRunRefusesClaims: the 5.2 run-status guard covers parked —
// the exact mechanism "fleet stops claiming" rests on.
func TestParkedRunRefusesClaims(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, runctlPair))
	if _, err := parkRun(t, s, run.ID, store.ParkReasonManual); err != nil {
		t.Fatalf("ParkRun: %v", err)
	}
	_, err := claimStep(t, s, run.ID, "a")
	if te := conflictError(t, err, store.ConflictRunNotRunning); te.From != store.RunStatusParked {
		t.Errorf("claim conflict From = %q, want parked", te.From)
	}
}

// TestCancelRunSweep: the cancel request from running — status, reason,
// the sweep-side step cancel from ready, and the same-transaction rollup
// once nothing is in flight (exercised here through the store primitives
// the engine op composes).
func TestCancelRunFromRunningAndParked(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))

	// From running.
	run := instantiate(t, s, decodeDef(t, runctlPair))
	cancelling, err := cancelRun(t, s, run.ID, store.RunCancelReasonManual)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if cancelling.Status != store.RunStatusCancelling ||
		cancelling.CancelReason == nil || *cancelling.CancelReason != store.RunCancelReasonManual {
		t.Fatalf("cancelling run = status %q, reason %v; want cancelling/manual", cancelling.Status, cancelling.CancelReason)
	}
	// Cancel of a cancelling run conflicts.
	_, err = cancelRun(t, s, run.ID, store.RunCancelReasonManual)
	if te := conflictError(t, err, store.ConflictWrongStatus); te.From != store.RunStatusCancelling {
		t.Errorf("double cancel From = %q, want cancelling", te.From)
	}
	// The claim path refuses a cancelling run.
	_, err = claimStep(t, s, run.ID, "a")
	wantConflict(t, err, store.ConflictRunNotRunning)

	// From parked: the park reason clears with the exit.
	run2 := instantiate(t, s, decodeDef(t, runctlPair))
	if _, err := parkRun(t, s, run2.ID, store.ParkReasonManual); err != nil {
		t.Fatalf("ParkRun: %v", err)
	}
	cancelling2, err := cancelRun(t, s, run2.ID, store.RunCancelReasonDeadlineExceeded)
	if err != nil {
		t.Fatalf("CancelRun from parked: %v", err)
	}
	if cancelling2.Status != store.RunStatusCancelling || cancelling2.ParkReason != nil {
		t.Fatalf("cancelling run = status %q, park reason %v; want cancelling/nil", cancelling2.Status, cancelling2.ParkReason)
	}
	if _, err := cancelRun(t, s, run2.ID, "boredom"); err == nil {
		t.Fatal("CancelRun with an unknown reason: want error, got nil")
	}
}

// TestCancelStepBroadenedAndRollup: the sweep-side step cancel now covers
// ready and retrying (schedule cleared), the rollup guard rejects while a
// step is live and passes once every step is terminal, and finished_at
// stamps on the terminal run.
func TestCancelStepBroadenedAndRollup(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, runctlPair))
	if _, err := cancelRun(t, s, run.ID, store.RunCancelReasonManual); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	// Rollup refused while steps are live.
	_, err := cancelRunRollup(t, s, run.ID)
	wantConflict(t, err, store.ConflictGuardFailed)

	// The sweep: `a` from ready, `b` from pending.
	if _, err := cancelStepFor(t, s, run.ID, "a"); err != nil {
		t.Fatalf("CancelStep(ready): %v", err)
	}
	if _, err := cancelStepFor(t, s, run.ID, "b"); err != nil {
		t.Fatalf("CancelStep(pending): %v", err)
	}
	// Re-cancel conflicts with wrong_status from cancelled.
	_, err = cancelStepFor(t, s, run.ID, "a")
	if te := conflictError(t, err, store.ConflictWrongStatus); te.From != store.StepStatusCancelled {
		t.Errorf("re-cancel From = %q, want cancelled", te.From)
	}

	done, err := cancelRunRollup(t, s, run.ID)
	if err != nil {
		t.Fatalf("CancelRunRollup: %v", err)
	}
	if done.Status != store.RunStatusCancelled || done.FinishedAt == nil {
		t.Fatalf("rolled-up run = status %q, finished_at %v; want cancelled/stamped", done.Status, done.FinishedAt)
	}
	if done.StepsCancelled != 2 {
		t.Errorf("steps_cancelled = %d, want 2", done.StepsCancelled)
	}
	types := eventTypes(t, s, run.ID)
	if !hasEvent(types, store.EventRunCancelling) || !hasEvent(types, store.EventRunCancelled) {
		t.Errorf("events = %v, want run_cancelling and run_cancelled", types)
	}
}

// TestCancelRetryingStepClearsSchedule: a retrying step cancels with its
// next_attempt_at cleared, so nothing downstream mistakes it as due.
func TestCancelRetryingStepClearsSchedule(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, runctlPair))
	claimed, err := claimStep(t, s, run.ID, "a")
	if err != nil {
		t.Fatalf("ClaimStep: %v", err)
	}
	err = s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.RetryStep(ctx, q, store.RetryStepArgs{
			RunID: run.ID, StepID: "a", ClaimID: *claimed.ClaimID,
			Outcome:       store.AttemptOutcomeTransient,
			Error:         json.RawMessage(`{"message":"boom"}`),
			NextAttemptAt: testNow.Add(time.Minute), Now: testNow,
		})
		return err
	})
	if err != nil {
		t.Fatalf("RetryStep: %v", err)
	}
	if _, err := cancelRun(t, s, run.ID, store.RunCancelReasonManual); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	st, err := cancelStepFor(t, s, run.ID, "a")
	if err != nil {
		t.Fatalf("CancelStep(retrying): %v", err)
	}
	if st.Status != store.StepStatusCancelled || st.NextAttemptAt != nil {
		t.Errorf("cancelled retrying step = status %q, next_attempt_at %v; want cancelled/nil", st.Status, st.NextAttemptAt)
	}
}

// TestCancelRunningStepFenced: the in-flight settlement — attempt closed
// with the administrative `cancelled`, counter bumped, event appended —
// and the claim fence rejecting a stale holder.
func TestCancelRunningStepFenced(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, runctlPair))
	claimed, err := claimStep(t, s, run.ID, "a")
	if err != nil {
		t.Fatalf("ClaimStep: %v", err)
	}
	if _, err := cancelRun(t, s, run.ID, store.RunCancelReasonManual); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	// A stale claim is fenced off.
	stale := uuid.New()
	_, err = cancelRunningStep(t, s, run.ID, "a", stale, nil)
	wantConflict(t, err, store.ConflictClaimMismatch)

	errJSON := json.RawMessage(`{"message":"context canceled"}`)
	st, err := cancelRunningStep(t, s, run.ID, "a", *claimed.ClaimID, errJSON)
	if err != nil {
		t.Fatalf("CancelRunningStep: %v", err)
	}
	if st.Status != store.StepStatusCancelled || st.FinishedAt == nil {
		t.Fatalf("settled step = status %q, finished_at %v; want cancelled/stamped", st.Status, st.FinishedAt)
	}
	attempts, err := s.Attempts().ListByStep(t.Context(), run.ID, "a")
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %v (err %v), want exactly one", attempts, err)
	}
	if attempts[0].Outcome == nil || *attempts[0].Outcome != store.AttemptOutcomeCancelled {
		t.Errorf("attempt outcome = %v, want cancelled", attempts[0].Outcome)
	}
	got, err := s.Runs().Get(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if got.StepsCancelled != 1 {
		t.Errorf("steps_cancelled = %d, want 1", got.StepsCancelled)
	}
}

// TestRollupsFireFromParked: park pauses new dispatch, not the settling
// of in-flight work — the last completion terminalizes a parked run
// (ADR-006 "Park semantics") through both rollup guards.
func TestRollupsFireFromParked(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))

	// Succeed rollup from parked: claim `a`, park, complete both steps.
	run := instantiate(t, s, decodeDef(t, runctlPair))
	claimed, err := claimStep(t, s, run.ID, "a")
	if err != nil {
		t.Fatalf("ClaimStep: %v", err)
	}
	if _, err := parkRun(t, s, run.ID, store.ParkReasonManual); err != nil {
		t.Fatalf("ParkRun: %v", err)
	}
	if _, err := succeedStep(t, s, run.ID, "a", *claimed.ClaimID, nil); err != nil {
		t.Fatalf("SucceedStep on parked run: %v", err)
	}
	// Resolve a→b and skip b would need edge machinery; cancel b via the
	// 5.4 write-off status instead — any terminal mix exercises the guard.
	err = s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		if _, err := store.ResolveEdge(ctx, q, store.ResolveEdgeArgs{
			RunID: run.ID, Ordinal: 0, Fired: false, Now: testNow,
		}); err != nil {
			return err
		}
		_, err := store.SkipStep(ctx, q, store.SkipStepArgs{RunID: run.ID, StepID: "b", Now: testNow})
		return err
	})
	if err != nil {
		t.Fatalf("skipping b: %v", err)
	}
	var done gen.Run
	err = s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		done, err = store.SucceedRun(ctx, q, store.SucceedRunArgs{RunID: run.ID, Now: testNow})
		return err
	})
	if err != nil {
		t.Fatalf("SucceedRun from parked: %v", err)
	}
	if done.Status != store.RunStatusSucceeded || done.ParkReason != nil {
		t.Errorf("run = status %q, park reason %v; want succeeded/nil", done.Status, done.ParkReason)
	}

	// fail_fast disposition from parked: FailRun's guard admits parked.
	run2 := instantiate(t, s, decodeDef(t, runctlPair))
	claimed2, err := claimStep(t, s, run2.ID, "a")
	if err != nil {
		t.Fatalf("ClaimStep: %v", err)
	}
	if _, err := parkRun(t, s, run2.ID, store.ParkReasonManual); err != nil {
		t.Fatalf("ParkRun: %v", err)
	}
	if err := failStep(t, s, run2.ID, "a", *claimed2.ClaimID, nil); err != nil {
		t.Fatalf("DeadLetterStep on parked run: %v", err)
	}
	var failed gen.Run
	err = s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		failed, err = store.FailRun(ctx, q, store.FailRunArgs{RunID: run2.ID, Now: testNow})
		return err
	})
	if err != nil {
		t.Fatalf("FailRun from parked: %v", err)
	}
	if failed.Status != store.RunStatusFailed || failed.ParkReason != nil {
		t.Errorf("run = status %q, park reason %v; want failed/nil", failed.Status, failed.ParkReason)
	}
}

// deadlineDef carries a run wall-clock deadline.
const deadlineDef = `{
	"schema_version": 1,
	"name": "runctl-deadline",
	"max_wall_clock": "1h",
	"steps": [{"id": "a", "type": "noop"}],
	"edges": []
}`

// TestDeadlineMaterialized: instantiation stamps deadline_at = now +
// max_wall_clock; absent max_wall_clock stores NULL.
func TestDeadlineMaterialized(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))

	run := instantiate(t, s, decodeDef(t, deadlineDef))
	if run.DeadlineAt == nil || !run.DeadlineAt.Equal(testNow.Add(time.Hour)) {
		t.Errorf("deadline_at = %v, want %v", run.DeadlineAt, testNow.Add(time.Hour))
	}

	bare := instantiate(t, s, decodeDef(t, runctlPair))
	if bare.DeadlineAt != nil {
		t.Errorf("deadline_at = %v without max_wall_clock, want NULL", bare.DeadlineAt)
	}
}

// TestDeadlineScan: ListDeadlineExceededRuns matches running and parked
// runs past their deadline, and nothing else — not future deadlines, not
// deadline-free runs, not cancelling ones.
func TestDeadlineScan(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))

	overdue := instantiate(t, s, decodeDef(t, deadlineDef))
	parked := instantiate(t, s, decodeDef(t, deadlineDef))
	if _, err := parkRun(t, s, parked.ID, store.ParkReasonManual); err != nil {
		t.Fatalf("ParkRun: %v", err)
	}
	already := instantiate(t, s, decodeDef(t, deadlineDef))
	if _, err := cancelRun(t, s, already.ID, store.RunCancelReasonManual); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	instantiate(t, s, decodeDef(t, runctlPair)) // no deadline

	scanAt := func(now time.Time) []store.DeadlineExceededRun {
		var runs []store.DeadlineExceededRun
		err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
			var err error
			runs, err = store.ListDeadlineExceededRuns(ctx, q, now, 16)
			return err
		})
		if err != nil {
			t.Fatalf("ListDeadlineExceededRuns: %v", err)
		}
		return runs
	}

	if got := scanAt(testNow.Add(30 * time.Minute)); len(got) != 0 {
		t.Errorf("scan before the deadline = %+v, want empty", got)
	}
	got := scanAt(testNow.Add(2 * time.Hour))
	want := map[uuid.UUID]bool{overdue.ID: true, parked.ID: true}
	if len(got) != 2 || !want[got[0].RunID] || !want[got[1].RunID] {
		t.Errorf("scan past the deadline = %+v, want exactly the running and parked overdue runs", got)
	}
}

// TestCancellingStaleRunningScan: the cancelling-run stale scan sees a
// stale running step of a cancelling run and nothing from running runs
// (those belong to the ordinary scan).
func TestCancellingStaleRunningScan(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))

	cancelling := instantiate(t, s, decodeDef(t, runctlPair))
	if _, err := claimStep(t, s, cancelling.ID, "a"); err != nil {
		t.Fatalf("ClaimStep: %v", err)
	}
	if _, err := cancelRun(t, s, cancelling.ID, store.RunCancelReasonManual); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	stillRunning := instantiate(t, s, decodeDef(t, runctlPair))
	if _, err := claimStep(t, s, stillRunning.ID, "a"); err != nil {
		t.Fatalf("ClaimStep: %v", err)
	}

	var got []store.StaleRunningStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		got, err = store.ListStaleRunningStepsInCancellingRuns(ctx, q, testNow.Add(time.Hour), 16)
		return err
	})
	if err != nil {
		t.Fatalf("ListStaleRunningStepsInCancellingRuns: %v", err)
	}
	if len(got) != 1 || got[0].RunID != cancelling.ID || got[0].StepID != "a" || got[0].ClaimID == nil {
		t.Errorf("scan = %+v, want exactly the cancelling run's stale step with its claim", got)
	}
}
