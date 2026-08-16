package dag_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// schemaPath is the committed generated artifact, relative to this package.
const schemaPath = "../../docs/schema/workflow-definition.v1.json"

// planSchemaPath is the committed PlanOutput schema (ADR-015).
const planSchemaPath = "../../docs/schema/plan-output.v1.json"

// TestGeneratedSchemaIsCommitted is the local half of the CI drift check:
// the committed schemas must match what the current structs generate.
func TestGeneratedSchemaIsCommitted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		path string
		gen  func() ([]byte, error)
	}{
		{"workflow-definition", schemaPath, dag.GenerateJSONSchema},
		{"plan-output", planSchemaPath, dag.GeneratePlanOutputSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want, err := tc.gen()
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			got, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("reading committed schema (run `make generate`?): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Error("committed JSON Schema is stale; run `make generate` and commit the result")
			}
		})
	}
}

// TestGeneratedPlanSchemaContent pins the PlanOutput schema's shape: it must
// reuse the definition schema's step-config $defs so an injected step's config
// cannot drift from an authored step's.
func TestGeneratedPlanSchemaContent(t *testing.T) {
	t.Parallel()

	data, err := dag.GeneratePlanOutputSchema()
	if err != nil {
		t.Fatalf("GeneratePlanOutputSchema: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("generated plan schema is not valid JSON")
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("unmarshaling schema: %v", err)
	}
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("plan schema has no $defs object")
	}
	for _, name := range []string{
		"PlanOutput", "Step", "Edge", "LLMConfig", "PlannerConfig", "MapConfig", "JoinConfig",
	} {
		if _, ok := defs[name]; !ok {
			t.Errorf("plan schema $defs is missing %s", name)
		}
	}
	for _, marker := range []string{`"additionalProperties": false`, "oneOf", `"schema_version"`} {
		if !bytes.Contains(data, []byte(marker)) {
			t.Errorf("generated plan schema does not contain %q", marker)
		}
	}
}

func TestGeneratedSchemaContent(t *testing.T) {
	t.Parallel()

	data, err := dag.GenerateJSONSchema()
	if err != nil {
		t.Fatalf("GenerateJSONSchema: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("generated schema is not valid JSON")
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("unmarshaling schema: %v", err)
	}
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("schema has no $defs object")
	}
	// Every registered config struct must be present so the oneOf refs
	// resolve.
	for _, name := range []string{
		"Definition", "Step", "Edge", "ParamSpec", "RetryPolicy", "BackoffSpec",
		"CachePolicy", "CacheMode", "CacheScope",
		"ContextSpec", "ContextSource", "ContextSourceKind", "ContextMissingPolicy",
		"CompactionStrategy",
		"LLMConfig", "ToolConfig", "RetrieveConfig", "MapConfig", "PlannerConfig",
		"AgentConfig", "HumanApprovalConfig", "JoinConfig", "BranchConfig",
		"NoopConfig", "EchoConfig", "SleepConfig", "FailNTimesConfig", "CounterConfig",
	} {
		if _, ok := defs[name]; !ok {
			t.Errorf("$defs is missing %s", name)
		}
	}
	// Strictness and the ui carve-out must be visible in the schema.
	for _, marker := range []string{
		`"additionalProperties": false`,
		`"schema_version"`,
		`"max_iterations"`,
		`"const": "human_approval"`,
		"oneOf",
		"round-tripped byte-for-byte",
		`"on_failure"`,
		`"max_attempts"`,
		`"validation_failed"`,
		`"read_write"`,
		`"step_output"`,
		`"on_missing"`,
	} {
		if !bytes.Contains(data, []byte(marker)) {
			t.Errorf("generated schema does not contain %q", marker)
		}
	}
}
