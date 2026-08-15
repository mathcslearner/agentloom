package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec/steplog"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/queue"
)

// The instrument structs must keep satisfying the producing packages'
// seams structurally — a signature drift on either side fails compilation
// right here rather than at cmd wiring time.
var (
	_ engine.Metrics        = (*metrics.WorkerMetrics)(nil)
	_ queue.ConsumerMetrics = (*metrics.WorkerMetrics)(nil)
	_ steplog.Metrics       = (*metrics.WorkerMetrics)(nil)
	_ api.RequestMetrics    = (*metrics.APIMetrics)(nil)
	_ api.RateLimitMetrics  = (*metrics.APIMetrics)(nil)
)

// allowedSubsystems is ADR-008's subsystem vocabulary; extending it is an
// ADR amendment first.
var allowedSubsystems = []string{
	"build", "queue", "outbox", "dispatch", "reconcile", "step", "steplog", "run", "api", "worker", "ratelimit",
}

// allowedLabels is ADR-008's label allowlist. run_id, step_id, attempt,
// claim_id, worker_id, key_id, raw paths, and error strings must never
// appear here — they are log and trace fields.
var allowedLabels = map[string]bool{
	"service": true, "version": true,
	"step_type": true, "outcome": true, "status": true,
	"reason": true, "class": true, "source": true,
	"route": true, "method": true, "code": true,
	"duty": true, "result": true, "bucket": true, "decision": true,
	"resource": true,
}

// exercise touches every instrument at least once so vec children exist
// and every declared label key reaches the gathered output.
func exercise(w *metrics.WorkerMetrics, a *metrics.APIMetrics) {
	w.Reclaimed(1)
	w.PoisonDiverted()
	w.Promoted(2, 50*time.Millisecond)
	w.ClaimDecision(metrics.ClaimResultWon)
	w.SchedulingLatency(5 * time.Millisecond)
	w.StepDuration("noop", "succeeded", time.Millisecond)
	w.RetryScheduled("transient")
	w.Takeover()
	w.FencingRejection()
	w.DeadLetter("permanent")
	w.RunCompleted("succeeded", time.Second)
	w.Dispatched("step_ready", 10*time.Millisecond)
	w.ReconcileHealed("reconcile_ready", 1)
	w.SetQueueDepths(1, 2, 3, 4)
	w.SetOutbox(5, time.Second)
	w.SetActiveWorkers(2)
	w.StepLogCaptured(3)
	w.StepLogDropped(1)
	w.StepLogFlushFailure()
	w.Throttled("anthropic:claude-sonnet-5", "requests")
	w.ThrottleWait("anthropic:claude-sonnet-5", 2*time.Second)
	w.RateLimitFailOpen()
	a.Request("/v1/runs", "POST", 200, 20*time.Millisecond)
	a.RequestStarted()
	a.RequestFinished()
	a.Decision("submit", false, true)
	a.Decision("submit", true, false)
	a.FailOpen("read")
}

// TestInstrumentConformance is ADR-008's cardinality audit in executable
// form: every engine_* metric name uses an allowed subsystem and unit
// suffix, and every label key is in the allowlist. A new metric or label
// that skips the ADR tables fails here.
func TestInstrumentConformance(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	exercise(metrics.NewWorkerMetrics(reg), metrics.NewAPIMetrics(reg))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("no metric families gathered")
	}
	for _, fam := range families {
		name := fam.GetName()
		rest, ok := strings.CutPrefix(name, metrics.Namespace+"_")
		if !ok {
			t.Errorf("metric %q outside the %s_ namespace", name, metrics.Namespace)
			continue
		}
		subsystem, _, ok := strings.Cut(rest, "_")
		if !ok {
			t.Errorf("metric %q has no subsystem segment (want %s_<subsystem>_<name>)", name, metrics.Namespace)
			continue
		}
		found := false
		for _, s := range allowedSubsystems {
			if subsystem == s {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metric %q uses subsystem %q, not in ADR-008's vocabulary %v", name, subsystem, allowedSubsystems)
		}
		switch fam.GetType().String() {
		case "COUNTER":
			if !strings.HasSuffix(name, "_total") {
				t.Errorf("counter %q must end in _total (ADR-008)", name)
			}
		case "HISTOGRAM":
			if !strings.HasSuffix(name, "_seconds") {
				t.Errorf("histogram %q must carry its unit suffix (ADR-008: durations in _seconds)", name)
			}
		case "GAUGE":
			if strings.HasSuffix(name, "_total") {
				t.Errorf("gauge %q must not use the counter suffix (ADR-008)", name)
			}
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if !allowedLabels[lp.GetName()] {
					t.Errorf("metric %q carries label %q, not in ADR-008's allowlist", name, lp.GetName())
				}
			}
		}
	}
}

// TestInstrumentsRegisterOnBuildInfoRegistry proves the instrument sets
// coexist with NewRegistry's collectors (build info + Go runtime) — the
// exact wiring both deployables use — without duplicate registration.
func TestInstrumentsRegisterOnBuildInfoRegistry(t *testing.T) {
	t.Parallel()
	reg := metrics.NewRegistry(metrics.ServiceWorker)
	exercise(metrics.NewWorkerMetrics(reg), metrics.NewAPIMetrics(reg))
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("Gather after full registration: %v", err)
	}
}
