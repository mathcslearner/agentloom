package dag

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/invopop/jsonschema"
)

// stepTypes enumerates the step-type catalog in documentation order; it
// fixes the order of the generated schema's step variants.
var stepTypes = []StepType{
	StepLLM, StepTool, StepRetrieve, StepMap, StepPlanner, StepAgent,
	StepHumanApproval, StepJoin, StepBranch, StepNoop, StepEcho,
	StepSleep, StepFailNTimes, StepCounter,
}

// StepTypes returns the step-type catalog in documentation order. It
// exists for exhaustiveness checks outside the package — notably the
// exec-registry sync test pinning which catalog types carry executors
// (post-M4 audit) — never for validation, which consults the catalog
// maps directly.
func StepTypes() []StepType { return append([]StepType(nil), stepTypes...) }

// JSONSchema builds the JSON Schema for the workflow definition format
// from the same Go structs the decoder uses, so the published schema
// cannot drift from what the engine accepts. The schema is documentation-
// grade (docs and UI forms); Decode remains the authority — in particular
// the branch edge-firing rule and the `ui` byte-for-byte round-trip are
// not expressible here (ADR-003).
//
// The generator command internal/dag/gen writes the result to
// docs/schema/, and CI fails if the committed file is stale.
func JSONSchema() (*jsonschema.Schema, error) {
	r := &jsonschema.Reflector{Anonymous: true}
	root := r.Reflect(&Definition{})
	root.Version = jsonschema.Version
	root.Title = "agentloom workflow definition"
	root.Description = fmt.Sprintf(
		"Workflow definition format, schema_version %d (ADR-003). Unknown fields are rejected everywhere except the engine-opaque ui block.",
		CurrentSchemaVersion)

	// The per-type config structs are reachable only through the StepConfig
	// interface, which reflection cannot see through; reflect each one
	// explicitly into the shared $defs.
	for _, st := range stepTypes {
		cfgSchema := r.Reflect(stepConfigTypes[st]())
		for name, def := range cfgSchema.Definitions {
			root.Definitions[name] = def
		}
	}

	stepDef, ok := root.Definitions["Step"]
	if !ok || stepDef.Properties == nil {
		return nil, fmt.Errorf("dag: JSONSchema: reflected schema has no Step definition")
	}
	stepDef.Properties.Set("config", &jsonschema.Schema{
		Type:        "object",
		Description: "Typed per step type; Step's oneOf variants bind each type to its config shape.",
	})
	variants := make([]*jsonschema.Schema, 0, len(stepTypes))
	for _, st := range stepTypes {
		props := jsonschema.NewProperties()
		props.Set("type", &jsonschema.Schema{Const: string(st)})
		props.Set("config", &jsonschema.Schema{Ref: "#/$defs/" + configTypeName(st)})
		variants = append(variants, &jsonschema.Schema{Properties: props})
	}
	stepDef.OneOf = variants

	defDef, ok := root.Definitions["Definition"]
	if !ok || defDef.Properties == nil {
		return nil, fmt.Errorf("dag: JSONSchema: reflected schema has no Definition definition")
	}
	defDef.Properties.Set("ui", &jsonschema.Schema{
		Type:        "object",
		Description: "Engine-opaque builder state: never validated or interpreted, round-tripped byte-for-byte.",
	})

	return root, nil
}

// GenerateJSONSchema renders the workflow definition JSON Schema exactly
// as it is committed under docs/schema/: indented, trailing newline. The
// generator command and the drift test both use this, so there is a single
// rendering to keep in sync.
func GenerateJSONSchema() ([]byte, error) {
	schema, err := JSONSchema()
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("dag: marshaling JSON Schema: %w", err)
	}
	return append(data, '\n'), nil
}

// configTypeName is the $defs name of a step type's config struct.
func configTypeName(st StepType) string {
	return reflect.TypeOf(stepConfigTypes[st]()).Elem().Name()
}

// enumAny renders an enum value list for jsonschema.
func enumAny[T ~string](vals []T) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = string(v)
	}
	return out
}

// JSONSchema declares the step-type enum in the generated schema.
func (StepType) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(stepTypes)}
}

// JSONSchema declares the edge-type enum in the generated schema.
func (EdgeType) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny([]EdgeType{EdgeNormal, EdgeLoop})}
}

// JSONSchema declares the join-mode enum in the generated schema.
func (JoinMode) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny([]JoinMode{JoinAll, JoinAny})}
}

// JSONSchema declares the param-type enum in the generated schema.
func (ParamType) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(paramTypes)}
}

// JSONSchema declares the failure-policy enum in the generated schema.
func (FailurePolicy) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(failurePolicies)}
}

// JSONSchema declares the error-class enum in the generated schema. The
// full vocabulary is published; which classes `retry_on` admits today
// (the retryable subset — validation_failed is reserved for M11) is
// validation, like the branch edge-firing rule (ADR-003: the schema is
// documentation-grade, Decode and Validate are the authority).
func (ErrorClass) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(errorClasses)}
}

// JSONSchema declares the jitter-mode enum in the generated schema.
func (JitterMode) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(jitterModes)}
}
