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
	"build", "queue", "outbox", "dispatch", "reconcile", "step", "steplog", "run", "api", "worker", "ratelimit", "cache", "cost", "validate", "context", "approval",
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
	"resource": true, "plugin": true,
	"limit": true, "action": true, "trigger": true,
	"validator": true,
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
	w.EstimateError("anthropic:claude-sonnet-5", -128)
	w.ReconcileFailure()
	w.CacheHit("model_provider:anthropic")
	w.CacheMiss("model_provider:anthropic")
	w.CacheBypass("tool:http_request")
	w.CacheStore("tool:json_transform")
	w.CacheFailOpen()
	w.CostSpent("mock:sim-1", 2_000_000)
	w.CostSaved("mock:sim-1", 1_500_000)
	w.CostTokens("mock:sim-1", 1000, 500)
	w.BudgetExceeded("run", "park")
	w.ModelDowngraded("budget_threshold")
	w.ValidationVerdict("llm", "mock:sim-1", "fail")
	w.ValidatorResult("json_schema", "pass")
	w.SemanticRetryDepth("succeeded", 3)
	w.OutputRepair("repaired")
	w.JudgeScore("llm_judge", 0.9)
	w.ContextUtilization("mock:sim-1", 0.75)
	w.ContextWindowRejection("mock:small")
	w.SetApprovalPending(3)
	w.ApprovalDecided("reject", "timeout")
	w.ApprovalTimeout("rejected")
	a.Request("/v1/runs", "POST", 200, 20*time.Millisecond)
	a.RequestStarted()
	a.RequestFinished()
	a.ApprovalDecided("approve", "human")
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
	// Worker and API register on SEPARATE registries — mirroring production,
	// where each is a distinct process. They deliberately share one metric name
	// (engine_approval_decisions_total, ticket 15.4: recorded by the API on a
	// human decision and by the worker on a timeout), which a single registry
	// would reject as a duplicate; the conformance checks below run over the
	// union of both.
	regW, regA := prometheus.NewRegistry(), prometheus.NewRegistry()
	exercise(metrics.NewWorkerMetrics(regW), metrics.NewAPIMetrics(regA))

	famW, err := regW.Gather()
	if err != nil {
		t.Fatalf("Gather (worker): %v", err)
	}
	famA, err := regA.Gather()
	if err != nil {
		t.Fatalf("Gather (api): %v", err)
	}
	families := append(famW, famA...)
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
			// ADR-008 unit suffixes: durations in _seconds, token counts in
			// _tokens (the 9.3 estimate-error histogram), attempt counts in
			// _attempts and dimensionless ratios in _ratio (11.6's quality
			// histograms). A new unit is an ADR amendment first.
			if !strings.HasSuffix(name, "_seconds") && !strings.HasSuffix(name, "_tokens") &&
				!strings.HasSuffix(name, "_attempts") && !strings.HasSuffix(name, "_ratio") {
				t.Errorf("histogram %q must carry its unit suffix (ADR-008: durations _seconds, token counts _tokens, attempt counts _attempts, ratios _ratio)", name)
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
	// One build-info registry per deployable (worker / api), as production
	// wires them — the two sets share engine_approval_decisions_total (ticket
	// 15.4) and so cannot coexist on one registry.
	regW := metrics.NewRegistry(metrics.ServiceWorker)
	regA := metrics.NewRegistry(metrics.ServiceAPI)
	exercise(metrics.NewWorkerMetrics(regW), metrics.NewAPIMetrics(regA))
	if _, err := regW.Gather(); err != nil {
		t.Fatalf("Gather worker after full registration: %v", err)
	}
	if _, err := regA.Gather(); err != nil {
		t.Fatalf("Gather api after full registration: %v", err)
	}
}
