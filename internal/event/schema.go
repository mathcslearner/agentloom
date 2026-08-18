package event

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/invopop/jsonschema"
)

// Schema builds the JSON Schema for the event feed (ADR-018) from the same Go
// structs the store marshals and the decode path unmarshals, so the published
// schema — the basis for the generated TS types (M16.5) — cannot drift from
// what the engine writes. It renders the envelope with a oneOf over the catalog
// payloads, discriminated by the envelope `type` const.
//
// The generator command internal/dag/gen writes the result to
// docs/schema/events.v1.json, and CI fails if the committed file is stale.
func Schema() (*jsonschema.Schema, error) {
	r := &jsonschema.Reflector{Anonymous: true}
	root := r.Reflect(&Envelope{})
	root.Version = jsonschema.Version
	root.Title = "agentloom event feed"
	root.Description = fmt.Sprintf(
		"Per-run event feed envelope, schema_version %d (ADR-018). Ordering is by seq; delivery is at-least-once (dedupe by run_id+seq). One payload variant per event type.",
		SchemaVersion)

	envDef, ok := root.Definitions["Envelope"]
	if !ok || envDef.Properties == nil {
		return nil, fmt.Errorf("event: Schema: reflected schema has no Envelope definition")
	}

	// Pull each catalog payload's schema (and its nested deps) into the shared
	// $defs, and bind the envelope's payload to a oneOf of (type const, payload
	// $ref) variants in catalog order.
	variants := make([]*jsonschema.Schema, 0, len(Catalog))
	for _, e := range Catalog {
		name := reflect.TypeOf(e.New()).Elem().Name()
		pSchema := r.Reflect(e.New())
		for defName, def := range pSchema.Definitions {
			root.Definitions[defName] = def
		}
		props := jsonschema.NewProperties()
		props.Set("type", &jsonschema.Schema{Const: string(e.Type)})
		props.Set("payload", &jsonschema.Schema{Ref: "#/$defs/" + name})
		variants = append(variants, &jsonschema.Schema{Properties: props})
	}
	// The base `payload` property is a generic object; the oneOf narrows it per
	// type. `type` stays a string enum of the whole vocabulary.
	envDef.Properties.Set("payload", &jsonschema.Schema{Type: "object"})
	envDef.Properties.Set("type", &jsonschema.Schema{Type: "string", Enum: typeEnum()})
	envDef.OneOf = variants

	return root, nil
}

// typeEnum is the event-type vocabulary as an enum value list.
func typeEnum() []any {
	out := make([]any, 0, len(Catalog))
	for _, e := range Catalog {
		out = append(out, string(e.Type))
	}
	return out
}

// GenerateSchema renders the event feed JSON Schema exactly as it is committed
// under docs/schema/: indented, trailing newline. The generator command and the
// drift test both use this, so there is a single rendering to keep in sync.
func GenerateSchema() ([]byte, error) {
	schema, err := Schema()
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("event: marshaling JSON Schema: %w", err)
	}
	return append(data, '\n'), nil
}
