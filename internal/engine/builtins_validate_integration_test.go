//go:build integration

package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 11.2 headline: the real deterministic built-in validators, wired
// through the production engine validate stage, judge a real mock-provider
// output. The offline mock echoes the prompt as "[mock] <prompt>", so a
// contains chain over /text passes on the echoed substring and a json_schema
// chain fails (the echoed text is not JSON — the malformed-JSON path lands as
// a structured invalid_json issue, not a panic), each with the verdict
// persisted and visible in the run status.

// builtinValidationDef builds a one-llm-step definition carrying a real
// validation chain (concrete validator configs, not the config-less in-test
// stubs).
func builtinValidationDef(t *testing.T, specs ...dag.ValidatorSpec) *dag.Definition {
	t.Helper()
	vjson, err := json.Marshal(dag.ValidationPolicy{Validators: specs})
	if err != nil {
		t.Fatalf("marshal validation: %v", err)
	}
	body := `{
		"schema_version": 1,
		"name": "builtin-validation-test",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "produce output", "max_tokens": 64, "temperature": 0},
			 "validation": ` + string(vjson) + `}
		],
		"edges": []
	}`
	return mustDecode(t, body)
}

// TestBuiltinContainsPasses: a contains validator whose substring is present
// in the echoed output passes; the run succeeds and the persisted verdict
// names the contains validator.
func TestBuiltinContainsPasses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newValidationFixture(t, []validate.Validator{validate.NewContains()}, false)
	spec := dag.ValidatorSpec{Name: "contains", Config: json.RawMessage(`{"substring":"produce output"}`)}
	runID := f.submit(t, builtinValidationDef(t, spec))

	waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 || atts[0].Outcome == nil || *atts[0].Outcome != store.StepStatusSucceeded {
		t.Fatalf("attempts = %+v, want one succeeded", atts)
	}
	var v validate.Verdict
	if err := json.Unmarshal(atts[0].Verdict, &v); err != nil {
		t.Fatalf("unmarshaling verdict: %v", err)
	}
	if !v.Passed() || len(v.Results) != 1 || v.Results[0].Name != "contains" || v.Results[0].Status != validate.StatusPass {
		t.Fatalf("verdict = %+v, want a pass from the contains validator", v)
	}
}

// TestBuiltinJSONSchemaMalformedFails: a json_schema chain over an llm output
// whose /text is not JSON records validation_failed with a structured
// invalid_json issue (never a panic) and dead-letters.
func TestBuiltinJSONSchemaMalformedFails(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newValidationFixture(t, []validate.Validator{validate.NewJSONSchema()}, false)
	spec := dag.ValidatorSpec{Name: "json_schema", Config: json.RawMessage(`{"schema":{"type":"object"}}`)}
	runID := f.submit(t, builtinValidationDef(t, spec))

	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	requireStepStatuses(t, f.s, runID, map[string]string{"gen": store.StepStatusDeadLettered})

	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 || atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomeValidationFailed {
		t.Fatalf("attempts = %+v, want one validation_failed", atts)
	}
	var v validate.Verdict
	if err := json.Unmarshal(atts[0].Verdict, &v); err != nil {
		t.Fatalf("unmarshaling verdict: %v", err)
	}
	if v.Passed() {
		t.Fatalf("verdict = %+v, want a fail", v)
	}
	found := false
	for _, iss := range v.Issues {
		if iss.Validator == "json_schema" && iss.Code == "invalid_json" {
			found = true
		}
	}
	if !found {
		t.Fatalf("verdict issues = %+v, want an invalid_json issue from json_schema", v.Issues)
	}
}
