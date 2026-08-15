//go:build integration

package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 11.5's headline e2e (ADR-013/ADR-012): the llm_judge validator. One
// mock provider serves both the productive llm step and the judge (they route
// "mock/sim-1" and "mock/judge-1" to the same provider), so the whole flow is
// offline. The judge fails a low-quality first output, the semantic retry
// feeds the judge's rationale back, the revised output passes — and the
// judge's provider calls are ledgered as OVERHEAD on the serving step.

// newJudgeFixture wires one mock provider (scripted by mockCfg) shared by the
// llm executor and the llm_judge validator, plus pricing, into one engine.
func newJudgeFixture(t *testing.T, mockCfg llm.MockConfig, judgeConfigJSON, maxAttempts string) (*semanticFixture, *dag.Definition) {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	mock, err := llm.NewMock(mockCfg)
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(providers), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	// The real llm_judge over the same provider registry — so the judge's
	// "mock/judge-1" routes to the same offline mock.
	vreg, err := validate.NewRegistry(validate.NewLLMJudge(providers))
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "judge-worker",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithValidators(vreg),
		engine.WithPricing(testCatalogE2E(t)))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("judge-worker", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))

	validation := `{"validators":[{"name":"llm_judge","config":` + judgeConfigJSON + `}],"max_attempts":` + maxAttempts + `}`
	body := `{
		"schema_version": 1,
		"name": "judge-test",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "write an answer", "max_tokens": 64, "temperature": 0},
			 "validation": ` + validation + `}
		],
		"edges": []
	}`
	return &semanticFixture{s: s, h: h, e: eng}, mustDecode(t, body)
}

// TestJudgeSemanticRetryE2E: a judge fails the terse first output, the
// semantic retry carries the rationale back, the revised output passes, and
// the judge's two calls are ledgered as overhead on the serving step.
func TestJudgeSemanticRetryE2E(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	mockCfg := llm.MockConfig{
		Rules: []llm.MockRule{
			// The judge (identified by its system prompt) scores the first
			// candidate low, the second high — a sticky two-outcome sequence.
			{Substring: "output-quality judge", Respond: []llm.MockOutcome{
				{Text: `{"score": 0.2, "rationale": "too terse, add detail and a citation"}`},
				{Text: `{"score": 0.9, "rationale": "detailed and well-supported"}`},
			}},
			// The productive step's second attempt carries the feedback (the
			// rationale "too terse ..."), so it produces the polished answer.
			{Substring: "too terse", Respond: []llm.MockOutcome{{Text: "a detailed and well-supported answer with a citation"}}},
		},
		// The productive step's first attempt (no feedback) is terse.
		Default: &llm.MockOutcome{Text: "terse"},
	}
	judgeCfg := `{"model":"mock/judge-1","rubric":"The answer must be detailed and cite a source.","threshold":0.7,"on_error":"fail"}`
	f, def := newJudgeFixture(t, mockCfg, judgeCfg, "3")

	runID := f.submit(t, def)
	waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	// Two attempts: validation_failed (judge 0.2), then succeeded (judge 0.9).
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("attempts = %d, want 2 (validation_failed, succeeded)", len(atts))
	}
	if atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomeValidationFailed {
		t.Errorf("attempt 1 outcome = %v, want validation_failed", atts[0].Outcome)
	}
	if atts[1].Outcome == nil || *atts[1].Outcome != store.StepStatusSucceeded {
		t.Errorf("attempt 2 outcome = %v, want succeeded", atts[1].Outcome)
	}
	// The failing attempt's verdict carries the judge rationale (the critique).
	if atts[0].Verdict == nil {
		t.Fatal("attempt 1 has no verdict")
	}
	var v1 validate.Verdict
	if err := json.Unmarshal(atts[0].Verdict, &v1); err != nil {
		t.Fatalf("decoding verdict 1: %v", err)
	}
	if v1.Status != validate.StatusFail || v1.Score == nil || *v1.Score != 0.2 {
		t.Errorf("verdict 1 = %+v, want fail score 0.2", v1)
	}
	if len(v1.Results) != 1 || v1.Results[0].Rationale == "" {
		t.Errorf("verdict 1 result missing rationale: %+v", v1.Results)
	}
	// The second attempt was given the rationale as feedback.
	if atts[1].Feedback == nil || !strings.Contains(string(atts[1].Feedback), "too terse") {
		t.Errorf("attempt 2 feedback does not carry the judge rationale: %s", atts[1].Feedback)
	}
	// The succeeding attempt's verdict passes at 0.9.
	var v2 validate.Verdict
	if err := json.Unmarshal(atts[1].Verdict, &v2); err != nil {
		t.Fatalf("decoding verdict 2: %v", err)
	}
	if v2.Status != validate.StatusPass || v2.Score == nil || *v2.Score != 0.9 {
		t.Errorf("verdict 2 = %+v, want pass score 0.9", v2)
	}

	// Judges are terminal: the run has exactly one step (gen), no judge step.
	steps, err := f.s.Steps().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("run has %d steps, want 1 (judges are not steps)", len(steps))
	}

	// The ledger: two productive rows + two judge OVERHEAD rows, and the run
	// aggregate equals the exact ledger sum.
	rows, err := f.s.Ledger().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	var overheadRows, productiveRows int
	for _, r := range rows {
		if r.Overhead {
			overheadRows++
			if r.Resource != "mock:judge-1" {
				t.Errorf("overhead row resource = %q, want mock:judge-1", r.Resource)
			}
			if !strings.HasPrefix(r.Entry, "judge:") {
				t.Errorf("overhead row entry = %q, want judge:*", r.Entry)
			}
		} else {
			productiveRows++
			if r.Resource != "mock:sim-1" {
				t.Errorf("productive row resource = %q, want mock:sim-1", r.Resource)
			}
		}
	}
	if overheadRows != 2 || productiveRows != 2 {
		t.Errorf("ledger rows: %d productive, %d overhead; want 2 and 2", productiveRows, overheadRows)
	}
	sum, err := f.s.Ledger().SumByRun(ctx, runID)
	if err != nil {
		t.Fatalf("SumByRun: %v", err)
	}
	run := waitRun(t, f.s, runID, store.RunStatusSucceeded)
	if run.SpentNanoUsd != sum.SpentNanoUsd {
		t.Errorf("run aggregate spent %d != ledger sum %d", run.SpentNanoUsd, sum.SpentNanoUsd)
	}

	// The per-step cost breakdown surfaces the judge overhead separately.
	byStep, err := f.s.Ledger().AggregateByStep(ctx, runID)
	if err != nil {
		t.Fatalf("AggregateByStep: %v", err)
	}
	if len(byStep) != 1 {
		t.Fatalf("by-step rows = %d, want 1", len(byStep))
	}
	if byStep[0].OverheadNanoUsd <= 0 {
		t.Errorf("step overhead = %d, want > 0 (the judge calls)", byStep[0].OverheadNanoUsd)
	}
	if byStep[0].OverheadNanoUsd >= byStep[0].SpentNanoUsd {
		t.Errorf("overhead %d should be a slice of total spend %d", byStep[0].OverheadNanoUsd, byStep[0].SpentNanoUsd)
	}
}

// TestJudgeOnErrorFailMetersProductive: a judge that errors under
// on_error:fail (a malformed answer) fails the step as a transport failure —
// and the productive provider call that DID bill is still metered (the
// completeFailure gap fix, ticket 11.5), even though the output was rejected.
func TestJudgeOnErrorFailMetersProductive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	mockCfg := llm.MockConfig{
		Rules: []llm.MockRule{
			// The judge always answers with un-parseable prose → a malformed
			// answer → a permanent judge error under on_error:fail.
			{Substring: "output-quality judge", Respond: []llm.MockOutcome{{Text: "honestly it's fine i guess"}}},
		},
		Default: &llm.MockOutcome{Text: "some productive output"},
	}
	// max_attempts 1: no semantic loop — a judge error dead-letters at once.
	judgeCfg := `{"model":"mock/judge-1","rubric":"be good","threshold":0.7,"on_error":"fail"}`
	f, def := newJudgeFixture(t, mockCfg, judgeCfg, "1")

	runID := f.submit(t, def)
	// on_failure defaults to fail_fast → the run fails.
	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	// The productive call billed — its usage and cost must be metered even
	// though the validation stage errored (the gap fix). And the judge's own
	// billed (malformed) call is metered as overhead.
	rows, err := f.s.Ledger().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	var haveProductive, haveJudgeOverhead bool
	for _, r := range rows {
		if !r.Overhead && r.Resource == "mock:sim-1" {
			haveProductive = true
		}
		if r.Overhead && r.Resource == "mock:judge-1" {
			haveJudgeOverhead = true
		}
	}
	if !haveProductive {
		t.Error("productive spend was not metered on the validation-error path (the gap fix)")
	}
	if !haveJudgeOverhead {
		t.Error("the judge's billed malformed-answer call was not metered as overhead")
	}
	// The failing attempt recorded its usage.
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) == 0 || atts[len(atts)-1].Usage == nil {
		t.Error("the failed attempt did not record its productive usage")
	}
}

// TestJudgeOnErrorSkipDegrades: a judge that errors under on_error:skip does
// not fail the step — the run succeeds, the verdict result records the error,
// and no overhead row is written (the judge's failing call reported no usage
// here, a provider error).
func TestJudgeOnErrorSkipDegrades(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	mockCfg := llm.MockConfig{
		Rules: []llm.MockRule{
			// The judge always returns a scripted 500 → a provider error (no
			// usage billed).
			{Substring: "output-quality judge", Respond: []llm.MockOutcome{{Status: 500, Message: "judge is down"}}},
		},
		Default: &llm.MockOutcome{Text: "productive output"},
	}
	judgeCfg := `{"model":"mock/judge-1","rubric":"be good","threshold":0.7,"on_error":"skip"}`
	f, def := newJudgeFixture(t, mockCfg, judgeCfg, "1")

	runID := f.submit(t, def)
	waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 || atts[0].Outcome == nil || *atts[0].Outcome != store.StepStatusSucceeded {
		t.Fatalf("attempts = %+v, want one succeeded (skip degrades to pass)", atts)
	}
	if atts[0].Verdict == nil {
		t.Fatal("no verdict on the succeeded attempt")
	}
	var v validate.Verdict
	if err := json.Unmarshal(atts[0].Verdict, &v); err != nil {
		t.Fatalf("decoding verdict: %v", err)
	}
	if v.Status != validate.StatusPass {
		t.Errorf("verdict status = %q, want pass (skip)", v.Status)
	}
	if len(v.Results) != 1 || v.Results[0].Status != validate.StatusError {
		t.Errorf("verdict result = %+v, want a single error result", v.Results)
	}
	// No overhead row: the provider error billed nothing.
	rows, err := f.s.Ledger().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	for _, r := range rows {
		if r.Overhead {
			t.Errorf("unexpected overhead row for a provider error that billed nothing: %+v", r)
		}
	}
}
