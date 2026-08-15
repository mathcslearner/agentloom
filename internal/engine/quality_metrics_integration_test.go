//go:build integration

package engine_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 11.6: the output-quality metrics. Verdict pass/fail counts by step
// type and resource, per-validator results, the semantic-retry depth
// histogram, the structured-output repair rate, and the llm-judge score
// distribution — recorded post-commit, exercised end-to-end on the offline
// mock. These are the "metrics emitted per conventions" acceptance criterion.

// meteredValidationEngine wires a store, harness, dispatcher, llm executor
// (over the given provider registry), and validator registry into one engine
// with a fresh Prometheus-backed WorkerMetrics, returning the store, harness,
// and registry so a test can read engine_validate_* series after quiescence.
func meteredValidationEngine(t *testing.T, worker string, providers *llm.Registry, vreg *validate.Registry, extraExec []exec.Executor, opts ...engine.Option) (*store.Store, *queuetest.Harness, *prometheus.Registry) {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	execs := append([]exec.Executor{exec.NewLLMExecutor(providers)}, extraExec...)
	reg, err := exec.NewRegistry(execs...)
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	mreg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(mreg)
	d := startDispatcher(t, s, h.Queue())
	full := append([]engine.Option{
		engine.WithDispatchNudge(d.Nudge),
		engine.WithValidators(vreg),
		engine.WithMetrics(wm),
	}, opts...)
	eng, err := engine.New(s, reg, worker, full...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn(worker, eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return s, h, mreg
}

// TestQualityMetricsSemanticLoop: a fail→fail→pass semantic loop records two
// fail verdicts and one pass (by step type and resource), the matching
// per-validator results, and one semantic-depth sample of 3 under the
// succeeded outcome.
func TestQualityMetricsSemanticLoop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	gateCalls := &atomic.Int64{}
	mock, err := llm.NewMock(llm.MockConfig{
		Rules: []llm.MockRule{
			{Substring: "attempt 3 of 3", Respond: []llm.MockOutcome{{Text: "APPROVED final answer"}}},
		},
		Default: &llm.MockOutcome{Text: "draft, not yet approved"},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(containsGateV("gate", "APPROVED", gateCalls))
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	s, h, mreg := meteredValidationEngine(t, "quality-sem", providers, vreg, []exec.Executor{exec.NoopExecutor{}})

	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: semanticDef(t, 3, 64, 0, "gate"), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	const llmType, resource = "llm", "mock:sim-1"
	if got := counterValue(t, mreg, "engine_validate_verdicts_total",
		map[string]string{"step_type": llmType, "resource": resource, "status": "fail"}); got != 2 {
		t.Errorf("verdicts fail = %v, want 2", got)
	}
	if got := counterValue(t, mreg, "engine_validate_verdicts_total",
		map[string]string{"step_type": llmType, "resource": resource, "status": "pass"}); got != 1 {
		t.Errorf("verdicts pass = %v, want 1", got)
	}
	if got := counterValue(t, mreg, "engine_validate_validator_results_total",
		map[string]string{"validator": "gate", "status": "fail"}); got != 2 {
		t.Errorf("validator gate fail = %v, want 2", got)
	}
	if got := counterValue(t, mreg, "engine_validate_validator_results_total",
		map[string]string{"validator": "gate", "status": "pass"}); got != 1 {
		t.Errorf("validator gate pass = %v, want 1", got)
	}
	// One terminated loop under succeeded, depth 3.
	depth := histogramOf(t, mreg, "engine_validate_semantic_depth_attempts", map[string]string{"outcome": "succeeded"})
	if depth == nil {
		t.Fatal("no succeeded semantic-depth sample")
	}
	if depth.GetSampleCount() != 1 || depth.GetSampleSum() != 3 {
		t.Errorf("semantic depth succeeded = count %d sum %v, want 1/3", depth.GetSampleCount(), depth.GetSampleSum())
	}
}

// TestQualityMetricsExhaustionDepth: an always-failing chain under
// max_attempts=2 records two fail verdicts and one semantic-depth sample of 2
// under the validation_failed outcome (the terminal dead-letter).
func TestQualityMetricsExhaustionDepth(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	mock, err := llm.NewMock(llm.MockConfig{})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(alwaysFailV("strict2"))
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	s, h, mreg := meteredValidationEngine(t, "quality-exhaust", providers, vreg, []exec.Executor{exec.NoopExecutor{}})

	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: semanticDef(t, 2, 64, 0, "strict2"), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	if got := counterValue(t, mreg, "engine_validate_verdicts_total",
		map[string]string{"step_type": "llm", "resource": "mock:sim-1", "status": "fail"}); got != 2 {
		t.Errorf("verdicts fail = %v, want 2", got)
	}
	depth := histogramOf(t, mreg, "engine_validate_semantic_depth_attempts", map[string]string{"outcome": "validation_failed"})
	if depth == nil {
		t.Fatal("no validation_failed semantic-depth sample")
	}
	if depth.GetSampleCount() != 1 || depth.GetSampleSum() != 2 {
		t.Errorf("semantic depth validation_failed = count %d sum %v, want 1/2", depth.GetSampleCount(), depth.GetSampleSum())
	}
}

// TestQualityMetricsRepairRate: a fenced-and-trailing-comma completion under an
// output_format step is repaired, recording one engine_validate_repairs_total
// sample under the repaired status (the implicit json_schema validator then
// passes it).
func TestQualityMetricsRepairRate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	mock, err := llm.NewMock(llm.MockConfig{
		Rules: []llm.MockRule{
			{Substring: "repair-me", Respond: []llm.MockOutcome{{Text: "```json\n{\"title\": \"repaired\",}\n```"}}},
		},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validate.NewJSONSchema())
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	s, h, mreg := meteredValidationEngine(t, "quality-repair", providers, vreg, []exec.Executor{exec.EchoExecutor{}})

	def := mustDecode(t, `{
		"schema_version": 1,
		"name": "quality-repair",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "repair-me", "max_tokens": 64, "temperature": 0,
			   "output_format": {"type": "json_schema", "mode": "repair_only",
			     "schema": {"type": "object", "properties": {"title": {"type": "string"}}, "required": ["title"]}}}}
		],
		"edges": []
	}`)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	if got := counterValue(t, mreg, "engine_validate_repairs_total",
		map[string]string{"status": "repaired"}); got != 1 {
		t.Errorf("repairs repaired = %v, want 1", got)
	}
	// The implicit json_schema validator passed the repaired output.
	if got := counterValue(t, mreg, "engine_validate_verdicts_total",
		map[string]string{"step_type": "llm", "resource": "mock:sim-1", "status": "pass"}); got != 1 {
		t.Errorf("verdicts pass = %v, want 1", got)
	}
}

// TestQualityMetricsJudgeScore: the llm_judge scores a terse answer low then a
// revised answer high; both scores land in the judge_score_ratio histogram,
// and the per-validator results record one fail and one pass for llm_judge.
func TestQualityMetricsJudgeScore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	mock, err := llm.NewMock(llm.MockConfig{
		Rules: []llm.MockRule{
			{Substring: "output-quality judge", Respond: []llm.MockOutcome{
				{Text: `{"score": 0.2, "rationale": "too terse, add detail"}`},
				{Text: `{"score": 0.9, "rationale": "detailed and well-supported"}`},
			}},
			{Substring: "too terse", Respond: []llm.MockOutcome{{Text: "a detailed and well-supported answer"}}},
		},
		Default: &llm.MockOutcome{Text: "terse"},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validate.NewLLMJudge(providers))
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	s, h, mreg := meteredValidationEngine(t, "quality-judge", providers, vreg,
		[]exec.Executor{exec.NoopExecutor{}}, engine.WithPricing(testCatalogE2E(t)))

	def := mustDecode(t, `{
		"schema_version": 1,
		"name": "quality-judge",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "write an answer", "max_tokens": 64, "temperature": 0},
			 "validation": {"validators": [{"name": "llm_judge",
			   "config": {"model":"mock/judge-1","rubric":"The answer must be detailed.","threshold":0.7,"on_error":"fail"}}],
			   "max_attempts": 3}}
		],
		"edges": []
	}`)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// Two judge calls: one low score, one high. Both land in the histogram.
	score := histogramOf(t, mreg, "engine_validate_judge_score_ratio", map[string]string{"validator": "llm_judge"})
	if score == nil {
		t.Fatal("no judge score samples")
	}
	if score.GetSampleCount() != 2 {
		t.Fatalf("judge score samples = %d, want 2", score.GetSampleCount())
	}
	if sum := score.GetSampleSum(); sum < 1.05 || sum > 1.15 {
		t.Errorf("judge score sum = %v, want ~1.1 (0.2 + 0.9)", sum)
	}
	// Per-validator results: one fail (0.2 < 0.7), one pass (0.9 >= 0.7).
	if got := counterValue(t, mreg, "engine_validate_validator_results_total",
		map[string]string{"validator": "llm_judge", "status": "fail"}); got != 1 {
		t.Errorf("llm_judge fail results = %v, want 1", got)
	}
	if got := counterValue(t, mreg, "engine_validate_validator_results_total",
		map[string]string{"validator": "llm_judge", "status": "pass"}); got != 1 {
		t.Errorf("llm_judge pass results = %v, want 1", got)
	}
}
