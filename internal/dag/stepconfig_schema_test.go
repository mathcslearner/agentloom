package dag_test

// Ticket 8.1: per-plugin config schemas (ADR-009). StepConfigSchema is
// the generator behind executor manifests' config_schema; these tests
// pin that every catalog type yields a valid standalone document and
// that the shape is machine-usable (inlined properties, strict
// additionalProperties matching DisallowUnknownFields).

import (
	"encoding/json"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

func TestStepConfigSchemaAllCatalogTypes(t *testing.T) {
	t.Parallel()

	for _, st := range dag.StepTypes() {
		raw, err := dag.StepConfigSchema(st)
		if err != nil {
			t.Errorf("StepConfigSchema(%s): %v", st, err)
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("StepConfigSchema(%s) is not a JSON object: %v", st, err)
			continue
		}
		if doc["$schema"] == "" || doc["$schema"] == nil {
			t.Errorf("StepConfigSchema(%s) missing $schema", st)
		}
		// DisallowUnknownFields is the decode contract; the schema must
		// mirror it so UI forms reject what the engine would.
		if got, ok := doc["additionalProperties"].(bool); !ok || got {
			t.Errorf("StepConfigSchema(%s): additionalProperties = %v — want false", st, doc["additionalProperties"])
		}
	}
}

func TestStepConfigSchemaShape(t *testing.T) {
	t.Parallel()

	raw, err := dag.StepConfigSchema(dag.StepLLM)
	if err != nil {
		t.Fatalf("StepConfigSchema(llm): %v", err)
	}
	var doc struct {
		Title      string                     `json:"title"`
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshaling llm schema: %v", err)
	}
	if doc.Title != "llm step config" {
		t.Errorf("llm schema title = %q — want %q", doc.Title, "llm step config")
	}
	// Fields are inlined at the root (ExpandedStruct), so a UI form can
	// walk properties directly.
	for _, field := range []string{"model", "prompt", "messages", "max_tokens", "temperature"} {
		if _, ok := doc.Properties[field]; !ok {
			t.Errorf("llm schema missing property %q", field)
		}
	}
	// The nested message type lands in $defs.
	if _, ok := doc.Defs["LLMMessage"]; !ok {
		t.Errorf("llm schema $defs missing LLMMessage; has %v", keys(doc.Defs))
	}
}

func TestStepConfigSchemaUnknownType(t *testing.T) {
	t.Parallel()

	if _, err := dag.StepConfigSchema(dag.StepType("no_such_type")); err == nil {
		t.Fatal("StepConfigSchema on an unknown type succeeded — want an error")
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
