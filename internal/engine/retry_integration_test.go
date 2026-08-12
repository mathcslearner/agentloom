//go:build integration

package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
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

// Ticket 5.2's integration suite: the retry engine end to end. Timing is
// fully injected — the engine and reconciler run on a shared fake clock,
// the spawned consumers' own promoter tick is parked (an hour), and the
// tests drive delayed promotion by hand through the harness. Real wall
// time never decides anything, per the injectable-time invariant.

// fakeClock is a settable clock shared by the engine, the reconciler,
// and the test's promotion calls.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock(t0 time.Time) *fakeClock {
	c := &fakeClock{}
	c.ns.Store(t0.UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time   { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Set(tm time.Time) { c.ns.Store(tm.UnixNano()) }

// retryWorkerConfig parks every real-time duty that could promote or
// reclaim behind the test's back; the tests promote by hand.
func retryWorkerConfig() queue.ConsumerConfig {
	return queue.ConsumerConfig{
		Block: 500 * time.Millisecond, Batch: 1,
		PromoterTick: time.Hour,
	}
}

// waitStep polls until the step satisfies cond, failing on timeout with a
// state dump.
func waitStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, what string, cond func(gen.RunStep) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var step gen.RunStep
	for time.Now().Before(deadline) {
		var err error
		step, err = s.Steps().Get(t.Context(), runID, stepID)
		if err != nil {
			t.Fatalf("reading step %q: %v", stepID, err)
		}
		if cond(step) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("step %q never reached %s (status %q, next_attempt_at %v):\n%s",
		stepID, what, step.Status, step.NextAttemptAt, dumpSteps(t, s, runID))
}

// delayedLener is the slice of the queuetest harness waitRetryScheduled
// needs.
type delayedLener interface {
	DelayedLen(ctx context.Context) int64
}

// waitRetryScheduled waits for the step to rest in retrying with the
// expected due time AND for its delayed entry to exist — the post-commit
// schedule is asynchronous relative to the status flip.
func waitRetryScheduled(t *testing.T, s *store.Store, h delayedLener, runID uuid.UUID, stepID string, wantDue time.Time) {
	t.Helper()
	waitStep(t, s, runID, stepID, "retrying at "+wantDue.String(), func(st gen.RunStep) bool {
		return st.Status == store.StepStatusRetrying &&
			st.NextAttemptAt != nil && st.NextAttemptAt.Equal(wantDue)
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.DelayedLen(t.Context()) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delayed entry never appeared for the scheduled retry")
}

// requireAttemptOutcomes asserts the step's attempt history is exactly
// the given outcome sequence.
func requireAttemptOutcomes(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, want []string) {
	t.Helper()
	attempts, err := s.Attempts().ListByStep(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != len(want) {
		t.Fatalf("attempt count = %d, want %d (%+v)", len(attempts), len(want), attempts)
	}
	for i := 0; i < len(attempts) && i < len(want); i++ {
		switch a := attempts[i]; {
		case a.Outcome == nil:
			t.Errorf("attempt %d has no outcome, want %q", i+1, want[i])
		case *a.Outcome != want[i]:
			t.Errorf("attempt %d outcome = %q, want %q", i+1, *a.Outcome, want[i])
		}
	}
}

// retryDef fails twice then succeeds, under an explicit deterministic
// policy: three total attempts, exponential 1s/2s delays, no jitter.
const retryDef = `{
	"schema_version": 1,
	"name": "retry-happy",
	"steps": [
		{"id": "flaky", "type": "fail_n_times", "config": {"n": 2},
		 "retry": {"max_attempts": 3, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}},
		{"id": "after", "type": "noop"}
	],
	"edges": [{"from": "flaky", "to": "after"}]
}`

// TestRetrySucceedsWithinBudget is 5.2's headline acceptance:
// fail_n_times(2) under max_attempts 3 succeeds on attempt 3, and the
// timings honor the backoff exactly — next_attempt_at lands at +1s then
// +2s on the fake clock, the delayed entry refuses to promote before its
// due time, and the run completes with attempt history
// transient/transient/succeeded.
func TestRetrySucceedsWithinBudget(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, retryDef)
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()))
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	// Attempt 1 fails; the first counted failure schedules retry 1 at
	// t0+1s (initial, no jitter).
	due1 := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "flaky", due1)

	// Not due yet: promotion just before the fire time must move nothing.
	if res := h.PromoteDue(ctx, due1.Add(-time.Millisecond), 16); res.Promoted != 0 {
		t.Fatalf("promoted %d entries before the due time", res.Promoted)
	}

	// Fire retry 1. Attempt 2 fails; retry 2 is due 2s later (doubling).
	clk.Set(due1)
	if res := h.PromoteDue(ctx, due1, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries at due time, want 1", res.Promoted)
	}
	due2 := due1.Add(2 * time.Second)
	waitRetryScheduled(t, s, h, runID, "flaky", due2)

	// Fire retry 2. Attempt 3 succeeds and the run completes.
	clk.Set(due2)
	if res := h.PromoteDue(ctx, due2, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries at due time, want 1", res.Promoted)
	}
	run := waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"flaky": store.StepStatusSucceeded, "after": store.StepStatusSucceeded,
	})
	requireAttemptOutcomes(t, s, runID, "flaky", []string{
		store.AttemptOutcomeTransient, store.AttemptOutcomeTransient, store.StepStatusSucceeded,
	})
	if run.StepsFailed != 0 || run.StepsSucceeded != 2 {
		t.Errorf("run counters = %d/%d (succeeded/failed), want 2/0 — retries must not bump steps_failed",
			run.StepsSucceeded, run.StepsFailed)
	}
	if got := countEvents(t, s, runID, store.EventStepRetryScheduled); got != 2 {
		t.Errorf("step_retry_scheduled events = %d, want 2", got)
	}
	flaky, err := s.Steps().Get(ctx, runID, "flaky")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if flaky.NextAttemptAt != nil {
		t.Errorf("next_attempt_at = %v after success, want cleared", flaky.NextAttemptAt)
	}
	requireOutboxEmpty(t, s)
}

// exhaustDef can never succeed within its two-attempt budget.
const exhaustDef = `{
	"schema_version": 1,
	"name": "retry-exhausted",
	"steps": [
		{"id": "doomed", "type": "fail_n_times", "config": {"n": 5},
		 "retry": {"max_attempts": 2, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}},
		{"id": "never", "type": "noop"}
	],
	"edges": [{"from": "doomed", "to": "never"}]
}`

// TestRetryExhaustionFailsRun: when the budget runs out, the final
// counted failure routes to the terminal path — step failed with the full
// error history (every attempt classed), run failed, no further dispatch.
func TestRetryExhaustionFailsRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, exhaustDef)
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()))
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	due1 := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "doomed", due1)
	clk.Set(due1)
	if res := h.PromoteDue(ctx, due1, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries, want 1", res.Promoted)
	}

	run := waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"doomed": store.StepStatusDeadLettered, "never": store.StepStatusPending,
	})
	requireAttemptOutcomes(t, s, runID, "doomed", []string{
		store.AttemptOutcomeTransient, store.AttemptOutcomeTransient,
	})
	if run.StepsFailed != 1 {
		t.Errorf("steps_failed = %d, want 1", run.StepsFailed)
	}
	doomed, err := s.Steps().Get(ctx, runID, "doomed")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if !strings.Contains(string(doomed.Error), "deliberate failure") ||
		!strings.Contains(string(doomed.Error), store.AttemptOutcomeTransient) {
		t.Errorf("step error = %s, want the message and the judged class recorded", doomed.Error)
	}
	if got := countEvents(t, s, runID, store.EventStepRetryScheduled); got != 1 {
		t.Errorf("step_retry_scheduled events = %d, want 1", got)
	}
	if got := countEvents(t, s, runID, store.EventStepDeadLettered); got != 1 {
		t.Errorf("step_dead_lettered events = %d, want 1", got)
	}
	dls, err := s.DeadLetters().ListByStep(ctx, runID, "doomed")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 || dls[0].Source != store.DeadLetterSourceRetriesExhausted ||
		dls[0].Class == nil || *dls[0].Class != store.AttemptOutcomeTransient || dls[0].AttemptsAtDeath != 2 {
		t.Errorf("dead letters = %+v, want one retries_exhausted/transient row at attempt 2", dls)
	}
	if h.DelayedLen(ctx) != 0 {
		t.Error("delayed set not empty after exhaustion")
	}
	requireOutboxEmpty(t, s)
}

// permanentNoop is a noop-typed executor that always returns a declared
// permanent error.
type permanentNoop struct{}

func (permanentNoop) Type() string { return string(dag.StepNoop) }

func (permanentNoop) Execute(context.Context, exec.StepContext) (exec.Output, error) {
	return exec.Output{}, exec.Permanentf("model does not exist")
}

// TestPermanentClassSkipsRetry: a declared-permanent failure never
// consumes the retry budget — one attempt, outcome permanent, terminal on
// the spot even though the default policy would admit two more attempts.
func TestPermanentClassSkipsRetry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, soloDef)
	clk := newFakeClock(testNow)
	reg, err := exec.NewRegistry(permanentNoop{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	_ = d
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireAttemptOutcomes(t, s, runID, "solo", []string{store.AttemptOutcomePermanent})
	if got := countEvents(t, s, runID, store.EventStepRetryScheduled); got != 0 {
		t.Errorf("step_retry_scheduled events = %d, want 0", got)
	}
	if h.DelayedLen(ctx) != 0 {
		t.Error("delayed set not empty — a permanent failure must not schedule")
	}
	dls, err := s.DeadLetters().ListByStep(ctx, runID, "solo")
	if err != nil || len(dls) != 1 || dls[0].Source != store.DeadLetterSourcePermanent {
		t.Errorf("dead letters = %v (err %v), want one row with source permanent (5.4)", dls, err)
	}
}

// failingScheduler always fails its delayed write — the deterministic way
// to provoke ADR-006's failure-commit/delayed-schedule crash gap.
type failingScheduler struct{ calls atomic.Int64 }

func (f *failingScheduler) Schedule(context.Context, queue.Envelope, time.Time) error {
	f.calls.Add(1)
	return errors.New("injected schedule failure")
}

// gapDef fails once then succeeds, with a policy that admits the retry.
const gapDef = `{
	"schema_version": 1,
	"name": "retry-crash-gap",
	"steps": [
		{"id": "flaky", "type": "fail_n_times", "config": {"n": 1},
		 "retry": {"max_attempts": 3, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}}
	],
	"edges": []
}`

// TestRetryCrashGapHealedByReconciler: the retry commits but the delayed
// schedule is lost (injected scheduler failure — the same durable state a
// crash between commit and ZADD leaves). The delivery still ACKs, the
// step rests in retrying with next_attempt_at set, and the reconciler's
// overdue-retrying scan re-outboxes it under reason reconcile_retry —
// idempotently — after which the step re-executes and the run completes.
func TestRetryCrashGapHealedByReconciler(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, gapDef)
	clk := newFakeClock(testNow)

	// A parked dispatcher: hour-long tick, drained only via Nudge, so the
	// two reconciler sweeps below race nothing.
	d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{
		Interval: time.Hour, Batch: 16,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	dctx, dcancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { dcancel(); <-done })
	go func() { defer close(done); d.Run(dctx) }()

	sched := &failingScheduler{}
	eng := newWorker(t, s, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(sched))
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())
	d.Nudge() // dispatch the entry step

	// Attempt 1 fails; the retry commits but its delayed write was lost.
	due := testNow.Add(time.Second)
	waitStep(t, s, runID, "flaky", "retrying", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusRetrying &&
			st.NextAttemptAt != nil && st.NextAttemptAt.Equal(due)
	})
	if h.DelayedLen(ctx) != 0 {
		t.Fatal("delayed entry exists despite the failing scheduler")
	}
	if sched.calls.Load() == 0 {
		t.Fatal("scheduler was never invoked")
	}

	// The reconciler heals once the due time is RetryStale in the past.
	retryStale := 30 * time.Second
	rec, err := engine.NewReconciler(s, engine.ReconcilerConfig{
		Interval:     time.Hour,
		ReadyStale:   time.Hour,
		RunningStale: time.Hour,
		RetryStale:   retryStale,
		Limit:        16,
	}, engine.WithReconcilerClock(clk.Now))
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	clk.Set(due.Add(retryStale).Add(time.Second))

	swept, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(swept.RetriesHealed) != 1 || swept.RetriesHealed[0].StepID != "flaky" {
		t.Fatalf("RetriesHealed = %+v, want exactly the stuck retry", swept.RetriesHealed)
	}
	// The heal is an outbox row with the dedicated reason, and the sweep
	// is idempotent while that row is pending (the anti-join).
	tasks, err := s.Outbox().List(ctx, 10)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Reason != store.OutboxReasonReconcileRetry {
		t.Fatalf("outbox = %+v, want one reconcile_retry row", tasks)
	}
	again, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("second ReconcileOnce: %v", err)
	}
	if len(again.RetriesHealed) != 0 {
		t.Errorf("second sweep healed %+v, want nothing (idempotent anti-join)", again.RetriesHealed)
	}

	// Drain the heal; the step is past due, so the claim admits it and
	// attempt 2 succeeds.
	d.Nudge()
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
	requireAttemptOutcomes(t, s, runID, "flaky", []string{
		store.AttemptOutcomeTransient, store.StepStatusSucceeded,
	})
	requireOutboxEmpty(t, s)
}
