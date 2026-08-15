//go:build integration

package engine_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/limits"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 9.2's integration suite: the fleet-wide rate-limit middleware end to
// end — throttle as a delayed requeue (never a failure), the wait-vs-never
// permanent paths, fail-open, and the headline multi-worker property that N
// workers observe no more than the shared bucket allows at the provider.

// costBearingNoop is a noop-typed executor that names a fleet resource (so
// the limiter middleware governs it) and counts its executions — the probe
// proving throttled attempts never reach the provider.
type costBearingNoop struct {
	res   string
	est   int64
	calls atomic.Int64
	// onCall, when set, is invoked inside Execute (the "provider call") —
	// the fleet test records call timestamps through it.
	onCall func()
}

func (c *costBearingNoop) Type() string { return string(dag.StepNoop) }

func (c *costBearingNoop) ResourceClaim(exec.StepContext) (string, int64, error) {
	return c.res, c.est, nil
}

func (c *costBearingNoop) Execute(context.Context, exec.StepContext) (exec.Output, error) {
	c.calls.Add(1)
	if c.onCall != nil {
		c.onCall()
	}
	return exec.Output{}, nil
}

// scriptedLimiter is an injected ResourceLimiter whose per-acquire verdict is
// a function of the acquire ordinal — the deterministic way to drive a
// throttle, a permanent denial, or a fail-open without a real Redis bucket.
type scriptedLimiter struct {
	acquires atomic.Int64
	verdict  func(n int64) (resource.Decision, error)
}

func (l *scriptedLimiter) Acquire(_ context.Context, _ string, _ int64) (resource.Decision, error) {
	return l.verdict(l.acquires.Add(1))
}

// TestThrottleDeferAndComplete is 9.2's headline: the first acquire is denied
// (throttle), so the step routes running → retrying WITHOUT a counted attempt
// or a steps_failed bump, its delayed re-dispatch is scheduled under reason
// throttle at the clamped retry_after, and once promoted the second acquire
// grants and the run completes — attempt history exactly [throttled,
// succeeded], the executor invoked exactly once.
func TestThrottleDeferAndComplete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, soloDef)
	clk := newFakeClock(testNow)

	limiter := &scriptedLimiter{verdict: func(n int64) (resource.Decision, error) {
		if n == 1 {
			return resource.Decision{
				Limited: true, Allowed: false, Resource: "mock:sim-1",
				Bucket: "requests", RetryAfter: 2 * time.Second,
			}, nil
		}
		return resource.Decision{Limited: true, Allowed: true, Resource: "mock:sim-1"}, nil
	}}
	execu := &costBearingNoop{res: "mock:sim-1", est: 100}
	reg, err := exec.NewRegistry(execu)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()),
		engine.WithResourceLimiter(limiter),
		// Jitter off so the delay is exactly clamp(retry_after).
		engine.WithThrottleBackoff(500*time.Millisecond, 5*time.Minute, 0))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	// The first delivery is throttled: retrying at t0+2s with a delayed entry.
	due := testNow.Add(2 * time.Second)
	waitRetryScheduled(t, s, h, runID, "solo", due)

	// The budget is untouched — a throttle is not a counted failure.
	if got := countCountedFailures(t, s, runID, "solo"); got != 0 {
		t.Errorf("counted failures = %d after a throttle, want 0", got)
	}
	if execu.calls.Load() != 0 {
		t.Errorf("executor ran %d times before the grant, want 0", execu.calls.Load())
	}

	// Promote the throttle re-dispatch; the second acquire grants.
	clk.Set(due)
	if res := h.PromoteDue(ctx, due, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries at due time, want 1", res.Promoted)
	}
	run := waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireAttemptOutcomes(t, s, runID, "solo", []string{
		store.AttemptOutcomeThrottled, store.StepStatusSucceeded,
	})
	if run.StepsFailed != 0 || run.StepsSucceeded != 1 {
		t.Errorf("run counters = %d/%d (succeeded/failed), want 1/0 — a throttle is not a failure",
			run.StepsSucceeded, run.StepsFailed)
	}
	if execu.calls.Load() != 1 {
		t.Errorf("executor ran %d times, want exactly 1 (only the granted attempt)", execu.calls.Load())
	}
	if got := countEvents(t, s, runID, store.EventStepThrottled); got != 1 {
		t.Errorf("step_throttled events = %d, want 1", got)
	}
	if got := countEvents(t, s, runID, store.EventStepRetryScheduled); got != 0 {
		t.Errorf("step_retry_scheduled events = %d, want 0 (a throttle is not a retry)", got)
	}
	solo, err := s.Steps().Get(ctx, runID, "solo")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if solo.NextAttemptAt != nil {
		t.Errorf("next_attempt_at = %v after success, want cleared", solo.NextAttemptAt)
	}
	if h.DelayedLen(ctx) != 0 {
		t.Error("delayed set not empty after completion")
	}
	requireOutboxEmpty(t, s)
}

// TestThrottlePermanentPaths: the two denials no wait can lift — an estimate
// larger than the bucket (ErrCostExceedsCapacity) and a never-refilling
// bucket (RetryAfterNever) — perm-fail to the DLQ (source permanent) rather
// than throttling forever (ADR-010 wait-vs-never).
func TestThrottlePermanentPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		verdict func(int64) (resource.Decision, error)
	}{
		{"cost exceeds capacity", func(int64) (resource.Decision, error) {
			return resource.Decision{}, resource.ErrCostExceedsCapacity
		}},
		{"never refills", func(int64) (resource.Decision, error) {
			return resource.Decision{
				Limited: true, Allowed: false, Resource: "mock:sim-1",
				Bucket: "tokens", RetryAfter: resource.RetryAfterNever,
			}, nil
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s, h, runID := setup(t, soloDef)
			clk := newFakeClock(testNow)
			limiter := &scriptedLimiter{verdict: func(n int64) (resource.Decision, error) { return tc.verdict(n) }}
			execu := &costBearingNoop{res: "mock:sim-1", est: 100}
			reg, err := exec.NewRegistry(execu)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			d := startDispatcher(t, s, h.Queue())
			eng, err := engine.New(s, reg, "worker-a",
				engine.WithDispatchNudge(d.Nudge),
				engine.WithClock(clk.Now),
				engine.WithRetryScheduler(h.Delayed()),
				engine.WithResourceLimiter(limiter))
			if err != nil {
				t.Fatalf("engine.New: %v", err)
			}
			h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

			waitRun(t, s, runID, store.RunStatusFailed)
			h.WaitQuiescent(ctx)
			h.RequireHandledOncePerClaim()

			if execu.calls.Load() != 0 {
				t.Errorf("executor ran %d times, want 0 (perm-failed before execution)", execu.calls.Load())
			}
			requireAttemptOutcomes(t, s, runID, "solo", []string{store.AttemptOutcomePermanent})
			dls, err := s.DeadLetters().ListByStep(ctx, runID, "solo")
			if err != nil || len(dls) != 1 || dls[0].Source != store.DeadLetterSourcePermanent {
				t.Errorf("dead letters = %v (err %v), want one permanent row", dls, err)
			}
			if h.DelayedLen(ctx) != 0 {
				t.Error("delayed set not empty — a permanent throttle must not reschedule")
			}
		})
	}
}

// TestThrottleFailOpen: a limiter error (e.g. Redis unreachable) must not
// stall the step — it proceeds to the provider without a limit (ADR-010: a
// limit is never a correctness dependency).
func TestThrottleFailOpen(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, soloDef)
	clk := newFakeClock(testNow)
	limiter := &scriptedLimiter{verdict: func(int64) (resource.Decision, error) {
		return resource.Decision{}, errors.New("redis unreachable")
	}}
	execu := &costBearingNoop{res: "mock:sim-1", est: 100}
	reg, err := exec.NewRegistry(execu)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()),
		engine.WithResourceLimiter(limiter))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	if execu.calls.Load() != 1 {
		t.Errorf("executor ran %d times, want 1 (fail-open proceeds)", execu.calls.Load())
	}
	requireAttemptOutcomes(t, s, runID, "solo", []string{store.StepStatusSucceeded})
}

// TestFleetRespectsResourceLimit is 9.2's exit criterion: a fleet of workers
// sharing one real Redis resource bucket never exceeds it at the provider.
// Each of many runs makes one governed "provider call"; the workers throttle
// and re-dispatch under contention on the shared ledger. The token-bucket
// guarantee — cumulative calls ≤ burst + refill × elapsed — is asserted
// against every recorded call, and every run eventually completes.
func TestFleetRespectsResourceLimit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const (
		runs       = 24
		burst      = 5
		perMinute  = 600.0 // 10 tokens/sec
		refillPerS = perMinute / 60
	)
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	// One shared real limiter over the harness's Redis: a requests-only
	// resource with a small burst so contention forces throttling.
	set, err := limits.Parse([]byte(`{"resources":[{"name":"mock:sim-1","requests":{"per_minute":600,"burst":5}}]}`))
	if err != nil {
		t.Fatalf("limits.Parse: %v", err)
	}
	keyPrefix := "agentloom-test:fleet:" + uuid.NewString()
	t.Cleanup(func() {
		keys, _ := h.Client().Keys(context.Background(), keyPrefix+"*").Result()
		if len(keys) > 0 {
			h.Client().Del(context.Background(), keys...) //nolint:errcheck // best-effort
		}
	})
	limiter, err := resource.New(h.Client(), set, keyPrefix)
	if err != nil {
		t.Fatalf("resource.New: %v", err)
	}

	// Record every provider call's elapsed time from a fixed start.
	start := time.Now()
	var mu sync.Mutex
	var callTimes []time.Duration
	record := func() {
		mu.Lock()
		callTimes = append(callTimes, time.Since(start))
		mu.Unlock()
	}

	// A short promoter tick so throttled re-dispatches promote quickly; the
	// engine runs on real time so next_attempt_at and refill stay consistent.
	consumerCfg := queue.ConsumerConfig{
		Block: 100 * time.Millisecond, Batch: 4,
		PromoterTick: 50 * time.Millisecond, DelayedKey: h.Delayed().Key(),
	}
	d := startDispatcher(t, s, h.Queue())
	var execs []*costBearingNoop
	for i := 0; i < 4; i++ {
		execu := &costBearingNoop{res: "mock:sim-1", est: 0, onCall: record}
		execs = append(execs, execu)
		reg, err := exec.NewRegistry(execu)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		eng, err := engine.New(s, reg, "", // consumer name assigned by Spawn
			engine.WithDispatchNudge(d.Nudge),
			engine.WithRetryScheduler(h.Delayed()),
			engine.WithResourceLimiter(limiter),
			engine.WithThrottleBackoff(100*time.Millisecond, 5*time.Second, 0))
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		h.Spawn(uuidName(i), eng.Handle, consumerCfg)
	}

	// Submit the runs; instantiation outboxes each entry step, the dispatcher
	// drains them onto the ready stream.
	def, err := dag.Decode([]byte(soloDef))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	runIDs := make([]uuid.UUID, runs)
	for i := 0; i < runs; i++ {
		res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		runIDs[i] = res.Run.ID
	}
	d.Nudge()

	// Every run eventually completes despite the contention.
	for _, id := range runIDs {
		waitRun(t, s, id, store.RunStatusSucceeded)
	}
	h.WaitQuiescent(ctx)

	// Exactly `runs` provider calls total — throttles never call.
	var total int64
	for _, e := range execs {
		total += e.calls.Load()
	}
	if total != runs {
		t.Errorf("total provider calls = %d, want %d (throttles must not call)", total, runs)
	}

	// The token-bucket guarantee: the n-th call (1-based) cannot have happened
	// before the bucket held n tokens — cumulative calls ≤ burst + refill ×
	// elapsed. A small epsilon absorbs scheduling and clock slop. This is the
	// "N workers ≤ R calls" property the milestone requires.
	mu.Lock()
	defer mu.Unlock()
	const eps = 2.0
	for i, at := range callTimes {
		n := float64(i + 1)
		bound := burst + refillPerS*at.Seconds() + eps
		if n > bound {
			t.Errorf("call #%d at %v exceeds the shared bucket: %d > burst(%d)+refill(%.3f)*%.3fs+eps(%.0f)=%.2f",
				i+1, at, i+1, burst, refillPerS, at.Seconds(), eps, bound)
		}
	}
}

// uuidName gives each spawned worker a distinct consumer name.
func uuidName(i int) string { return "fleet-worker-" + strconv.Itoa(i) }

// countCountedFailures reads the step's retry-budget counter (transient +
// timeout attempts) — the count a throttle must leave unchanged.
func countCountedFailures(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) int64 {
	t.Helper()
	n, err := s.Attempts().CountCountedFailures(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("CountCountedFailures: %v", err)
	}
	return n
}
