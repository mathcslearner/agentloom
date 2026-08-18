package event

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// eventsSchemaPath is the committed generated artifact, relative to this package.
const eventsSchemaPath = "../../docs/schema/events.v1.json"

// TestGeneratedEventSchemaIsCommitted is the local half of the CI drift check
// (ADR-018): the committed schema must match what the current structs generate.
func TestGeneratedEventSchemaIsCommitted(t *testing.T) {
	t.Parallel()

	want, err := GenerateSchema()
	if err != nil {
		t.Fatalf("GenerateSchema: %v", err)
	}
	got, err := os.ReadFile(eventsSchemaPath)
	if err != nil {
		t.Fatalf("reading committed schema (run `make generate`?): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("committed events JSON Schema is stale; run `make generate` and commit the result")
	}
}

// TestGeneratedEventSchemaContent pins the schema's shape: a oneOf variant per
// catalog type, discriminated by the envelope `type` const, and every payload
// def present.
func TestGeneratedEventSchemaContent(t *testing.T) {
	t.Parallel()

	data, err := GenerateSchema()
	if err != nil {
		t.Fatalf("GenerateSchema: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("generated event schema is not valid JSON")
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("unmarshaling schema: %v", err)
	}
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("event schema has no $defs object")
	}
	if _, ok := defs["Envelope"]; !ok {
		t.Fatal("event schema $defs is missing Envelope")
	}
	env, ok := defs["Envelope"].(map[string]any)
	if !ok {
		t.Fatal("Envelope def is not an object")
	}
	oneOf, ok := env["oneOf"].([]any)
	if !ok {
		t.Fatal("Envelope has no oneOf")
	}
	if len(oneOf) != len(Catalog) {
		t.Errorf("Envelope oneOf has %d variants, want %d (one per catalog entry)", len(oneOf), len(Catalog))
	}
	// Every payload struct must be present so the oneOf refs resolve.
	for _, name := range []string{
		"RunCreated", "StepClaimed", "StepSucceeded", "CostUpdated", "GraphExpanded",
		"ApprovalDecided", "ContextAssembled", "GuardTripped",
	} {
		if _, ok := defs[name]; !ok {
			t.Errorf("event schema $defs is missing %s", name)
		}
	}
	// The GraphExpanded delta reuses the PlanOutput's step-config shapes.
	for _, marker := range []string{
		`"schema_version"`,
		`"const": "cost_updated"`,
		`"const": "approval_requested"`,
		`"$ref": "#/$defs/GraphExpanded"`,
		`"additionalProperties": false`,
	} {
		if !bytes.Contains(data, []byte(marker)) {
			t.Errorf("generated event schema does not contain %q", marker)
		}
	}
}
