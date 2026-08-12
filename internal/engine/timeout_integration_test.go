//go:build integration

package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 5.3's integration suite: step execution timeouts end to end.
// The watchdog deadline is genuinely wall-clock-bound (context.WithTimeout),
// so these tests follow the queue package's timing convention — small real
// durations, with the sleep far exceeding the timeout so the ordering can
// never flake — while the backoff schedule stays on 5.2's injected clock.

// timeoutDef sleeps 10s under a 100ms timeout — every attempt times out —
// with a two-attempt budget and a deterministic 1s backoff.
const timeoutDef = `{
	"schema_version": 1,
	"name": "timeout-doomed",
	"steps": [
		{"id": "slow", "type": "sleep", "config": {"duration": "10s"},
		 "timeout": "100ms",
		 "retry": {"max_attempts": 2, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}},
		{"id": "never", "type": "noop"}
	],
	"edges": [{"from": "slow", "to": "never"}]
}`

// TestTimeoutRetriesPerPolicy is 5.3's headline acceptance: sleep(10s)
// under timeout=100ms records a `timeout` attempt and routes through the
// retry engine — retrying with next_attempt_at honoring the backoff, then
// exhaustion onto the terminal path with the full classed history. The
// timeout is distinguishable from a crash throughout: outcomes are
// `timeout` (a live worker's judgment), never `lost`, and no
// step_reclaimed event exists because the heartbeat kept the lease for
// the whole (cancelled) execution.
func TestTimeoutRetriesPerPolicy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, timeoutDef)
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()))
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	// Attempt 1 is cancelled at the 100ms deadline; the first counted
	// failure schedules retry 1 at t0+1s (initial, no jitter).
	due1 := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "slow", due1)

	// Fire the retry. Attempt 2 times out as well; the two-attempt budget
	// is exhausted and the failure lands terminal.
	clk.Set(due1)
	if res := h.PromoteDue(ctx, due1, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries at due time, want 1", res.Promoted)
	}
	run := waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"slow": store.StepStatusFailed, "never": store.StepStatusPending,
	})
	requireAttemptOutcomes(t, s, runID, "slow", []string{
		store.AttemptOutcomeTimeout, store.AttemptOutcomeTimeout,
	})
	if run.StepsFailed != 1 {
		t.Errorf("steps_failed = %d, want 1", run.StepsFailed)
	}

	// Timeout ≠ crash: no takeover happened, no attempt closed as lost.
	if got := countEvents(t, s, runID, store.EventStepReclaimed); got != 0 {
		t.Errorf("step_reclaimed events = %d, want 0 — a timeout must not look like a crash", got)
	}
	if got := countEvents(t, s, runID, store.EventStepRetryScheduled); got != 1 {
		t.Errorf("step_retry_scheduled events = %d, want 1", got)
	}

	slow, err := s.Steps().Get(ctx, runID, "slow")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if !strings.Contains(string(slow.Error), "timed out after 100ms") ||
		!strings.Contains(string(slow.Error), store.AttemptOutcomeTimeout) {
		t.Errorf("step error = %s, want the timeout message and the timeout class recorded", slow.Error)
	}
	if h.DelayedLen(ctx) != 0 {
		t.Error("delayed set not empty after exhaustion")
	}
	requireOutboxEmpty(t, s)
}

// unexpiredDef sleeps well under its generous timeout.
const unexpiredDef = `{
	"schema_version": 1,
	"name": "timeout-unexpired",
	"steps": [
		{"id": "quick", "type": "sleep", "config": {"duration": "20ms"}, "timeout": "5s"}
	],
	"edges": []
}`

// TestTimeoutNotHitIsInert: a configured timeout that never fires changes
// nothing — the step succeeds on attempt 1 with no retry machinery
// engaged.
func TestTimeoutNotHitIsInert(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, unexpiredDef)
	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a", engine.WithDispatchNudge(d.Nudge))
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireAttemptOutcomes(t, s, runID, "quick", []string{store.StepStatusSucceeded})
	if got := countEvents(t, s, runID, store.EventStepRetryScheduled); got != 0 {
		t.Errorf("step_retry_scheduled events = %d, want 0", got)
	}
	requireOutboxEmpty(t, s)
}
