package validate_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// stubValidator is an in-test Validator whose verdict and capability flags
// are fully scripted — the workhorse for the registry and chain matrices
// (11.2 ships the real built-ins).
type stubValidator struct {
	name        string
	costBearing bool
	schema      json.RawMessage
	// fn produces the verdict/error for a call; nil means always-pass.
	fn func(in validate.Input) (validate.Verdict, error)
	// calls counts Validate invocations, for the cheap-first ordering test.
	calls *int
}

func (s *stubValidator) Manifest() plugin.Manifest {
	schema := s.schema
	if schema == nil {
		schema = validate.EmptyConfigSchema()
	}
	return plugin.Manifest{
		Kind: plugin.KindValidator, Name: s.name, Version: "1.0.0",
		Capabilities: plugin.Capabilities{Cacheable: true, CostBearing: s.costBearing},
		ConfigSchema: schema,
	}
}

func (s *stubValidator) Validate(_ context.Context, in validate.Input) (validate.Verdict, error) {
	if s.calls != nil {
		*s.calls++
	}
	if s.fn == nil {
		return validate.PassVerdict(), nil
	}
	return s.fn(in)
}

func pass(name string) *stubValidator { return &stubValidator{name: name} }
func failing(name string, code string) *stubValidator {
	return &stubValidator{name: name, fn: func(validate.Input) (validate.Verdict, error) {
		return validate.FailVerdict(validate.Issue{Code: code, Message: "nope"}), nil
	}}
}

func mustRegistry(t *testing.T, vs ...validate.Validator) *validate.Registry {
	t.Helper()
	r, err := validate.NewRegistry(vs...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func TestRegistryRegisterRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    validate.Validator
	}{
		{"nil validator", nil},
		{"wrong kind", &wrongKind{}},
		{"nil config schema", &nilSchema{}},
		{"uncompilable schema", &badSchema{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := validate.NewRegistry(tc.v); err == nil {
				t.Fatalf("expected registration error, got nil")
			}
		})
	}
}

func TestRegistryDuplicate(t *testing.T) {
	t.Parallel()
	if _, err := validate.NewRegistry(pass("dup"), pass("dup")); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}

func TestRegistryGetUnknown(t *testing.T) {
	t.Parallel()
	r := mustRegistry(t, pass("known"))
	_, err := r.Get("missing")
	var unk *validate.UnknownValidatorError
	if !errors.As(err, &unk) || unk.Name != "missing" {
		t.Fatalf("Get missing: want *UnknownValidatorError{missing}, got %v", err)
	}
	if !errors.Is(err, validate.ErrUnknownValidator) {
		t.Fatalf("Get missing: want errors.Is ErrUnknownValidator")
	}
}

func TestValidateConfigGate(t *testing.T) {
	t.Parallel()
	// A validator requiring {"threshold": number}.
	schema := json.RawMessage(`{"type":"object","properties":{"threshold":{"type":"number"}},"required":["threshold"],"additionalProperties":false}`)
	r := mustRegistry(t, &stubValidator{name: "scored", schema: schema})

	if err := r.ValidateConfig("scored", json.RawMessage(`{"threshold":0.5}`)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// Missing required field.
	err := r.ValidateConfig("scored", json.RawMessage(`{}`))
	var cve *validate.ConfigValidationError
	if !errors.As(err, &cve) {
		t.Fatalf("missing field: want *ConfigValidationError, got %v", err)
	}
	if !errors.Is(err, validate.ErrInvalidConfig) {
		t.Fatalf("missing field: want errors.Is ErrInvalidConfig")
	}
	// Unknown validator.
	if err := r.ValidateConfig("nope", json.RawMessage(`{}`)); !errors.Is(err, validate.ErrUnknownValidator) {
		t.Fatalf("unknown validator config: want ErrUnknownValidator, got %v", err)
	}
	// Absent config validates as {} → still rejected by a schema requiring
	// a field (the empty object lacks "threshold").
	if err := r.ValidateConfig("scored", nil); err == nil {
		t.Fatal("nil config should fail a schema requiring a field")
	}
}

func TestResolveUnknownValidatorNoRegistry(t *testing.T) {
	t.Parallel()
	policy := &dag.ValidationPolicy{Validators: []dag.ValidatorSpec{{Name: "x"}}}
	// nil registry: a step authored a chain but nothing is registered.
	_, err := validate.Resolve(nil, policy, dag.StepLLM)
	if !errors.Is(err, validate.ErrUnknownValidator) {
		t.Fatalf("nil registry: want ErrUnknownValidator, got %v", err)
	}
	// nil policy → nil chain, no error.
	c, err := validate.Resolve(mustRegistry(t), nil, dag.StepLLM)
	if err != nil || !c.Empty() {
		t.Fatalf("nil policy: want (nil,nil), got (%v,%v)", c, err)
	}
}

func TestChainAllPass(t *testing.T) {
	t.Parallel()
	r := mustRegistry(t, pass("a"), pass("b"))
	c := resolve(t, r, dag.StepNoop, "a", "b")
	v, err := c.Run(context.Background(), json.RawMessage(`{"x":1}`), 1, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !v.Passed() {
		t.Fatalf("want pass, got %s", v.Status)
	}
	if len(v.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(v.Results))
	}
}

func TestChainCollectsAllCheapIssues(t *testing.T) {
	t.Parallel()
	// Two failing cheap validators: BOTH run (full critique), and both
	// sets of issues appear in chain order.
	r := mustRegistry(t, failing("a", "code_a"), failing("b", "code_b"))
	c := resolve(t, r, dag.StepNoop, "a", "b")
	v, err := c.Run(context.Background(), json.RawMessage(`{}`), 1, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.Passed() {
		t.Fatal("want fail")
	}
	if len(v.Issues) != 2 || v.Issues[0].Code != "code_a" || v.Issues[1].Code != "code_b" {
		t.Fatalf("want issues [code_a, code_b], got %+v", v.Issues)
	}
	// Issues are attributed to their validator.
	if v.Issues[0].Validator != "a" || v.Issues[1].Validator != "b" {
		t.Fatalf("issues not attributed: %+v", v.Issues)
	}
}

func TestChainCostBearingSkippedWhenCheapFails(t *testing.T) {
	t.Parallel()
	judgeCalls := 0
	judge := &stubValidator{name: "judge", costBearing: true, calls: &judgeCalls}
	r := mustRegistry(t, failing("cheap", "bad"), judge)
	c := resolve(t, r, dag.StepNoop, "cheap", "judge")
	v, err := c.Run(context.Background(), json.RawMessage(`{}`), 1, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.Passed() {
		t.Fatal("want fail")
	}
	if judgeCalls != 0 {
		t.Fatalf("cost-bearing judge should not run when a cheap validator failed; ran %d", judgeCalls)
	}
	// The judge's result is recorded skipped.
	var judgeResult *validate.ValidatorResult
	for i := range v.Results {
		if v.Results[i].Name == "judge" {
			judgeResult = &v.Results[i]
		}
	}
	if judgeResult == nil || judgeResult.Status != validate.StatusSkipped {
		t.Fatalf("want judge skipped, got %+v", judgeResult)
	}
}

func TestChainCostBearingRunsWhenCheapPasses(t *testing.T) {
	t.Parallel()
	judgeCalls := 0
	judge := &stubValidator{name: "judge", costBearing: true, calls: &judgeCalls}
	r := mustRegistry(t, pass("cheap"), judge)
	c := resolve(t, r, dag.StepNoop, "cheap", "judge")
	if _, err := c.Run(context.Background(), json.RawMessage(`{}`), 1, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if judgeCalls != 1 {
		t.Fatalf("judge should run once when cheap passed; ran %d", judgeCalls)
	}
}

func TestChainValidatorErrorAborts(t *testing.T) {
	t.Parallel()
	boom := &stubValidator{name: "boom", fn: func(validate.Input) (validate.Verdict, error) {
		return validate.Verdict{}, validate.Permanentf("boom", nil, "kaput")
	}}
	r := mustRegistry(t, boom)
	c := resolve(t, r, dag.StepNoop, "boom")
	_, err := c.Run(context.Background(), json.RawMessage(`{}`), 1, nil)
	var ve *validate.Error
	if !errors.As(err, &ve) || ve.Class != dag.ClassPermanent {
		t.Fatalf("want permanent *validate.Error, got %v", err)
	}
}

func TestChainScoreIsMinimum(t *testing.T) {
	t.Parallel()
	score := func(name string, s float64) *stubValidator {
		return &stubValidator{name: name, fn: func(validate.Input) (validate.Verdict, error) {
			sc := s
			return validate.Verdict{SchemaVersion: validate.VerdictSchemaVersion, Status: validate.StatusPass, Score: &sc}, nil
		}}
	}
	r := mustRegistry(t, score("hi", 0.9), score("lo", 0.3))
	c := resolve(t, r, dag.StepNoop, "hi", "lo")
	v, err := c.Run(context.Background(), json.RawMessage(`{}`), 1, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.Score == nil || *v.Score != 0.3 {
		t.Fatalf("want chain score 0.3, got %v", v.Score)
	}
}

func TestChainTargetSelection(t *testing.T) {
	t.Parallel()
	// A validator that asserts it saw exactly the /text value.
	var seen string
	spy := &stubValidator{name: "spy", fn: func(in validate.Input) (validate.Verdict, error) {
		seen = string(in.Value)
		return validate.PassVerdict(), nil
	}}
	r := mustRegistry(t, spy)
	// llm-family default target is /text.
	c := resolve(t, r, dag.StepLLM, "spy")
	out := json.RawMessage(`{"model":"m","text":"hello"}`)
	if _, err := c.Run(context.Background(), out, 1, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seen != `"hello"` {
		t.Fatalf("llm default target /text: want \"hello\", got %s", seen)
	}
}

func TestChainTargetNotFoundIsFailVerdict(t *testing.T) {
	t.Parallel()
	spyCalls := 0
	spy := &stubValidator{name: "spy", calls: &spyCalls}
	r := mustRegistry(t, spy)
	c := resolveWithTarget(t, r, dag.StepNoop, "spy", "/missing")
	v, err := c.Run(context.Background(), json.RawMessage(`{"present":1}`), 1, nil)
	if err != nil {
		t.Fatalf("Run should not error on a missing target: %v", err)
	}
	if v.Passed() {
		t.Fatal("missing target should fail the verdict")
	}
	if spyCalls != 0 {
		t.Fatal("validator should not be called when its target is missing")
	}
	if len(v.Issues) != 1 || v.Issues[0].Code != "target_not_found" {
		t.Fatalf("want one target_not_found issue, got %+v", v.Issues)
	}
}

func TestChainExplicitTargetOverridesDefault(t *testing.T) {
	t.Parallel()
	var seen string
	spy := &stubValidator{name: "spy", fn: func(in validate.Input) (validate.Verdict, error) {
		seen = string(in.Value)
		return validate.PassVerdict(), nil
	}}
	r := mustRegistry(t, spy)
	c := resolveWithTarget(t, r, dag.StepLLM, "spy", "/model")
	out := json.RawMessage(`{"model":"m","text":"hi"}`)
	if _, err := c.Run(context.Background(), out, 1, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seen != `"m"` {
		t.Fatalf("explicit target should override llm default; got %s", seen)
	}
}

func TestVerdictRoundTrip(t *testing.T) {
	t.Parallel()
	v := validate.FailVerdict(validate.Issue{Validator: "a", Code: "c", Path: "/x", Message: "m"})
	raw, err := v.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back validate.Verdict
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Status != validate.StatusFail || len(back.Issues) != 1 || back.Issues[0].Path != "/x" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	if back.SchemaVersion != validate.VerdictSchemaVersion {
		t.Fatalf("schema version not preserved: %d", back.SchemaVersion)
	}
}

// resolve builds a chain from named validators with no targets.
func resolve(t *testing.T, r *validate.Registry, st dag.StepType, names ...string) *validate.Chain {
	t.Helper()
	specs := make([]dag.ValidatorSpec, len(names))
	for i, n := range names {
		specs[i] = dag.ValidatorSpec{Name: n}
	}
	c, err := validate.Resolve(r, &dag.ValidationPolicy{Validators: specs}, st)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return c
}

// resolveWithTarget builds a single-entry chain with an explicit target.
func resolveWithTarget(t *testing.T, r *validate.Registry, st dag.StepType, name, target string) *validate.Chain {
	t.Helper()
	c, err := validate.Resolve(r, &dag.ValidationPolicy{Validators: []dag.ValidatorSpec{{Name: name, Target: target}}}, st)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return c
}

// wrongKind declares the wrong plugin kind.
type wrongKind struct{}

func (wrongKind) Manifest() plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindTool, Name: "wrong", Version: "1.0.0", ConfigSchema: validate.EmptyConfigSchema()}
}

func (wrongKind) Validate(context.Context, validate.Input) (validate.Verdict, error) {
	return validate.PassVerdict(), nil
}

// nilSchema declares no config schema.
type nilSchema struct{}

func (nilSchema) Manifest() plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindValidator, Name: "nilschema", Version: "1.0.0"}
}

func (nilSchema) Validate(context.Context, validate.Input) (validate.Verdict, error) {
	return validate.PassVerdict(), nil
}

// badSchema declares an uncompilable config schema.
type badSchema struct{}

func (badSchema) Manifest() plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindValidator, Name: "badschema", Version: "1.0.0", ConfigSchema: json.RawMessage(`{"type":123}`)}
}

func (badSchema) Validate(context.Context, validate.Input) (validate.Verdict, error) {
	return validate.PassVerdict(), nil
}
