package validate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// TestBuiltinValidatorManifests pins ADR-009's validator flag table (11.2):
// the five deterministic built-ins, each cacheable-only at 1.0.0 with a
// compilable, described config schema. It mirrors tools.TestBuiltinToolManifests.
func TestBuiltinValidatorManifests(t *testing.T) {
	t.Parallel()

	reg, err := validate.NewBuiltins()
	if err != nil {
		t.Fatalf("NewBuiltins: %v", err)
	}
	want := map[string]plugin.Capabilities{
		"json_schema":   {Cacheable: true},
		"regex":         {Cacheable: true},
		"contains":      {Cacheable: true},
		"cel":           {Cacheable: true},
		"numeric_range": {Cacheable: true},
	}
	manifests := reg.Manifests()
	if len(manifests) != len(want) {
		t.Fatalf("Manifests() = %d, want %d", len(manifests), len(want))
	}
	for _, m := range manifests {
		if m.Kind != plugin.KindValidator {
			t.Errorf("%s: kind %q, want validator", m.Name, m.Kind)
		}
		if m.Version != "1.0.0" {
			t.Errorf("%s: version %q, want 1.0.0", m.Name, m.Version)
		}
		if m.Description == "" {
			t.Errorf("%s: manifest has no description", m.Name)
		}
		w, ok := want[m.Name]
		if !ok {
			t.Errorf("%s: not in the ADR-009 validator flag table", m.Name)
			continue
		}
		if m.Capabilities != w {
			t.Errorf("%s: capabilities %+v, want %+v", m.Name, m.Capabilities, w)
		}
		if len(m.ConfigSchema) == 0 {
			t.Errorf("%s: no config schema", m.Name)
		}
	}
}

// TestKitchenSinkConfigsAccepted proves the corpus's authored chain configs
// pass ValidateConfig against the real built-ins.
func TestKitchenSinkConfigsAccepted(t *testing.T) {
	t.Parallel()
	reg := builtins(t)
	cases := []struct {
		name   string
		config string
	}{
		{"contains", `{"substring":"Introduction"}`},
		{"cel", `{"expr":"size(value) > 200"}`},
		{"json_schema", `{"schema":{"type":"object"}}`},
		{"regex", `{"pattern":"^Summary"}`},
		{"numeric_range", `{"min":0,"max":1}`},
	}
	for _, c := range cases {
		if err := reg.ValidateConfig(c.name, []byte(c.config)); err != nil {
			t.Errorf("%s %s: ValidateConfig = %v", c.name, c.config, err)
		}
	}
}

// verdictCase is one row of a validator's good/bad output corpus.
type verdictCase struct {
	name      string
	config    string
	value     string // the target value (already selected), as JSON
	stepType  dag.StepType
	wantFail  bool
	wantCodes []string // issue codes expected on a fail (order-independent)
}

func runVerdictCases(t *testing.T, v validate.Validator, cases []verdictCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validate.Input{
				StepType: c.stepType,
				Output:   json.RawMessage(c.value),
				Value:    json.RawMessage(c.value),
				Config:   json.RawMessage(c.config),
				Attempt:  1,
			}
			verdict, err := v.Validate(context.Background(), in)
			if err != nil {
				t.Fatalf("Validate returned transport error (should be a fail verdict): %v", err)
			}
			if verdict.Passed() == c.wantFail {
				t.Fatalf("status = %s, wantFail=%v", verdict.Status, c.wantFail)
			}
			if !c.wantFail {
				if len(verdict.Issues) != 0 {
					t.Errorf("pass verdict carries issues: %+v", verdict.Issues)
				}
				return
			}
			got := map[string]bool{}
			for _, iss := range verdict.Issues {
				got[iss.Code] = true
				if iss.Validator == "" {
					t.Errorf("issue missing validator name: %+v", iss)
				}
			}
			for _, code := range c.wantCodes {
				if !got[code] {
					t.Errorf("missing issue code %q; got %+v", code, verdict.Issues)
				}
			}
		})
	}
}

func TestJSONSchemaValidator(t *testing.T) {
	t.Parallel()
	v := validate.NewJSONSchema()
	const objSchema = `{"schema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}}`
	runVerdictCases(t, v, []verdictCase{
		{name: "valid object", config: objSchema, value: `{"n":3}`, wantFail: false},
		{name: "missing required", config: objSchema, value: `{}`, wantFail: true, wantCodes: []string{"required"}},
		{name: "wrong type", config: objSchema, value: `{"n":"x"}`, wantFail: true, wantCodes: []string{"type_mismatch"}},
		{name: "extra property", config: objSchema, value: `{"n":3,"z":1}`, wantFail: true, wantCodes: []string{"additional_properties"}},
		// llm /text default: the value is a JSON string whose contents are parsed.
		{name: "json-in-string valid", config: objSchema, value: `"{\"n\":7}"`, stepType: dag.StepLLM, wantFail: false},
		{name: "string not json", config: objSchema, value: `"not json at all"`, stepType: dag.StepLLM, wantFail: true, wantCodes: []string{"invalid_json"}},
	})
}

func TestRegexValidator(t *testing.T) {
	t.Parallel()
	v := validate.NewRegex()
	runVerdictCases(t, v, []verdictCase{
		{name: "match", config: `{"pattern":"^Sum"}`, value: `"Summary of results"`, wantFail: false},
		{name: "no match", config: `{"pattern":"^Sum"}`, value: `"Intro"`, wantFail: true, wantCodes: []string{"pattern_no_match"}},
		{name: "negate blocks match", config: `{"pattern":"SECRET","negate":true}`, value: `"has SECRET inside"`, wantFail: true, wantCodes: []string{"pattern_matched"}},
		{name: "negate passes when absent", config: `{"pattern":"SECRET","negate":true}`, value: `"clean text"`, wantFail: false},
		// non-string target: compact JSON is scanned.
		{name: "structured value", config: `{"pattern":"\"k\":42"}`, value: `{"k":42}`, wantFail: false},
	})
}

func TestContainsValidator(t *testing.T) {
	t.Parallel()
	v := validate.NewContains()
	runVerdictCases(t, v, []verdictCase{
		{name: "present", config: `{"substring":"Intro"}`, value: `"Introduction"`, wantFail: false},
		{name: "absent", config: `{"substring":"Intro"}`, value: `"Summary"`, wantFail: true, wantCodes: []string{"substring_missing"}},
		{name: "case-insensitive", config: `{"substring":"intro","case_insensitive":true}`, value: `"INTRODUCTION"`, wantFail: false},
		{name: "case-sensitive fails", config: `{"substring":"intro"}`, value: `"INTRODUCTION"`, wantFail: true, wantCodes: []string{"substring_missing"}},
		{name: "negate present", config: `{"substring":"TODO","negate":true}`, value: `"body TODO left"`, wantFail: true, wantCodes: []string{"substring_present"}},
	})
}

func TestCELValidator(t *testing.T) {
	t.Parallel()
	v := validate.NewCEL()
	runVerdictCases(t, v, []verdictCase{
		{name: "true predicate", config: `{"expr":"size(value) > 3"}`, value: `"hello world"`, wantFail: false},
		{name: "false predicate", config: `{"expr":"size(value) > 100"}`, value: `"short"`, wantFail: true, wantCodes: []string{"predicate_false"}},
		{name: "field access object", config: `{"expr":"value.score > 0.5"}`, value: `{"score":0.9}`, wantFail: false},
		{name: "field access fail", config: `{"expr":"value.score > 0.5"}`, value: `{"score":0.1}`, wantFail: true, wantCodes: []string{"predicate_false"}},
		{name: "missing field eval error", config: `{"expr":"value.score > 0.5"}`, value: `{"other":1}`, wantFail: true, wantCodes: []string{"cel_eval_error"}},
		{name: "parse_json valid", config: `{"expr":"value.n == 5","parse_json":true}`, value: `"{\"n\":5}"`, stepType: dag.StepLLM, wantFail: false},
		{name: "parse_json bad json", config: `{"expr":"value.n == 5","parse_json":true}`, value: `"nope"`, stepType: dag.StepLLM, wantFail: true, wantCodes: []string{"invalid_json"}},
		{name: "step_type binding", config: `{"expr":"step_type == 'llm'"}`, value: `"x"`, stepType: dag.StepLLM, wantFail: false},
	})
}

func TestNumericRangeValidator(t *testing.T) {
	t.Parallel()
	v := validate.NewNumericRange()
	runVerdictCases(t, v, []verdictCase{
		{name: "in range", config: `{"min":0,"max":1}`, value: `0.5`, wantFail: false},
		{name: "below min", config: `{"min":0,"max":1}`, value: `-0.1`, wantFail: true, wantCodes: []string{"below_min"}},
		{name: "above max", config: `{"min":0,"max":1}`, value: `1.5`, wantFail: true, wantCodes: []string{"above_max"}},
		{name: "numeric string", config: `{"min":0,"max":100}`, value: `"42"`, wantFail: false},
		{name: "padded numeric string", config: `{"min":0,"max":100}`, value: `" 42 "`, wantFail: false},
		{name: "not a number", config: `{"min":0,"max":100}`, value: `"abc"`, wantFail: true, wantCodes: []string{"not_a_number"}},
		{name: "object not a number", config: `{"min":0}`, value: `{"x":1}`, wantFail: true, wantCodes: []string{"not_a_number"}},
		{name: "exclusive max boundary", config: `{"exclusive_max":10}`, value: `10`, wantFail: true, wantCodes: []string{"above_max"}},
		{name: "exclusive min boundary", config: `{"exclusive_min":0}`, value: `0`, wantFail: true, wantCodes: []string{"below_min"}},
	})
}

// TestMalformedJSONNeverPanics feeds every validator a target that is not
// valid JSON at all — the chain never marshals such a value (resolveTarget
// re-marshals), but the built-ins must still degrade to a structured verdict
// or transport error, never panic (the ticket's explicit criterion).
func TestMalformedJSONNeverPanics(t *testing.T) {
	t.Parallel()
	reg := builtins(t)
	bad := json.RawMessage(`{not json`)
	for _, name := range reg.Names() {
		v, err := reg.Get(name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		config := map[string]string{
			"json_schema":   `{"schema":{"type":"object"}}`,
			"regex":         `{"pattern":"x"}`,
			"contains":      `{"substring":"x"}`,
			"cel":           `{"expr":"true"}`,
			"numeric_range": `{"min":0}`,
		}[name]
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked on malformed JSON: %v", name, r)
				}
			}()
			_, _ = v.Validate(context.Background(), validate.Input{
				Output: bad, Value: bad, Config: json.RawMessage(config), Attempt: 1,
			})
		}()
	}
}

// TestPreflightRejectsBadConfig proves the deeper pre-flight gate
// (CompileConfig) turns an uncompilable regex/CEL/JSON-schema and a
// bound-less/inverted numeric_range into a permanent *ConfigValidationError
// through the public Resolve path — before any spend.
func TestPreflightRejectsBadConfig(t *testing.T) {
	t.Parallel()
	reg := builtins(t)
	cases := []struct {
		name   string
		config string
	}{
		{"regex", `{"pattern":"("}`},
		{"cel", `{"expr":"value +"}`},     // syntax error
		{"cel", `{"expr":"size(value)"}`}, // not a boolean predicate
		{"json_schema", `{"schema":{"type":"nonsense"}}`},
		{"numeric_range", `{}`},                                    // no bound
		{"numeric_range", `{"min":10,"max":1}`},                    // inverted
		{"numeric_range", `{"exclusive_min":5,"exclusive_max":5}`}, // unsatisfiable
	}
	for _, c := range cases {
		spec := dag.ValidatorSpec{Name: c.name, Config: json.RawMessage(c.config)}
		policy := &dag.ValidationPolicy{Validators: []dag.ValidatorSpec{spec}}
		_, err := validate.Resolve(reg, policy, dag.StepLLM)
		if err == nil {
			t.Errorf("%s %s: Resolve accepted a bad config", c.name, c.config)
			continue
		}
		var cve *validate.ConfigValidationError
		if !errors.As(err, &cve) {
			t.Errorf("%s %s: error %T, want *ConfigValidationError", c.name, c.config, err)
		}
	}
}

// TestPreflightWarmsCacheNoRecompile proves compiled artifacts are reused
// across attempts: after the pre-flight gate, running the same chain many
// times performs no further compilation (the ticket's "compiled artifacts
// reused across attempts — benchmarked" criterion, asserted structurally).
func TestPreflightWarmsCacheNoRecompile(t *testing.T) {
	t.Parallel()
	// Use a fresh validator instance so its compile cache starts cold.
	reg, err := validate.NewRegistry(validate.NewRegex())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	policy := &dag.ValidationPolicy{Validators: []dag.ValidatorSpec{
		{Name: "regex", Config: json.RawMessage(`{"pattern":"^ok"}`)},
	}}
	chain, err := validate.Resolve(reg, policy, dag.StepLLM)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for i := 0; i < 200; i++ {
		if _, err := chain.Run(context.Background(), json.RawMessage(`{"text":"ok go"}`), 1, nil); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	// The regex validator compiled exactly once (at Resolve/pre-flight); 200
	// Runs reused it. The counter is checked in an in-package test
	// (compilecache_internal_test.go); here we assert only that the many runs
	// succeeded, which they cannot do without a live compiled artifact.
}

// builtins builds the built-in validator registry or fails the test.
func builtins(t *testing.T) *validate.Registry {
	t.Helper()
	reg, err := validate.NewBuiltins()
	if err != nil {
		t.Fatalf("NewBuiltins: %v", err)
	}
	return reg
}

// TestSchemaIssuePathIsPointer confirms json_schema reports the offending
// location as an RFC 6901 pointer relative to the target.
func TestSchemaIssuePathIsPointer(t *testing.T) {
	t.Parallel()
	v := validate.NewJSONSchema()
	config := `{"schema":{"type":"object","properties":{"items":{"type":"array","items":{"type":"integer"}}}}}`
	in := validate.Input{
		Output:  json.RawMessage(`{"items":[1,"two",3]}`),
		Value:   json.RawMessage(`{"items":[1,"two",3]}`),
		Config:  json.RawMessage(config),
		Attempt: 1,
	}
	verdict, err := v.Validate(context.Background(), in)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if verdict.Passed() {
		t.Fatal("expected fail verdict for a string in an integer array")
	}
	found := false
	for _, iss := range verdict.Issues {
		if strings.HasPrefix(iss.Path, "/items/1") {
			found = true
		}
	}
	if !found {
		t.Errorf("no issue pointing at /items/1; got %+v", verdict.Issues)
	}
}

// Benchmarks establish that a warm validator (compiled artifact reused) is
// materially cheaper than a cold one recompiling per call — the "no
// per-attempt recompilation — benchmarked" criterion.

func BenchmarkJSONSchemaWarm(b *testing.B) {
	v := validate.NewJSONSchema()
	config := json.RawMessage(`{"schema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}`)
	value := json.RawMessage(`{"n":5}`)
	// Warm the cache once.
	_, _ = v.Validate(context.Background(), validate.Input{Value: value, Config: config, Attempt: 1})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Validate(context.Background(), validate.Input{Value: value, Config: config, Attempt: 1})
	}
}

func BenchmarkJSONSchemaCold(b *testing.B) {
	config := json.RawMessage(`{"schema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}`)
	value := json.RawMessage(`{"n":5}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := validate.NewJSONSchema() // fresh cache: recompiles every iteration
		_, _ = v.Validate(context.Background(), validate.Input{Value: value, Config: config, Attempt: 1})
	}
}

func BenchmarkCELWarm(b *testing.B) {
	v := validate.NewCEL()
	config := json.RawMessage(`{"expr":"size(value) > 3"}`)
	value := json.RawMessage(`"hello world"`)
	_, _ = v.Validate(context.Background(), validate.Input{Value: value, Config: config, Attempt: 1})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Validate(context.Background(), validate.Input{Value: value, Config: config, Attempt: 1})
	}
}

func BenchmarkRegexWarm(b *testing.B) {
	v := validate.NewRegex()
	config := json.RawMessage(`{"pattern":"^h.*d$"}`)
	value := json.RawMessage(`"hello world"`)
	_, _ = v.Validate(context.Background(), validate.Input{Value: value, Config: config, Attempt: 1})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Validate(context.Background(), validate.Input{Value: value, Config: config, Attempt: 1})
	}
}
