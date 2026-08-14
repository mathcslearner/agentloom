//go:build integration

package engine_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 7.2's integration suite: the engine metrics recorded against real
// Postgres state, on fully injected clocks so latency observations are
// exact — the scheduling-latency histogram is proven against a scripted
// delay (the ticket's third acceptance criterion), not a sleep.

// findMetric returns the sample of name whose labels are a superset of
// want, or nil when absent.
func findMetric(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) *dto.Metric {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
	metric:
		for _, m := range fam.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			for k, v := range want {
				if labels[k] != v {
					continue metric
				}
			}
			return m
		}
	}
	return nil
}

// counterValue returns the counter's value, 0 when the series does not
// exist yet.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// histogramOf returns the histogram sample, or nil when the series does
// not exist yet.
func histogramOf(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) *dto.Histogram {
	t.Helper()
	m := findMetric(t, reg, name, labels)
	if m == nil {
		return nil
	}
	return m.GetHistogram()
}

// newMeteredEngine builds an engine over the full Builtins registry with
// the given clock and a fresh Prometheus-backed WorkerMetrics.
func newMeteredEngine(t *testing.T, s *store.Store, clock func() time.Time) (*engine.Engine, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(reg)
	e, err := engine.New(s, exec.Builtins(nil, nil, nil), "metered-worker",
		engine.WithClock(clock),
		engine.WithMetrics(wm))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e, reg
}

// TestSchedulingLatencyScriptedDelay is the acceptance proof: the run's
// entry step turns ready at testNow (the instantiation clock); the worker
// claims it exactly 7s later on its own injected clock. The
// ready→running histogram must record exactly one observation of exactly
// 7 seconds — no tolerance, both clocks are scripted. The duplicate
// delivery afterwards must land in the ack_drop counter without touching
// the histogram.
func TestSchedulingLatencyScriptedDelay(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, runID := setup(t, singleNoop)

	const scriptedDelay = 7 * time.Second
	e, reg := newMeteredEngine(t, s, func() time.Time { return testNow.Add(scriptedDelay) })

	d := queue.Delivery{ID: "1-1", Envelope: stepEnvelope(runID, "only"), DeliveryCount: 1}
	if err := e.Handle(ctx, d); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	hist := histogramOf(t, reg, "engine_step_scheduling_latency_seconds", nil)
	if hist == nil {
		t.Fatal("engine_step_scheduling_latency_seconds has no samples after a won claim of a ready step")
	}
	if got := hist.GetSampleCount(); got != 1 {
		t.Errorf("scheduling latency observations = %d, want exactly 1", got)
	}
	if got := hist.GetSampleSum(); got != scriptedDelay.Seconds() {
		t.Errorf("scheduling latency sum = %vs, want exactly %vs (both clocks are injected)", got, scriptedDelay.Seconds())
	}
	if got := counterValue(t, reg, "engine_step_claims_total", map[string]string{"result": "won"}); got != 1 {
		t.Errorf("claims{result=won} = %v, want 1", got)
	}
	stepHist := histogramOf(t, reg, "engine_step_duration_seconds",
		map[string]string{"step_type": "noop", "outcome": "succeeded"})
	if stepHist == nil || stepHist.GetSampleCount() != 1 {
		t.Errorf("step duration{noop,succeeded} observations = %v, want 1", stepHist.GetSampleCount())
	}
	// The single-step run terminalized in the same completion: completion
	// latency = claim clock − run created_at = the scripted 7s exactly.
	runHist := histogramOf(t, reg, "engine_run_duration_seconds", map[string]string{"status": store.RunStatusSucceeded})
	if runHist == nil {
		t.Fatal("engine_run_duration_seconds{status=succeeded} has no samples after the run rolled up")
	}
	if got := runHist.GetSampleSum(); got != scriptedDelay.Seconds() {
		t.Errorf("run duration sum = %vs, want exactly %vs", got, scriptedDelay.Seconds())
	}

	// A duplicate of the finished work: ack_drop counted, histogram
	// untouched.
	if err := e.Handle(ctx, d); err != nil {
		t.Fatalf("Handle (duplicate): %v", err)
	}
	if got := counterValue(t, reg, "engine_step_claims_total", map[string]string{"result": "ack_drop"}); got != 1 {
		t.Errorf("claims{result=ack_drop} = %v, want 1", got)
	}
	if got := histogramOf(t, reg, "engine_step_scheduling_latency_seconds", nil).GetSampleCount(); got != 1 {
		t.Errorf("scheduling latency observations after duplicate = %d, want still 1", got)
	}
}

// retryMeteredDef fails forever under a two-attempt budget: attempt 1
// routes to retry (transient), attempt 2 exhausts the budget and
// dead-letters with source retries_exhausted, failing the run (fail_fast).
const retryMeteredDef = `{
	"schema_version": 1,
	"name": "metrics-retry",
	"steps": [
		{"id": "doomed", "type": "fail_n_times", "config": {"n": 5},
		 "retry": {"max_attempts": 2, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}}
	],
	"edges": []
}`

// TestRetryAndDeadLetterMetrics drives one step through retry exhaustion
// on a scripted clock: the retry counter, the per-class step durations,
// the DLQ counter, the run-failure completion latency, and the
// no-scheduling-latency-for-retrying-claims rule.
func TestRetryAndDeadLetterMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, runID := setup(t, retryMeteredDef)

	now := testNow.Add(3 * time.Second)
	e, reg := newMeteredEngine(t, s, func() time.Time { return now })

	// Attempt 1 at +3s: transient failure, retry scheduled at +4s (initial
	// 1s, jitter none). No retry scheduler is wired — the durable row alone
	// carries the retry, which is all this test needs.
	d := queue.Delivery{ID: "1-1", Envelope: stepEnvelope(runID, "doomed"), DeliveryCount: 1}
	if err := e.Handle(ctx, d); err != nil {
		t.Fatalf("Handle attempt 1: %v", err)
	}
	if got := counterValue(t, reg, "engine_step_retries_total", map[string]string{"class": "transient"}); got != 1 {
		t.Errorf("retries{class=transient} = %v, want 1", got)
	}

	// Attempt 2 at +9s (past the 1s backoff): budget of 2 exhausted →
	// dead-letter, fail_fast fails the run. Run completion latency = 9s
	// from the instantiation clock, exactly.
	now = testNow.Add(9 * time.Second)
	d2 := queue.Delivery{ID: "1-2", Envelope: stepEnvelope(runID, "doomed"), DeliveryCount: 1}
	if err := e.Handle(ctx, d2); err != nil {
		t.Fatalf("Handle attempt 2: %v", err)
	}

	if got := counterValue(t, reg, "engine_step_dead_letters_total", map[string]string{"source": store.DeadLetterSourceRetriesExhausted}); got != 1 {
		t.Errorf("dead_letters{source=retries_exhausted} = %v, want 1", got)
	}
	stepHist := histogramOf(t, reg, "engine_step_duration_seconds",
		map[string]string{"step_type": "fail_n_times", "outcome": "transient"})
	if stepHist == nil || stepHist.GetSampleCount() != 2 {
		t.Errorf("step duration{fail_n_times,transient} observations = %v, want 2 (both judged attempts)", stepHist.GetSampleCount())
	}
	runHist := histogramOf(t, reg, "engine_run_duration_seconds", map[string]string{"status": store.RunStatusFailed})
	if runHist == nil {
		t.Fatal("engine_run_duration_seconds{status=failed} has no samples after fail_fast")
	}
	if got := runHist.GetSampleSum(); got != 9.0 {
		t.Errorf("run duration sum = %vs, want exactly 9s", got)
	}
	// The retrying→running claim must NOT count as scheduling latency —
	// its interval includes the deliberate backoff. Only the first (ready)
	// claim observed.
	sched := histogramOf(t, reg, "engine_step_scheduling_latency_seconds", nil)
	if sched == nil {
		t.Fatal("engine_step_scheduling_latency_seconds has no samples")
	}
	if got := sched.GetSampleCount(); got != 1 {
		t.Errorf("scheduling latency observations = %d, want 1 (the ready claim only, never the retrying one)", got)
	}
	if got := sched.GetSampleSum(); got != 3.0 {
		t.Errorf("scheduling latency sum = %vs, want exactly 3s (the first claim's delay)", got)
	}
	if got := counterValue(t, reg, "engine_step_claims_total", map[string]string{"result": "won"}); got != 2 {
		t.Errorf("claims{result=won} = %v, want 2", got)
	}
}

// TestDispatchMetrics drains the run-instantiation outbox row through a
// metered Dispatcher: one dispatched_total{reason=step_ready} increment
// and one drain-lag observation per row.
func TestDispatchMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, _ := setup(t, singleNoop)

	reg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(reg)
	dispatcher, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{Interval: time.Hour, Batch: 16},
		engine.WithDispatcherMetrics(wm))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	n, err := dispatcher.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("DrainOnce dispatched %d rows, want 1 (the entry step)", n)
	}

	if got := counterValue(t, reg, "engine_dispatch_dispatched_total", map[string]string{"reason": queue.ReasonStepReady}); got != 1 {
		t.Errorf("dispatched{reason=step_ready} = %v, want 1", got)
	}
	lag := histogramOf(t, reg, "engine_dispatch_lag_seconds", nil)
	if lag == nil || lag.GetSampleCount() != 1 {
		t.Errorf("dispatch lag observations = %v, want 1", lag.GetSampleCount())
	}
}

// TestReconcileHealMetrics provokes the lost-dispatch heal (a ready step
// past ReadyStale with no outbox row) and asserts the healed counter.
func TestReconcileHealMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, runID := setup(t, singleNoop)

	// Simulate the lost dispatch: drop the instantiation's outbox row.
	tasks, err := s.Outbox().List(ctx, 16)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	ids := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	if _, err := s.Outbox().Delete(ctx, ids); err != nil {
		t.Fatalf("deleting outbox rows: %v", err)
	}

	reg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(reg)
	rec, err := engine.NewReconciler(s, engine.ReconcilerConfig{
		Interval:   time.Hour,
		ReadyStale: time.Minute, RunningStale: time.Hour, RetryStale: time.Hour,
		Limit: 16,
	}, engine.WithReconcilerClock(func() time.Time { return testNow.Add(2 * time.Minute) }),
		engine.WithReconcilerMetrics(wm))
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	res, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(res.Requeued) != 1 {
		t.Fatalf("Requeued = %d steps, want 1 (run %s step \"only\")", len(res.Requeued), runID)
	}
	if got := counterValue(t, reg, "engine_reconcile_healed_total", map[string]string{"reason": store.OutboxReasonReconcileReady}); got != 1 {
		t.Errorf("reconcile healed{reason=reconcile_ready} = %v, want 1", got)
	}
}
