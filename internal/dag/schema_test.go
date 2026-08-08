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

// TestGeneratedSchemaIsCommitted is the local half of the CI drift check:
// the committed schema must match what the current structs generate.
func TestGeneratedSchemaIsCommitted(t *testing.T) {
	t.Parallel()

	want, err := dag.GenerateJSONSchema()
	if err != nil {
		t.Fatalf("GenerateJSONSchema: %v", err)
	}
	got, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("reading committed schema (run `make generate`?): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("committed JSON Schema is stale; run `make generate` and commit the result")
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
		"Definition", "Step", "Edge", "ParamSpec",
		"LLMConfig", "ToolConfig", "RetrieveConfig", "MapConfig", "PlannerConfig",
		"AgentConfig", "HumanApprovalConfig", "JoinConfig", "BranchConfig",
		"NoopConfig", "EchoConfig",
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
	} {
		if !bytes.Contains(data, []byte(marker)) {
			t.Errorf("generated schema does not contain %q", marker)
		}
	}
}
