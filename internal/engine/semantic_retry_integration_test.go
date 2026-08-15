//go:build integration

package engine_test

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 11.4's headline e2e (ADR-013): the semantic-retry engine. On a
// failing verdict the step is re-attempted with a feedback-augmented prompt
// under a semantic budget disjoint from the transport one; exhaustion
// dead-letters with the full verdict history; a run parking on budget halts
// the loop. All offline (mock provider + in-test validators).

// containsGateV passes iff the output's targeted value contains want; a fail
// carries a structured issue (the critique material). Records call count.
func containsGateV(name, want string, calls *atomic.Int64) *scriptedValidator {
	return &scriptedValidator{name: name, calls: calls, fn: func(in validate.Input) (validate.Verdict, error) {
		if strings.Contains(string(in.Value), want) {
			return validate.PassVerdict(), nil
		}
		return validate.FailVerdict(validate.Issue{
			Code: "marker_missing", Message: "output does not contain the required marker",
		}), nil
	}}
}

// semanticFixture wires a store, harness, counting provider (over a scripted
// mock), llm executor, and validator registry — optionally with pricing — into
// one engine, returning it for control-plane ops (SetBudget/Unpark).
type semanticFixture struct {
	s    *store.Store
	h    *queuetest.Harness
	e    *engine.Engine
	prov *countingProvider
}

func newSemanticFixture(t *testing.T, mockCfg llm.MockConfig, validators []validate.Validator) *semanticFixture {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	mock, err := llm.NewMock(mockCfg)
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	prov := &countingProvider{inner: mock}
	providers, err := llm.NewRegistry(prov)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(providers), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validators...)
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "semantic-worker",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("semantic-worker", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return &semanticFixture{s: s, h: h, e: eng, prov: prov}
}

func (f *semanticFixture) submit(t *testing.T, def *dag.Definition) uuid.UUID {
	t.Helper()
	res, err := f.s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return res.Run.ID
}

// semanticDef builds a one-llm-step definition with a validation chain naming
// the given validators, a max_attempts semantic budget, a completion max_tokens
// (which the estimate prices for the budget projection), and an optional run
// budget.
func semanticDef(t *testing.T, maxAttempts, maxTokens int, budgetUSD float64, validatorNames ...string) *dag.Definition {
	t.Helper()
	specs := make([]dag.ValidatorSpec, len(validatorNames))
	for i, n := range validatorNames {
		specs[i] = dag.ValidatorSpec{Name: n}
	}
	vjson, err := json.Marshal(dag.ValidationPolicy{Validators: specs, MaxAttempts: maxAttempts})
	if err != nil {
		t.Fatalf("marshal validation: %v", err)
	}
	budget := ""
	if budgetUSD > 0 {
		budget = `"budget_usd": ` + jsonNum(budgetUSD) + `,`
	}
	body := `{
		"schema_version": 1,
		"name": "semantic-test",
		` + budget + `
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "produce output", "max_tokens": ` + jsonNum(float64(maxTokens)) + `, "temperature": 0},
			 "validation": ` + string(vjson) + `}
		],
		"edges": []
	}`
	return mustDecode(t, body)
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// TestSemanticRetryFailFailPass is the ticket's headline: a mock scripted
// fail→fail→pass succeeds on semantic attempt 3, each augmented prompt is
// stored on its attempt (diffable), and two semantic-retry events fire. The
// mock approves only when the injected feedback says "attempt 3 of 3".
func TestSemanticRetryFailFailPass(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	gateCalls := &atomic.Int64{}
	f := newSemanticFixture(t, llm.MockConfig{
		Rules: []llm.MockRule{
			// The final attempt's prompt carries the injected "attempt 3 of 3"
			// feedback; approve it. Earlier attempts fall to the default.
			{Substring: "attempt 3 of 3", Respond: []llm.MockOutcome{{Text: "APPROVED final answer"}}},
		},
		Default: &llm.MockOutcome{Text: "draft, not yet approved"},
	}, []validate.Validator{containsGateV("gate", "APPROVED", gateCalls)})

	runID := f.submit(t, semanticDef(t, 3, 64, 0, "gate"))
	waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("attempts = %d, want 3 (fail, fail, pass)", len(atts))
	}
	wantOutcomes := []string{
		store.AttemptOutcomeValidationFailed,
		store.AttemptOutcomeValidationFailed,
		store.StepStatusSucceeded,
	}
	for i, want := range wantOutcomes {
		if atts[i].Outcome == nil || *atts[i].Outcome != want {
			t.Fatalf("attempt %d outcome = %v, want %q", i+1, atts[i].Outcome, want)
		}
		if len(atts[i].Verdict) == 0 {
			t.Errorf("attempt %d has no verdict", i+1)
		}
	}
	// The first attempt carried no feedback; attempts 2 and 3 each carry a
	// distinct (diffable) critique with the right semantic-attempt number.
	if len(atts[0].Feedback) != 0 {
		t.Errorf("attempt 1 should carry no feedback, got %s", atts[0].Feedback)
	}
	fb2 := decodeFeedback(t, atts[1].Feedback)
	fb3 := decodeFeedback(t, atts[2].Feedback)
	if fb2.SemanticAttempt != 2 || fb3.SemanticAttempt != 3 {
		t.Errorf("semantic attempts = %d,%d, want 2,3", fb2.SemanticAttempt, fb3.SemanticAttempt)
	}
	if !strings.Contains(fb2.Text, "attempt 2 of 3") {
		t.Errorf("attempt 2 feedback missing 'attempt 2 of 3': %q", fb2.Text)
	}
	if !strings.Contains(fb3.Text, "attempt 3 of 3") {
		t.Errorf("attempt 3 feedback missing 'attempt 3 of 3': %q", fb3.Text)
	}
	if fb2.Text == fb3.Text {
		t.Error("attempt 2 and 3 feedback are identical — augmented prompts must be diffable")
	}
	// The critique references the prior rejected output.
	if !strings.Contains(fb2.Text, "draft, not yet approved") {
		t.Errorf("attempt 2 feedback should quote the prior output: %q", fb2.Text)
	}
	if got := f.prov.calls.Load(); got != 3 {
		t.Errorf("provider calls = %d, want 3", got)
	}
	if got := gateCalls.Load(); got != 3 {
		t.Errorf("validator calls = %d, want 3", got)
	}
	// Two semantic-retry events, and the final output is the approved one.
	if got := countEvents(t, f.s, runID, store.EventStepSemanticRetryScheduled); got != 2 {
		t.Errorf("step_semantic_retry_scheduled events = %d, want 2", got)
	}
	step, err := f.s.Steps().Get(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if len(step.Feedback) != 0 {
		t.Errorf("succeeded step should have cleared its pending feedback, got %s", step.Feedback)
	}
	if !strings.Contains(string(step.Output), "APPROVED") {
		t.Errorf("final output = %s, want the approved answer", step.Output)
	}
}

// TestSemanticRetryExhaustionDeadLetters: an always-failing chain under
// max_attempts=2 makes two attempts then dead-letters, the DLQ record
// carrying the full verdict history; the step's pending feedback is cleared;
// a requeue restarts the semantic budget from one.
func TestSemanticRetryExhaustionDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	f := newSemanticFixture(t, llm.MockConfig{}, []validate.Validator{alwaysFailV("strict")})
	runID := f.submit(t, semanticDef(t, 2, 64, 0, "strict"))
	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	requireStepStatuses(t, f.s, runID, map[string]string{"gen": store.StepStatusDeadLettered})
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("attempts = %d, want 2 (semantic budget of 2)", len(atts))
	}
	for i, a := range atts {
		if a.Outcome == nil || *a.Outcome != store.AttemptOutcomeValidationFailed {
			t.Fatalf("attempt %d outcome = %v, want validation_failed", i+1, a.Outcome)
		}
	}
	if got := f.prov.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2", got)
	}
	// The DLQ record carries the full verdict history (two entries).
	dls, err := f.s.DeadLetters().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 {
		t.Fatalf("dead_letters rows = %d, want 1", len(dls))
	}
	var payload struct {
		SemanticAttempts int `json:"semantic_attempts"`
		VerdictHistory   []struct {
			AttemptNo       int32           `json:"attempt_no"`
			SemanticAttempt int             `json:"semantic_attempt"`
			Verdict         json.RawMessage `json:"verdict"`
		} `json:"verdict_history"`
	}
	if err := json.Unmarshal(dls[0].Error, &payload); err != nil {
		t.Fatalf("decoding DLQ error payload: %v", err)
	}
	if payload.SemanticAttempts != 2 {
		t.Errorf("DLQ semantic_attempts = %d, want 2", payload.SemanticAttempts)
	}
	if len(payload.VerdictHistory) != 2 {
		t.Fatalf("DLQ verdict_history len = %d, want 2", len(payload.VerdictHistory))
	}
	for i, h := range payload.VerdictHistory {
		if h.SemanticAttempt != i+1 || len(h.Verdict) == 0 {
			t.Errorf("verdict_history[%d] = %+v, want semantic attempt %d with a verdict", i, h, i+1)
		}
	}
	// The dead-lettered step cleared its pending feedback.
	step, err := f.s.Steps().Get(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if len(step.Feedback) != 0 {
		t.Errorf("dead-lettered step should have cleared feedback, got %s", step.Feedback)
	}

	// Requeue: the semantic budget re-arms from one (CountValidationFailures
	// counts past the DLQ baseline), so the step makes another two attempts and
	// dead-letters again — proving the requeue baseline works for the semantic
	// budget exactly as it does for the transport one.
	if _, err := f.e.Requeue(ctx, runID, "gen"); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)
	atts, err = f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts after requeue: %v", err)
	}
	if len(atts) != 4 {
		t.Fatalf("attempts after requeue = %d, want 4 (2 + 2 re-armed)", len(atts))
	}
	// The first attempt of the requeued run carried no feedback (a fresh
	// semantic budget), the second did.
	if len(atts[2].Feedback) != 0 {
		t.Errorf("requeued first attempt should carry no feedback, got %s", atts[2].Feedback)
	}
	if len(atts[3].Feedback) == 0 {
		t.Error("requeued second attempt should carry feedback")
	}
}

// TestSemanticRetryHaltsOnBudgetPark: a budgeted run whose first attempt fails
// validation halts the semantic loop when the next attempt's claim projects
// over budget — the run parks (budget_exceeded) rather than spending another
// provider call. Raising the budget and unparking resumes the loop, and the
// re-attempt (which the validator passes on semantic attempt 2) completes it.
func TestSemanticRetryHaltsOnBudgetPark(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// The validator passes only on semantic attempt >= 2 — so attempt 1 fails
	// (scheduling a semantic retry) and attempt 2, once it runs, passes.
	gate := &scriptedValidator{name: "attempt_gate", fn: func(in validate.Input) (validate.Verdict, error) {
		if in.Attempt >= 2 {
			return validate.PassVerdict(), nil
		}
		return validate.FailVerdict(validate.Issue{Code: "too_early", Message: "needs another pass"}), nil
	}}

	// Cost-bearing provider priced by the embedded catalog (mock:* wildcard);
	// each attempt costs 2_000_000 nano. Budget $0.003 = 3_000_000: attempt 1
	// (proj ~2M) proceeds and fails validation (spending 2M); attempt 2's claim
	// (spent 2M, proj ~4M > 3M) parks.
	prov := &fixedUsageProvider{in: 0, out: 1000}
	f := newSemanticFixturePriced(t, prov, gate)
	runID := f.submit(t, semanticDef(t, 3, 1000, 0.003, "attempt_gate"))

	// The run parks: the semantic loop is halted mid-flight. The budget-park
	// releases the second attempt's claim back to ready (Unpark re-dispatches
	// it), closing that attempt budget_exceeded; the pending feedback stays on
	// the step so the resumed attempt still carries the critique.
	waitRunStatus(t, f.s, runID, store.RunStatusParked)
	waitStep(t, f.s, runID, "gen", "ready under park with feedback intact", func(s gen.RunStep) bool {
		return s.Status == store.StepStatusReady && len(s.Feedback) > 0
	})
	f.h.WaitQuiescent(ctx)
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("parked attempts = %d, want 2 (validation_failed, then budget_exceeded)", len(atts))
	}
	if atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomeValidationFailed {
		t.Errorf("attempt 1 outcome = %v, want validation_failed", atts[0].Outcome)
	}
	if atts[1].Outcome == nil || *atts[1].Outcome != store.AttemptOutcomeBudgetExceeded {
		t.Errorf("attempt 2 outcome = %v, want budget_exceeded (the loop halted before the call)", atts[1].Outcome)
	}
	if got := prov.calls.Load(); got != 1 {
		t.Fatalf("provider calls while parked = %d, want 1 (the retry never reached the provider)", got)
	}

	// Raise the budget and unpark: the halted semantic retry resumes and, now on
	// semantic attempt 2, passes — the run completes.
	if _, err := f.e.SetBudget(ctx, runID, 1_000_000_000); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if _, err := f.e.Unpark(ctx, runID); err != nil {
		t.Fatalf("Unpark: %v", err)
	}
	waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)
	atts, err = f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts after unpark: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("attempts after unpark = %d, want 3 (validation_failed, budget_exceeded, succeeded)", len(atts))
	}
	if atts[2].Outcome == nil || *atts[2].Outcome != store.StepStatusSucceeded {
		t.Errorf("final attempt outcome = %v, want succeeded", atts[2].Outcome)
	}
	if got := prov.calls.Load(); got != 2 {
		t.Errorf("provider calls total = %d, want 2 (the parked attempt spent none)", got)
	}
}

// newSemanticFixturePriced wires a cost-bearing provider + validator + pricing
// (the budget-park interplay setup).
func newSemanticFixturePriced(t *testing.T, prov *fixedUsageProvider, validators ...validate.Validator) *semanticFixture {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(fixedUsageRegistry(t, prov)), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validators...)
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "semantic-priced-worker",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithValidators(vreg),
		engine.WithPricing(testCatalogE2E(t)))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("semantic-priced-worker", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return &semanticFixture{s: s, h: h, e: eng}
}

// decodeFeedback decodes an attempt's feedback JSON.
func decodeFeedback(t *testing.T, raw json.RawMessage) exec.Feedback {
	t.Helper()
	var fb exec.Feedback
	if err := json.Unmarshal(raw, &fb); err != nil {
		t.Fatalf("decoding feedback: %v", err)
	}
	return fb
}
