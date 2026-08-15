//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 11.1's headline e2e (ADR-013): the engine's output-validation
// stage. A step's validation chain runs over the executor's output; a
// passing verdict is persisted on the attempt and the step succeeds, a
// failing verdict records the validation_failed outcome and dead-letters
// (never caching the invalid output), an unknown validator fails permanent
// before any spend, and a validator's own error routes as a transport
// failure — all against the offline mock provider and in-test validators
// (11.2 ships the real built-ins).

// scriptedValidator is an in-test Validator whose verdict and capability
// flags are fully scripted — the workhorse for the engine validation tests.
type scriptedValidator struct {
	name        string
	costBearing bool
	fn          func(in validate.Input) (validate.Verdict, error)
	calls       *atomic.Int64
}

func (v *scriptedValidator) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind: plugin.KindValidator, Name: v.name, Version: "1.0.0",
		Capabilities: plugin.Capabilities{Cacheable: true, CostBearing: v.costBearing},
		ConfigSchema: validate.EmptyConfigSchema(),
	}
}

func (v *scriptedValidator) Validate(_ context.Context, in validate.Input) (validate.Verdict, error) {
	if v.calls != nil {
		v.calls.Add(1)
	}
	if v.fn == nil {
		return validate.PassVerdict(), nil
	}
	return v.fn(in)
}

func alwaysPassV(name string) *scriptedValidator { return &scriptedValidator{name: name} }

func alwaysFailV(name string) *scriptedValidator {
	return &scriptedValidator{name: name, fn: func(validate.Input) (validate.Verdict, error) {
		return validate.FailVerdict(validate.Issue{Code: "forced_fail", Message: "scripted failure"}), nil
	}}
}

// validationFixture wires a store, harness, counting mock provider, llm
// executor, and validator registry into one engine.
type validationFixture struct {
	s    *store.Store
	h    *queuetest.Harness
	prov *countingProvider
}

func newValidationFixture(t *testing.T, validators []validate.Validator, withCache bool) *validationFixture {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	mock, err := llm.NewMock(llm.MockConfig{})
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
	opts := []engine.Option{
		engine.WithDispatchNudge(d.Nudge),
		engine.WithValidators(vreg),
	}
	if withCache {
		opts = append(opts, engine.WithResponseCache(newCacheStore(t, h.Client()), time.Hour))
	}
	eng, err := engine.New(s, reg, "validate-worker", opts...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("validate-worker", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return &validationFixture{s: s, h: h, prov: prov}
}

// validationDef builds a one-llm-step definition whose validation chain
// names the given validators (no targets → llm-family default /text).
func validationDef(t *testing.T, validatorNames ...string) *dag.Definition {
	t.Helper()
	specs := make([]dag.ValidatorSpec, len(validatorNames))
	for i, n := range validatorNames {
		specs[i] = dag.ValidatorSpec{Name: n}
	}
	vjson, err := json.Marshal(dag.ValidationPolicy{Validators: specs})
	if err != nil {
		t.Fatalf("marshal validation: %v", err)
	}
	body := `{
		"schema_version": 1,
		"name": "validation-test",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "produce output", "max_tokens": 64, "temperature": 0},
			 "validation": ` + string(vjson) + `}
		],
		"edges": []
	}`
	return mustDecode(t, body)
}

func (f *validationFixture) submit(t *testing.T, def *dag.Definition) uuid.UUID {
	t.Helper()
	res, err := f.s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return res.Run.ID
}

// TestValidationVerdictPersistedOnPass: a passing chain persists a verdict
// on the succeeded attempt (the 11.1 acceptance: verdict visible in the run
// status). The single attempt records outcome succeeded and a pass verdict.
func TestValidationVerdictPersistedOnPass(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newValidationFixture(t, []validate.Validator{alwaysPassV("ok")}, false)
	runID := f.submit(t, validationDef(t, "ok"))

	waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(atts))
	}
	if atts[0].Outcome == nil || *atts[0].Outcome != store.StepStatusSucceeded {
		t.Fatalf("outcome = %v, want succeeded", atts[0].Outcome)
	}
	if len(atts[0].Verdict) == 0 {
		t.Fatal("succeeded attempt has no verdict — the passing chain must persist one")
	}
	var v validate.Verdict
	if err := json.Unmarshal(atts[0].Verdict, &v); err != nil {
		t.Fatalf("unmarshaling verdict: %v", err)
	}
	if !v.Passed() || v.SchemaVersion != validate.VerdictSchemaVersion {
		t.Fatalf("verdict = %+v, want a pass at schema version %d", v, validate.VerdictSchemaVersion)
	}
	if len(v.Results) != 1 || v.Results[0].Name != "ok" || v.Results[0].Status != validate.StatusPass {
		t.Fatalf("verdict results = %+v, want one passing 'ok'", v.Results)
	}
}

// TestValidationFailureDeadLetters: a failing chain records the
// validation_failed outcome with the verdict and usage persisted, and
// dead-letters the step (source retries_exhausted, class validation_failed)
// → the fail_fast run fails. The provider was called exactly once (the
// output was produced, then rejected).
func TestValidationFailureDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newValidationFixture(t, []validate.Validator{alwaysFailV("strict")}, false)
	runID := f.submit(t, validationDef(t, "strict"))

	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	requireStepStatuses(t, f.s, runID, map[string]string{"gen": store.StepStatusDeadLettered})

	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("attempts = %d, want 1 (a validation failure is not a transport retry)", len(atts))
	}
	if atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomeValidationFailed {
		t.Fatalf("outcome = %v, want validation_failed", atts[0].Outcome)
	}
	// Usage is present — the provider call happened and billed even though
	// the output was rejected (ADR-012 rule 5 amended).
	if len(atts[0].Usage) == 0 {
		t.Fatal("validation_failed attempt has no usage — the provider call billed")
	}
	// The verdict is persisted as the evidence.
	if len(atts[0].Verdict) == 0 {
		t.Fatal("validation_failed attempt has no verdict")
	}
	var v validate.Verdict
	if err := json.Unmarshal(atts[0].Verdict, &v); err != nil {
		t.Fatalf("unmarshaling verdict: %v", err)
	}
	if v.Passed() || len(v.Issues) != 1 || v.Issues[0].Code != "forced_fail" {
		t.Fatalf("verdict = %+v, want a fail with the forced_fail issue", v)
	}
	// The DLQ row: source retries_exhausted (semantic budget of 1), class
	// validation_failed.
	dls, err := f.s.DeadLetters().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 {
		t.Fatalf("dead_letters rows = %d, want 1", len(dls))
	}
	if dls[0].Source != store.DeadLetterSourceRetriesExhausted {
		t.Errorf("DLQ source = %q, want retries_exhausted", dls[0].Source)
	}
	if dls[0].Class == nil || *dls[0].Class != store.AttemptOutcomeValidationFailed {
		t.Errorf("DLQ class = %v, want validation_failed", dls[0].Class)
	}
	if got := f.prov.calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1", got)
	}
}

// TestValidationInvalidNeverCached: with the response cache enabled, an
// output that fails validation is never written to the cache — so a second
// identical run misses and calls the provider again (ADR-011/ADR-013
// interplay). If the invalid output had been cached, the provider would be
// called only once.
func TestValidationInvalidNeverCached(t *testing.T) {
	t.Parallel()
	f := newValidationFixture(t, []validate.Validator{alwaysFailV("strict")}, true)

	run1 := f.submit(t, validationDef(t, "strict"))
	waitRun(t, f.s, run1, store.RunStatusFailed)
	run2 := f.submit(t, validationDef(t, "strict"))
	waitRun(t, f.s, run2, store.RunStatusFailed)
	f.h.WaitQuiescent(t.Context())

	if got := f.prov.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2 — an invalid output must never be cached", got)
	}
}

// TestUnknownValidatorPermanentBeforeSpend: a chain naming an unregistered
// validator fails permanent at claim, before the executor runs — the
// provider is never called, and the step dead-letters source permanent with
// the ordinary permanent class (not validation_failed — the output was
// never produced).
func TestUnknownValidatorPermanentBeforeSpend(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newValidationFixture(t, []validate.Validator{alwaysPassV("ok")}, false)
	// The definition names "missing", which is not registered.
	runID := f.submit(t, validationDef(t, "missing"))

	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	requireStepStatuses(t, f.s, runID, map[string]string{"gen": store.StepStatusDeadLettered})
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 || atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomePermanent {
		t.Fatalf("attempts = %+v, want one permanent (unresolvable chain, pre-flight)", atts)
	}
	if got := f.prov.calls.Load(); got != 0 {
		t.Errorf("provider calls = %d, want 0 — the chain gate fires before spend", got)
	}
	dls, err := f.s.DeadLetters().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 || dls[0].Source != store.DeadLetterSourcePermanent {
		t.Fatalf("DLQ = %+v, want one permanent", dls)
	}
}

// TestValidatorErrorRoutesAsTransportFailure: a validator that returns a
// *validate.Error (permanent) is a transport failure of the validation
// stage, NOT a fail verdict — it routes through the ADR-006 taxonomy, so the
// attempt records `permanent` (not validation_failed) and no verdict.
func TestValidatorErrorRoutesAsTransportFailure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	erroring := &scriptedValidator{name: "flaky", fn: func(validate.Input) (validate.Verdict, error) {
		return validate.Verdict{}, validate.Permanentf("flaky", nil, "validator dependency down")
	}}
	f := newValidationFixture(t, []validate.Validator{erroring}, false)
	runID := f.submit(t, validationDef(t, "flaky"))

	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 || atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomePermanent {
		t.Fatalf("attempts = %+v, want one permanent (validator error is a transport failure)", atts)
	}
	if len(atts[0].Verdict) != 0 {
		t.Errorf("validator-error attempt recorded a verdict %s, want none", atts[0].Verdict)
	}
}

// TestValidationCacheHitRevalidated: a passing validated step's second
// identical run is a cache hit — the provider is called once — but the
// stored output is re-validated on the hit (the validator runs on both
// runs), so a stored-but-now-invalid output would be re-executed, not
// served (ADR-013).
func TestValidationCacheHitRevalidated(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var vcalls atomic.Int64
	spy := &scriptedValidator{name: "spy", calls: &vcalls}
	f := newValidationFixture(t, []validate.Validator{spy}, true)

	run1 := f.submit(t, validationDef(t, "spy"))
	waitRun(t, f.s, run1, store.RunStatusSucceeded)
	run2 := f.submit(t, validationDef(t, "spy"))
	waitRun(t, f.s, run2, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	if got := f.prov.calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1 — the second run is a cache hit", got)
	}
	if got := vcalls.Load(); got != 2 {
		t.Errorf("validator calls = %d, want 2 — a cache hit is re-validated", got)
	}
}
