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
	StepLLM, StepTool, StepRetrieve, StepMap, StepGather, StepPlanner, StepAgent,
	StepHumanApproval, StepJoin, StepBranch, StepNoop, StepEcho,
	StepSleep, StepFailNTimes, StepCounter, StepEffectfulEcho,
	StepBlackboardWrite,
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

	if err := bindStepConfigs(r, root); err != nil {
		return nil, fmt.Errorf("dag: JSONSchema: %w", err)
	}

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
	return generateSchema(JSONSchema)
}

// bindStepConfigs pulls the per-type config structs (reachable only through
// the StepConfig interface, which reflection cannot see through) into the
// root's shared $defs and binds each step type to its config shape via the
// Step definition's oneOf. Shared by the definition schema and the PlanOutput
// schema, whose injected steps carry the identical config shapes.
func bindStepConfigs(r *jsonschema.Reflector, root *jsonschema.Schema) error {
	for _, st := range stepTypes {
		cfgSchema := r.Reflect(stepConfigTypes[st]())
		for name, def := range cfgSchema.Definitions {
			root.Definitions[name] = def
		}
	}
	stepDef, ok := root.Definitions["Step"]
	if !ok || stepDef.Properties == nil {
		return fmt.Errorf("reflected schema has no Step definition")
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
	return nil
}

// PlanOutputSchema builds the JSON Schema for a planner step's PlanOutput
// document (ADR-015), from the same Go structs DecodePlanOutput uses. It
// reuses the definition schema's step-config $defs, so an injected step's
// config shape cannot drift from an authored step's. The generator command
// writes it to docs/schema/, and it feeds the planner's implicit json_schema
// validator (M11) and the builder UI.
func PlanOutputSchema() (*jsonschema.Schema, error) {
	r := &jsonschema.Reflector{Anonymous: true}
	root := r.Reflect(&PlanOutput{})
	root.Version = jsonschema.Version
	root.Title = "agentloom planner PlanOutput"
	root.Description = fmt.Sprintf(
		"Planner expansion delta, schema_version %d (ADR-015): new steps and edges spliced into the running graph. Unknown fields are rejected.",
		PlanSchemaVersion)
	if err := bindStepConfigs(r, root); err != nil {
		return nil, fmt.Errorf("dag: PlanOutputSchema: %w", err)
	}
	return root, nil
}

// GeneratePlanOutputSchema renders the PlanOutput JSON Schema exactly as it
// is committed under docs/schema/: indented, trailing newline.
func GeneratePlanOutputSchema() ([]byte, error) {
	return generateSchema(PlanOutputSchema)
}

// generateSchema renders a schema builder's output in the committed form.
func generateSchema(build func() (*jsonschema.Schema, error)) ([]byte, error) {
	schema, err := build()
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

// StepConfigSchema renders one step type's config struct as a standalone
// JSON Schema document (ticket 8.1, ADR-009): the plugin SPI's
// per-plugin config schema, attached to executor manifests by
// exec.Registry and served on GET /v1/plugins for UI forms (M17.4). The
// struct's fields are inlined at the document root (nested types land in
// $defs), from the same structs the decoder uses — the schema cannot
// drift from what the engine accepts. Like the definition schema, it is
// documentation-grade: which fields are required per type stays
// validation's job (ADR-003).
func StepConfigSchema(st StepType) (json.RawMessage, error) {
	mk, ok := stepConfigTypes[st]
	if !ok {
		return nil, fmt.Errorf("dag: no config type registered for step type %q", st)
	}
	r := &jsonschema.Reflector{Anonymous: true, ExpandedStruct: true}
	schema := r.Reflect(mk())
	schema.Version = jsonschema.Version
	schema.Title = fmt.Sprintf("%s step config", st)
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("dag: marshaling %s config schema: %w", st, err)
	}
	return data, nil
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

// JSONSchema declares the map item-failure-policy enum in the generated schema
// (ADR-015, ticket 13.4b).
func (ItemFailurePolicy) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(itemFailurePolicies)}
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
// full vocabulary is published; which classes `retry_on` admits (the
// retryable subset — validation_failed is a semantic retry under the
// validation policy, not a transport class, ADR-013) is validation, like
// the branch edge-firing rule (ADR-003: the schema is documentation-grade,
// Decode and Validate are the authority).
func (ErrorClass) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(errorClasses)}
}

// JSONSchema declares the jitter-mode enum in the generated schema.
func (JitterMode) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(jitterModes)}
}

// JSONSchema declares the cache-mode enum in the generated schema (ADR-011).
func (CacheMode) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(cacheModes)}
}

// JSONSchema declares the cache-scope enum in the generated schema (ADR-011).
func (CacheScope) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(cacheScopes)}
}

// JSONSchema declares the budget-policy enum in the generated schema (ADR-012).
func (BudgetPolicy) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(budgetPolicies)}
}

// JSONSchema declares the context-source-kind enum in the generated schema
// (ADR-014).
func (ContextSourceKind) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(contextSourceKinds)}
}

// JSONSchema declares the context missing-source-policy enum in the generated
// schema (ADR-014).
func (ContextMissingPolicy) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Enum: enumAny(contextMissingPolicies)}
}
