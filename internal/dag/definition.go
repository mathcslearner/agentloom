// Package dag implements the workflow definition contract decided in
// ADR-003: the Go types that are the source of truth for the JSON format,
// a strict decoder (unknown fields rejected with path-qualified errors),
// a canonical encoder (deterministic output, `ui` subtree round-tripped
// byte-for-byte), and JSON Schema generation for docs and UI forms.
//
// The package is a pure, IO-free library. Validate (ticket 1.3) enforces
// ADR-003's structural rules on decoded definitions, including the graph
// rules (1.4: only marked loop edges may form cycles, loop-edge ancestry);
// Graph (1.4) provides adjacency, topological order, reachability, and
// ReadySteps — ADR-003's readiness, skip-propagation, and join semantics.
// CompileExpr and CompiledExpr.Eval (1.5) compile the `when`/`condition`
// CEL predicates at validation time and evaluate them for the engine —
// evaluation errors are typed failures, never coerced to false.
package dag

import "encoding/json"

// CurrentSchemaVersion is the workflow definition format version this
// engine reads and writes. Decode rejects any other value (ADR-003:
// breaking changes bump the integer; additive changes do not).
const CurrentSchemaVersion = 1

// Definition is a complete workflow definition — the top-level JSON
// document (ADR-003 "Top-level shape").
type Definition struct {
	SchemaVersion int                  `json:"schema_version"`
	Name          string               `json:"name"`
	Description   string               `json:"description,omitempty"`
	Params        map[string]ParamSpec `json:"params,omitempty"`
	Steps         []Step               `json:"steps"`
	Edges         []Edge               `json:"edges"`

	// UI is the engine-opaque builder state: an arbitrary JSON object that
	// is never interpreted and is round-tripped byte-for-byte exactly as it
	// appeared in the source document.
	UI json.RawMessage `json:"ui,omitempty"`
}

// ParamType is the declared type of a run parameter.
type ParamType string

// Permitted param types.
const (
	ParamString  ParamType = "string"
	ParamNumber  ParamType = "number"
	ParamBoolean ParamType = "boolean"
	ParamObject  ParamType = "object"
	ParamArray   ParamType = "array"
)

// paramTypes enumerates the valid ParamType values, in documentation order.
var paramTypes = []ParamType{ParamString, ParamNumber, ParamBoolean, ParamObject, ParamArray}

// ParamSpec declares one run parameter callers may supply at submit time.
// Submitted values are validated against the declaration at run creation,
// not at definition validation.
type ParamSpec struct {
	Type     ParamType `json:"type"`
	Required bool      `json:"required,omitempty"`
}

// StepType identifies a step's executor and the shape of its config.
type StepType string

// The step-type catalog (ADR-003).
const (
	StepLLM           StepType = "llm"
	StepTool          StepType = "tool"
	StepRetrieve      StepType = "retrieve"
	StepMap           StepType = "map"
	StepPlanner       StepType = "planner"
	StepAgent         StepType = "agent"
	StepHumanApproval StepType = "human_approval"
	StepJoin          StepType = "join"
	StepBranch        StepType = "branch"
	StepNoop          StepType = "noop"
	StepEcho          StepType = "echo"
	StepSleep         StepType = "sleep"
	StepFailNTimes    StepType = "fail_n_times"
	StepCounter       StepType = "counter"
)

// Step is one node in the workflow graph.
type Step struct {
	ID   string   `json:"id"`
	Type StepType `json:"type"`

	// Config holds the typed per-type configuration (nil when the source
	// document had no config key). Its concrete type is the pointer config
	// struct registered for Type, e.g. *LLMConfig for StepLLM.
	Config StepConfig `json:"config,omitempty"`
}

// EdgeType distinguishes normal dependency edges from marked loop edges.
type EdgeType string

// Permitted edge types.
const (
	EdgeNormal EdgeType = "normal"
	EdgeLoop   EdgeType = "loop"
)

// Edge connects two steps. Loop edges (Type == EdgeLoop) are the only
// sanctioned cycles and carry Condition and MaxIterations; readiness and
// skip propagation ignore them (ADR-003 "Loop edges").
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`

	// When is an optional CEL predicate on the source step's completion;
	// empty means unconditioned. Compiled at validation time (ticket 1.5).
	When string `json:"when,omitempty"`

	// Type is EdgeNormal or EdgeLoop. Decode normalizes an absent type to
	// EdgeNormal; Encode omits EdgeNormal, so the canonical JSON spelling
	// of a normal edge has no "type" key.
	Type EdgeType `json:"type,omitempty"`

	// Condition is the loop-continuation CEL predicate; loop edges only.
	Condition string `json:"condition,omitempty"`

	// MaxIterations is the hard iteration bound; loop edges only. Zero
	// means the key was absent (invalid on a loop edge; enforced in 1.3).
	MaxIterations int `json:"max_iterations,omitempty"`
}

// IsLoop reports whether the edge is a marked loop edge.
func (e Edge) IsLoop() bool { return e.Type == EdgeLoop }

// MarshalJSON emits the canonical edge encoding: EdgeNormal is spelled as
// an absent "type" key.
func (e Edge) MarshalJSON() ([]byte, error) {
	type edgeJSON Edge // drop the method to avoid recursion
	ej := edgeJSON(e)
	if ej.Type == EdgeNormal {
		ej.Type = ""
	}
	return marshalNoEscape(ej)
}

// JoinMode selects a join step's fan-in semantics.
type JoinMode string

// Permitted join modes.
const (
	JoinAll JoinMode = "all"
	JoinAny JoinMode = "any"
)
