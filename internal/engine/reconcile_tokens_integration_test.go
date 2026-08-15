//go:build integration

package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/limits"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 9.3's integration suite: post-call token-cost reconciliation. After a
// granted, token-metered call reports its real usage, the middleware corrects
// the token bucket by actual − estimate — so a biased estimator cannot drift
// the fleet past the provider's real tokens/min budget over time.

// TestReconcileAppliedAfterMeteredCall: a granted acquire that debited the
// token bucket is reconciled once the call returns — the middleware passes the
// exact (estimate, actual) to the limiter and records the signed error on the
// estimate-error histogram, labeled by the resolved resource.
func TestReconcileAppliedAfterMeteredCall(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, runID := setup(t, soloDef)

	limiter := &scriptedLimiter{verdict: func(int64) (resource.Decision, error) {
		return resource.Decision{
			Limited: true, Allowed: true, Resource: "mock:sim-1", TokensMetered: true,
		}, nil
	}}
	// Estimate 100 (from ResourceClaim), the call actually uses 150 → +50 error.
	execu := &costBearingNoop{res: "mock:sim-1", est: 100, actualTokens: 150}
	reg, err := exec.NewRegistry(execu)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	mreg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(mreg)
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithClock(func() time.Time { return testNow }),
		engine.WithResourceLimiter(limiter),
		engine.WithMetrics(wm))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	if err := eng.Handle(ctx, queue.Delivery{ID: "1-1", Envelope: stepEnvelope(runID, "solo"), DeliveryCount: 1}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)

	calls := limiter.reconcileCalls()
	if len(calls) != 1 {
		t.Fatalf("reconcile calls = %d, want exactly 1", len(calls))
	}
	if calls[0].resource != "mock:sim-1" || calls[0].est != 100 || calls[0].actual != 150 {
		t.Errorf("reconcile call = %+v, want {mock:sim-1 100 150}", calls[0])
	}
	// The estimate-error histogram observed exactly one +50 sample.
	hist := histogramOf(t, mreg, "engine_ratelimit_estimate_error_tokens", map[string]string{"resource": "mock:sim-1"})
	if hist == nil {
		t.Fatal("estimate-error histogram has no samples after a metered call")
	}
	if got := hist.GetSampleCount(); got != 1 {
		t.Errorf("estimate-error observations = %d, want 1", got)
	}
	if got := hist.GetSampleSum(); got != 50 {
		t.Errorf("estimate-error sum = %v, want 50 (actual 150 − estimate 100)", got)
	}
}

// TestReconcileSkippedWhenTokensNotMetered: a granted acquire that did NOT
// debit the token bucket — a requests-only or unlimited resource — is never
// reconciled; there is nothing on the token ledger to correct.
func TestReconcileSkippedWhenTokensNotMetered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, runID := setup(t, soloDef)

	limiter := &scriptedLimiter{verdict: func(int64) (resource.Decision, error) {
		// Granted, but TokensMetered stays false (requests-only / tool claim).
		return resource.Decision{Limited: true, Allowed: true, Resource: "tool:x"}, nil
	}}
	// est 0 (a tool-style claim), and even a usage report must not trigger a
	// reconcile since no token bucket was touched.
	execu := &costBearingNoop{res: "tool:x", est: 0, actualTokens: 42}
	reg, err := exec.NewRegistry(execu)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithClock(func() time.Time { return testNow }),
		engine.WithResourceLimiter(limiter))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	if err := eng.Handle(ctx, queue.Delivery{ID: "1-1", Envelope: stepEnvelope(runID, "solo"), DeliveryCount: 1}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)

	if calls := limiter.reconcileCalls(); len(calls) != 0 {
		t.Errorf("reconcile calls = %d, want 0 (tokens were never metered)", len(calls))
	}
}

// TestReconcileFailOpen: a reconciliation error (e.g. Redis unreachable) never
// affects the step — it succeeds — and is counted so operators can see the
// ledger correction was skipped (ADR-010 fail-open).
func TestReconcileFailOpen(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, runID := setup(t, soloDef)

	limiter := &scriptedLimiter{
		verdict: func(int64) (resource.Decision, error) {
			return resource.Decision{Limited: true, Allowed: true, Resource: "mock:sim-1", TokensMetered: true}, nil
		},
		reconcileErr: errors.New("redis unreachable"),
	}
	execu := &costBearingNoop{res: "mock:sim-1", est: 100, actualTokens: 150}
	reg, err := exec.NewRegistry(execu)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	mreg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(mreg)
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithClock(func() time.Time { return testNow }),
		engine.WithResourceLimiter(limiter),
		engine.WithMetrics(wm))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	if err := eng.Handle(ctx, queue.Delivery{ID: "1-1", Envelope: stepEnvelope(runID, "solo"), DeliveryCount: 1}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// The step still succeeds despite the reconcile failure.
	waitRun(t, s, runID, store.RunStatusSucceeded)
	if got := counterValue(t, mreg, "engine_ratelimit_reconcile_failures_total", nil); got != 1 {
		t.Errorf("reconcile_failures_total = %v, want 1", got)
	}
}

// TestFleetActualTokensRespectResourceLimit is 9.3's acceptance criterion:
// under sustained load with a biased-low estimator, the fleet's real token
// consumption stays within the configured tokens/min. Each governed call
// estimates `est` tokens but actually uses `actual` = 3× that; without
// reconciliation the bucket would gate on the estimate and the fleet would
// consume ~3× its token budget. The post-call reconciliation debits the
// shortfall, so the cumulative ACTUAL tokens obey the token-bucket bound —
// cumulative ≤ burst + refill × elapsed + a bounded in-flight slack.
func TestFleetActualTokensRespectResourceLimit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const (
		runs       = 24
		workers    = 4
		est        = 10
		actual     = 30 // biased 3× low
		burst      = 200
		perMinute  = 6000.0 // 100 tokens/sec
		refillPerS = perMinute / 60
	)
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	// One shared real limiter over the harness's Redis: a tokens-only resource.
	set, err := limits.Parse([]byte(`{"resources":[{"name":"mock:sim-1","tokens":{"per_minute":6000,"burst":200}}]}`))
	if err != nil {
		t.Fatalf("limits.Parse: %v", err)
	}
	keyPrefix := "agentloom-test:reconcile-fleet:" + uuid.NewString()
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

	// Record the cumulative ACTUAL tokens at each provider call, with its
	// elapsed time from a fixed start.
	start := time.Now()
	var mu sync.Mutex
	type sample struct {
		at        time.Duration
		cumActual int64
	}
	var samples []sample
	var cumActual int64
	record := func() {
		mu.Lock()
		cumActual += actual
		samples = append(samples, sample{at: time.Since(start), cumActual: cumActual})
		mu.Unlock()
	}

	consumerCfg := queue.ConsumerConfig{
		Block: 100 * time.Millisecond, Batch: 4,
		PromoterTick: 50 * time.Millisecond, DelayedKey: h.Delayed().Key(),
	}
	d := startDispatcher(t, s, h.Queue())
	var execs []*costBearingNoop
	for i := 0; i < workers; i++ {
		execu := &costBearingNoop{res: "mock:sim-1", est: est, actualTokens: actual, onCall: record}
		execs = append(execs, execu)
		reg, err := exec.NewRegistry(execu)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		eng, err := engine.New(s, reg, "",
			engine.WithDispatchNudge(d.Nudge),
			engine.WithRetryScheduler(h.Delayed()),
			engine.WithResourceLimiter(limiter),
			engine.WithThrottleBackoff(100*time.Millisecond, 5*time.Second, 0))
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		h.Spawn(uuidName(i), eng.Handle, consumerCfg)
	}

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

	for _, id := range runIDs {
		waitRun(t, s, id, store.RunStatusSucceeded)
	}
	h.WaitQuiescent(ctx)

	// Exactly `runs` provider calls, each consuming `actual` tokens.
	var total int64
	for _, e := range execs {
		total += e.calls.Load()
	}
	if total != runs {
		t.Errorf("total provider calls = %d, want %d", total, runs)
	}

	// The token-bucket guarantee on ACTUAL consumption: the fleet cannot have
	// spent more real tokens than the shared bucket held. The in-flight slack
	// absorbs the window between a call's estimate-debit (at acquire) and its
	// shortfall-debit (at reconcile): up to `workers` calls can each be
	// under-debited by (actual − est) at once. Without reconciliation this
	// bound would be violated 3× over (the bucket would gate on est=10, not
	// actual=30).
	slack := float64(workers*(actual-est)) + actual
	mu.Lock()
	defer mu.Unlock()
	for _, sm := range samples {
		bound := burst + refillPerS*sm.at.Seconds() + slack
		if float64(sm.cumActual) > bound {
			t.Errorf("cumulative actual tokens %d at %v exceeds the shared bucket: %d > burst(%d)+refill(%.3f)*%.3fs+slack(%.0f)=%.2f",
				sm.cumActual, sm.at, sm.cumActual, burst, refillPerS, sm.at.Seconds(), slack, bound)
		}
	}
}
