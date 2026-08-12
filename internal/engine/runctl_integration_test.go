//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Ticket 5.6's integration suite: run-level cancel (in-flight
// interruption, the sweep, quiescence), park/unpark, the wall-clock
// deadline, and the reconciler's cancelling-run heal.

// waitRunStatus polls until the run reaches want, tolerating any
// intermediate status (unlike waitRun, which fails on leaving running —
// cancel tests pass through cancelling, park tests through parked).
func waitRunStatus(t *testing.T, s *store.Store, runID uuid.UUID, want string) gen.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var run gen.Run
	for time.Now().Before(deadline) {
		var err error
		run, err = s.Runs().Get(t.Context(), runID)
		if err != nil {
			t.Fatalf("reading run: %v", err)
		}
		if run.Status == want {
			return run
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("run never reached %q (stuck at %q):\n%s", want, run.Status, dumpSteps(t, s, runID))
	return gen.Run{}
}

// eventReason decodes the reason field of the first event of the given
// type, or fails if none exists.
func eventReason(t *testing.T, s *store.Store, runID uuid.UUID, eventType string) string {
	t.Helper()
	events, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	for _, ev := range events {
		if ev.Type != eventType {
			continue
		}
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decoding %s payload %s: %v", eventType, ev.Payload, err)
		}
		return payload.Reason
	}
	t.Fatalf("no %s event in the run's log", eventType)
	return ""
}

// cancelDef is the mid-run cancel fixture: a long sleep in flight, a
// successor that must never run.
const cancelDef = `{
	"schema_version": 1,
	"name": "cancel-mid-run",
	"steps": [
		{"id": "slow", "type": "sleep", "config": {"duration": "30s"}},
		{"id": "after", "type": "noop"}
	],
	"edges": [{"from": "slow", "to": "after"}]
}`

// TestCancelMidRunInFlight is 5.6's headline acceptance: cancel a run
// with a step executing. The request's sweep cancels the pending
// successor, the cancellation watch cancels the executor context within
// one poll interval (not the 30s sleep), the settlement records the
// attempt as cancelled — never a retry — and the run quiesces to
// cancelled with no orphan PEL entries, outbox rows, or delayed entries.
func TestCancelMidRunInFlight(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, cancelDef)
	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithCancelPollInterval(20*time.Millisecond))
	h.Spawn("worker-a", eng.Handle, workerConfig())

	waitStep(t, s, runID, "slow", "running", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusRunning
	})
	start := time.Now()
	res, err := eng.Cancel(ctx, runID, store.RunCancelReasonManual)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if res.Run.Status != store.RunStatusCancelling || res.Finalized {
		t.Fatalf("cancel result = status %q, finalized %v; want cancelling, not finalized (a step is in flight)",
			res.Run.Status, res.Finalized)
	}
	if len(res.Cancelled) != 1 || res.Cancelled[0] != "after" {
		t.Fatalf("sweep cancelled %v, want exactly the pending successor", res.Cancelled)
	}

	run := waitRunStatus(t, s, runID, store.RunStatusCancelled)
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("cancel settled after %s — the watch must interrupt the 30s sleep, not wait it out", waited)
	}
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"slow": store.StepStatusCancelled, "after": store.StepStatusCancelled,
	})
	requireAttemptOutcomes(t, s, runID, "slow", []string{store.AttemptOutcomeCancelled})
	if run.CancelReason == nil || *run.CancelReason != store.RunCancelReasonManual {
		t.Errorf("cancel_reason = %v, want manual", run.CancelReason)
	}
	if run.StepsCancelled != 2 || run.StepsFailed != 0 {
		t.Errorf("counters = %d cancelled / %d failed, want 2/0 — a cancel is not a failure",
			run.StepsCancelled, run.StepsFailed)
	}
	if got := eventReason(t, s, runID, store.EventRunCancelling); got != store.RunCancelReasonManual {
		t.Errorf("run_cancelling reason = %q, want manual", got)
	}
	if got := countEvents(t, s, runID, store.EventRunCancelled); got != 1 {
		t.Errorf("run_cancelled events = %d, want 1", got)
	}
	if got := countEvents(t, s, runID, store.EventStepRetryScheduled); got != 0 {
		t.Errorf("step_retry_scheduled events = %d, want 0 — cancellation is never judged as a retryable failure", got)
	}
	slow, err := s.Steps().Get(ctx, runID, "slow")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if !strings.Contains(string(slow.Error), "cancel") {
		t.Errorf("step error = %s, want the cancellation recorded", slow.Error)
	}
	if h.DelayedLen(ctx) != 0 {
		t.Error("delayed set not empty after cancel")
	}
	requireOutboxEmpty(t, s)
}

// TestCancelIdleRunFinalizesImmediately: cancelling a run with nothing
// claimed sweeps every step and reaches terminal cancelled in the request
// transaction itself.
func TestCancelIdleRunFinalizesImmediately(t *testing.T) {
	t.Parallel()
	s, _, runID := setup(t, cancelDef)
	eng := newWorker(t, s, "worker-a")

	res, err := eng.Cancel(t.Context(), runID, store.RunCancelReasonManual)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !res.Finalized || len(res.Cancelled) != 2 {
		t.Fatalf("cancel result = finalized %v, cancelled %v; want finalized with both steps swept",
			res.Finalized, res.Cancelled)
	}
	run, err := s.Runs().Get(t.Context(), runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.Status != store.RunStatusCancelled || run.FinishedAt == nil {
		t.Errorf("run = status %q, finished_at %v; want cancelled/stamped", run.Status, run.FinishedAt)
	}
	// Cancel of a terminal run is a typed conflict, not a panic or a
	// silent success.
	if _, err := eng.Cancel(t.Context(), runID, store.RunCancelReasonManual); err == nil {
		t.Error("second Cancel: want conflict, got nil")
	}
}

// blockingNoop is a noop-typed executor that blocks until released, then
// succeeds. Deterministic interleaving control for the park and
// success-race tests: the test decides exactly when the "in-flight" step
// completes. Calls after the release pass straight through.
type blockingNoop struct {
	entered chan struct{} // one tick per Execute entry
	release chan struct{} // closed to let every Execute pass
}

func newBlockingNoop() *blockingNoop {
	return &blockingNoop{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (*blockingNoop) Type() string { return string(dag.StepNoop) }

func (b *blockingNoop) Execute(ctx context.Context, _ exec.StepContext) (exec.Output, error) {
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return exec.Output{Data: json.RawMessage(`{"done":true}`)}, nil
	case <-ctx.Done():
		return exec.Output{}, ctx.Err()
	}
}

// pairNoop is two noops in a chain, for the blocking-executor tests.
const pairNoop = `{
	"schema_version": 1,
	"name": "runctl-pair",
	"steps": [{"id": "block", "type": "noop"}, {"id": "after", "type": "noop"}],
	"edges": [{"from": "block", "to": "after"}]
}`

// TestCancelSuccessRaceHonored: the executor finishes successfully after
// the cancel request (the watch is disabled to force the race). The
// output is honored — recorded on the succeeded step — but fan-out is
// skipped, the swept successor stays cancelled, and the run finalizes
// cancelled.
func TestCancelSuccessRaceHonored(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, pairNoop)
	blocker := newBlockingNoop()
	reg, err := exec.NewRegistry(blocker)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, workerConfig())

	<-blocker.entered // the entry step is mid-execution
	if _, err := eng.Cancel(ctx, runID, store.RunCancelReasonManual); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(blocker.release) // the executor now returns success

	run := waitRunStatus(t, s, runID, store.RunStatusCancelled)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"block": store.StepStatusSucceeded, "after": store.StepStatusCancelled,
	})
	block, err := s.Steps().Get(ctx, runID, "block")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if !jsonEqual(t, block.Output, json.RawMessage(`{"done":true}`)) {
		t.Errorf("output = %s, want the honored result", block.Output)
	}
	if got := countStepReadyEvents(t, s, runID, "after"); got != 0 {
		t.Errorf("successor readied %d times, want 0 — fan-out must be skipped on a cancelling run", got)
	}
	if run.StepsSucceeded != 1 || run.StepsCancelled != 1 {
		t.Errorf("counters = %d succeeded / %d cancelled, want 1/1", run.StepsSucceeded, run.StepsCancelled)
	}
	requireOutboxEmpty(t, s)
}

// TestParkUnparkResumesToCompletion: park stops the fleet claiming while
// in-flight work settles — the completion fans out, the successor's
// delivery is consumed by the run-status guard without an attempt — and
// unpark re-outboxes the stranded ready step and the run completes.
func TestParkUnparkResumesToCompletion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, pairNoop)
	blocker := newBlockingNoop()
	reg, err := exec.NewRegistry(blocker)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, workerConfig())

	<-blocker.entered // the entry step is mid-execution
	parked, err := eng.Park(ctx, runID, store.ParkReasonManual)
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if parked.Status != store.RunStatusParked {
		t.Fatalf("parked run status = %q", parked.Status)
	}
	close(blocker.release) // the in-flight step now completes, while parked

	// The completion fans out normally: the successor becomes ready, its
	// dispatch is drained and delivered, and the claim guard consumes the
	// delivery without executing. Quiescence proves the delivery was
	// ack-dropped, not left pending.
	waitStep(t, s, runID, "after", "ready", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusReady
	})
	h.WaitQuiescent(ctx)
	after, err := s.Steps().Get(ctx, runID, "after")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if after.AttemptCount != 0 {
		t.Fatalf("parked run's successor has %d attempts, want 0 — the fleet must not claim", after.AttemptCount)
	}
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.Status != store.RunStatusParked {
		t.Fatalf("run status = %q, want still parked (in-flight settling does not unpark)", run.Status)
	}

	// Unpark re-outboxes the stranded ready step and the run completes.
	res, err := eng.Unpark(ctx, runID)
	if err != nil {
		t.Fatalf("Unpark: %v", err)
	}
	if len(res.Dispatched) != 1 || res.Dispatched[0] != "after" {
		t.Fatalf("unpark dispatched %v, want exactly the stranded ready step", res.Dispatched)
	}
	waitRunStatus(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
	requireStepStatuses(t, s, runID, map[string]string{
		"block": store.StepStatusSucceeded, "after": store.StepStatusSucceeded,
	})
	if got := countEvents(t, s, runID, store.EventRunParked); got != 1 {
		t.Errorf("run_parked events = %d, want 1", got)
	}
	if got := countEvents(t, s, runID, store.EventRunUnparked); got != 1 {
		t.Errorf("run_unparked events = %d, want 1", got)
	}
	requireOutboxEmpty(t, s)
}

// deadlineRunDef carries a one-hour wall-clock deadline.
const deadlineRunDef = `{
	"schema_version": 1,
	"name": "deadline-run",
	"max_wall_clock": "1h",
	"steps": [{"id": "only", "type": "noop"}],
	"edges": []
}`

// TestDeadlineExceededCancelsRun: the reconciler's deadline scan cancels
// a run past its materialized max_wall_clock with reason
// deadline_exceeded — on the injected clock, no wall time involved — and
// the sweep is idempotent (a cancelled run stops matching).
func TestDeadlineExceededCancelsRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, runID := setup(t, deadlineRunDef)
	clk := newFakeClock(testNow)
	rec, err := engine.NewReconciler(s, engine.ReconcilerConfig{
		Interval:   time.Hour,
		ReadyStale: time.Hour, RunningStale: time.Hour, RetryStale: time.Hour,
		Limit: 16,
	}, engine.WithReconcilerClock(clk.Now))
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}

	// Before the deadline: nothing to do.
	clk.Set(testNow.Add(30 * time.Minute))
	swept, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(swept.DeadlineCancelled) != 0 {
		t.Fatalf("DeadlineCancelled before the deadline = %v, want empty", swept.DeadlineCancelled)
	}

	// Past the deadline: cancelled with the typed reason; the un-run entry
	// step is swept and the run finalizes in the same transaction.
	clk.Set(testNow.Add(2 * time.Hour))
	swept, err = rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(swept.DeadlineCancelled) != 1 || swept.DeadlineCancelled[0] != runID {
		t.Fatalf("DeadlineCancelled = %v, want exactly the overdue run", swept.DeadlineCancelled)
	}
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.Status != store.RunStatusCancelled ||
		run.CancelReason == nil || *run.CancelReason != store.RunCancelReasonDeadlineExceeded {
		t.Fatalf("run = status %q, reason %v; want cancelled/deadline_exceeded", run.Status, run.CancelReason)
	}
	if got := eventReason(t, s, runID, store.EventRunCancelling); got != store.RunCancelReasonDeadlineExceeded {
		t.Errorf("run_cancelling reason = %q, want deadline_exceeded", got)
	}
	requireStepStatuses(t, s, runID, map[string]string{"only": store.StepStatusCancelled})

	again, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("third ReconcileOnce: %v", err)
	}
	if len(again.DeadlineCancelled) != 0 {
		t.Errorf("second past-deadline sweep cancelled %v, want nothing (idempotent)", again.DeadlineCancelled)
	}
}

// TestReconcilerHealsCancellingRun: the crash cell — a worker dies (here:
// stalls forever) holding a cancelling run's only in-flight step. The
// cancelling-run stale scan settles it: takeover closes the attempt
// `lost`, the step cancels, the run finalizes. The zombie's eventual
// completion is fenced off, and its unacked entry drains through reclaim
// into the run-status guard's ack-drop.
func TestReconcilerHealsCancellingRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, pairNoop)
	blocker := newBlockingNoop()
	reg, err := exec.NewRegistry(blocker)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	engA, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	// Short lease so the zombie's unacked entry redelivers quickly after
	// its fenced completion; worker-b is the reclaiming survivor.
	cfg := queue.ConsumerConfig{
		Block: 200 * time.Millisecond, Batch: 1,
		LeaseTTL: 500 * time.Millisecond,
	}
	engB, err := engine.New(s, reg, "worker-b", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", engA.Handle, cfg)
	h.Spawn("worker-b", engB.Handle, cfg)

	<-blocker.entered // one worker is mid-execution and will stall
	if _, err := engA.Cancel(ctx, runID, store.RunCancelReasonManual); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// The reconciler on a fake clock far in the future: the running step
	// is stale, its run cancelling — heal it. The claim stamped updated_at
	// from the worker's real clock, so the fake clock is anchored past it.
	clk := newFakeClock(time.Now().Add(24 * time.Hour))
	rec, err := engine.NewReconciler(s, engine.ReconcilerConfig{
		Interval:   time.Hour,
		ReadyStale: time.Hour, RunningStale: time.Minute, RetryStale: time.Hour,
		Limit: 16,
	}, engine.WithReconcilerClock(clk.Now))
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	swept, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(swept.CancelHealed) != 1 || swept.CancelHealed[0].StepID != "block" {
		t.Fatalf("CancelHealed = %+v, want exactly the stalled step", swept.CancelHealed)
	}
	run := waitRunStatus(t, s, runID, store.RunStatusCancelled)
	requireStepStatuses(t, s, runID, map[string]string{
		"block": store.StepStatusCancelled, "after": store.StepStatusCancelled,
	})
	requireAttemptOutcomes(t, s, runID, "block", []string{store.AttemptOutcomeLost})
	if got := countEvents(t, s, runID, store.EventStepReclaimed); got != 1 {
		t.Errorf("step_reclaimed events = %d, want 1", got)
	}
	if run.StepsCancelled != 2 {
		t.Errorf("steps_cancelled = %d, want 2", run.StepsCancelled)
	}

	// Release the zombie: its success completion must be fenced (the
	// takeover cleared its claim), the unacked entry redelivers, and the
	// run-status guard consumes it — full quiescence, nothing pending.
	close(blocker.release)
	h.WaitQuiescent(ctx)
	requireOutboxEmpty(t, s)
}
