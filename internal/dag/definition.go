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
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`

	// OnFailure is the workflow failure policy (ADR-006): what happens
	// to the run when a step dead-letters. Empty means the key was
	// absent — the engine default, FailFast. Encode omits the empty
	// value, so the canonical spelling of the default is no key.
	OnFailure FailurePolicy `json:"on_failure,omitempty"`

	// MaxWallClock is the run's optional wall-clock deadline (ticket 5.6),
	// a Go duration string; empty means the key was absent (no deadline).
	// The clock starts at run creation — instantiation materializes
	// created_at + max_wall_clock onto the run — and a run still unfinished
	// past it is cancelled with reason `deadline_exceeded` (ADR-006).
	MaxWallClock string `json:"max_wall_clock,omitempty"`

	// BudgetUSD is the run's optional overall spend budget in US dollars
	// (ADR-012); nil means the key was absent (unbudgeted). Instantiation
	// materializes it onto the run in nano-USD, and the claim path refuses a
	// step whose projected spend (run spend so far + pre-flight estimate)
	// would exceed it, taking the OnBudgetExceeded action.
	BudgetUSD *float64 `json:"budget_usd,omitempty"`

	// OnBudgetExceeded is the action when a claim's projected spend would
	// exceed BudgetUSD (ADR-012): park (the resumable default) or fail.
	// Empty means the key was absent — the engine default, BudgetPark.
	// Encode omits the empty value, so the canonical spelling of the default
	// is no key. Has no effect without BudgetUSD (Validate rejects that).
	OnBudgetExceeded BudgetPolicy `json:"on_budget_exceeded,omitempty"`

	// Expansion is the run's optional dynamic-expansion policy (ADR-015):
	// the caps that bound how far planner steps (and map fan-out / loop
	// unrolling, M13+) may grow the run's graph at runtime. Nil when the
	// source document had no expansion key — the compiled defaults apply.
	// Uniform across the run, so it lives at the top level, not on a step.
	Expansion *ExpansionPolicy `json:"expansion,omitempty"`

	// Templates is the run's optional library of named sub-templates
	// (ADR-015, ticket 13.4): each is a reusable sub-graph a `map` step
	// (MapConfig.body) instantiates once per runtime list item. Templates are
	// a library, NOT active steps — they are never instantiated at run
	// creation, only validated and carried in the definition snapshot so a map
	// expansion can read them at runtime. Nil when the document had no
	// templates key.
	Templates map[string]*Template `json:"templates,omitempty"`

	// Agents is the run's optional library of named agent roles (ADR-016,
	// ticket 14.1): each carries a role's default model-call configuration
	// (system prompt, model + fallbacks, allowed toolset, default validators,
	// default context spec). An `agent` step references one by name and merges
	// its own step-level overrides over the role's defaults (ResolveAgentStep).
	// Like `templates`, agents are a library — never instantiated as active
	// steps themselves — but unlike templates they are resolved into the run's
	// materialized step rows at instantiation, so the agents section is not
	// needed at claim time. Nil when the document had no agents key.
	Agents map[string]*AgentDef `json:"agents,omitempty"`

	Params map[string]ParamSpec `json:"params,omitempty"`
	Steps  []Step               `json:"steps"`
	Edges  []Edge               `json:"edges"`

	// UI is the engine-opaque builder state: an arbitrary JSON object that
	// is never interpreted and is round-tripped byte-for-byte exactly as it
	// appeared in the source document.
	UI json.RawMessage `json:"ui,omitempty"`
}

// Template is one named sub-template in the definition's `templates` library
// (ADR-015, ticket 13.4): a reusable sub-graph a `map` step instantiates once
// per runtime list item. Its steps reference the current item through the
// reserved `${{ item }}` / `${{ item_index }}` roots, and each other through
// ordinary `${{ steps.<id>.output }}` refs; at expansion time the engine
// rewrites both per instance (item → the map's output, `steps.x` → `steps.x#k`)
// and splices the instances between the map step and a generated gather join.
// Structurally identical to a mini definition graph (steps + edges), and
// validated as one (unique local ids, acyclic, exactly one sink step whose
// output the gather collects), but never a run's active graph.
type Template struct {
	Steps []Step `json:"steps"`
	Edges []Edge `json:"edges,omitempty"`
}

// AgentDef is one named agent role in the definition's `agents` library
// (ADR-016, ticket 14.1): the default model-call configuration a role brings
// to every `agent` step that references it. An `agent` step supplies the task
// (its prompt/messages) and may override any of these defaults; the effective
// step is the per-field merge of the step over the role (ResolveAgentStep),
// materialized into the run's step rows at instantiation so the executor and
// the engine's validate/context stages see a fully-configured step and never
// re-read this section at claim time.
//
// The role carries config-level defaults (System, Model, ModelFallbacks,
// Tools, MaxTokens, Temperature, OutputFormat) and two envelope-level defaults
// (Validation, Context) — the fields the multi-agent flows (M14.2+) share
// across a role. Retry/timeout/cache/budget/blackboard stay step-level only
// (a role has no opinion on transport policy). The role has no prompt of its
// own: the task text always comes from the step.
type AgentDef struct {
	// Role is the human-readable role name (researcher/critic/writer). It is
	// metadata for the handoff thread (M14.2) and observability; empty is
	// allowed (the agents-map key already names the role).
	Role string `json:"role,omitempty"`

	// System is the role's system prompt — the standing instructions every
	// step in this role carries. Projected onto the provider request's system
	// field. Empty means no default system prompt.
	System string `json:"system,omitempty"`

	// Model is the role's default model id (routed through the same provider
	// registry as an llm step). A step may override it. Empty means the step
	// must supply the model (the merged step is invalid without one).
	Model string `json:"model,omitempty"`

	// ModelFallbacks is the role's default downgrade chain (ADR-012): the same
	// ordered cheaper-model list an llm step carries. Overridden wholesale by a
	// step-level chain when present.
	ModelFallbacks []ModelFallback `json:"model_fallbacks,omitempty"`

	// Tools is the role's allowed toolset (ADR-016): the tool names the agent
	// may call. In 14.1 the toolset is enforced by rejection — a model
	// response naming a tool outside this set fails the step permanently; the
	// tools are not yet offered to the model or executed (deferred). Overridden
	// wholesale by a step-level toolset (including an explicit empty list, which
	// forbids all tools).
	Tools []string `json:"tools,omitempty"`

	// MaxTokens is the role's default completion bound; a step may override it.
	// Zero means absent (the executor's default applies).
	MaxTokens int `json:"max_tokens,omitempty"`

	// Temperature is the role's default sampling temperature; a step may
	// override it. Pointer so an explicit 0 survives; nil means absent.
	Temperature *float64 `json:"temperature,omitempty"`

	// OutputFormat is the role's default structured-output policy (ADR-013).
	// Overridden wholesale by a step-level output_format when present.
	OutputFormat *OutputFormat `json:"output_format,omitempty"`

	// Validation is the role's default output-validation chain (ADR-013). A
	// step-level `validation` block replaces it wholesale (block-level
	// override, not a deep merge). Materialized onto the merged step's
	// validation_policy so it flows through the engine's ordinary validate
	// stage.
	Validation *ValidationPolicy `json:"validation,omitempty"`

	// Context is the role's default context-assembly spec (ADR-014). A
	// step-level `context` block replaces it wholesale. Materialized onto the
	// merged step's context_policy.
	Context *ContextSpec `json:"context,omitempty"`
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
	StepLLM             StepType = "llm"
	StepTool            StepType = "tool"
	StepRetrieve        StepType = "retrieve"
	StepMap             StepType = "map"
	StepGather          StepType = "gather"
	StepPlanner         StepType = "planner"
	StepAgent           StepType = "agent"
	StepHumanApproval   StepType = "human_approval"
	StepJoin            StepType = "join"
	StepBranch          StepType = "branch"
	StepNoop            StepType = "noop"
	StepEcho            StepType = "echo"
	StepSleep           StepType = "sleep"
	StepFailNTimes      StepType = "fail_n_times"
	StepCounter         StepType = "counter"
	StepEffectfulEcho   StepType = "effectful_echo"
	StepBlackboardWrite StepType = "blackboard_write"
)

// IsLLMFamily reports whether the step type produces a model completion —
// llm, and the future planner/agent (ADR-013). These share the `/text`
// default validation target and are the step types on which output_format
// and the semantic-retry feedback template apply.
func (t StepType) IsLLMFamily() bool {
	switch t {
	case StepLLM, StepPlanner, StepAgent:
		return true
	default:
		return false
	}
}

// Step is one node in the workflow graph.
type Step struct {
	ID   string   `json:"id"`
	Type StepType `json:"type"`

	// Config holds the typed per-type configuration (nil when the source
	// document had no config key). Its concrete type is the pointer config
	// struct registered for Type, e.g. *LLMConfig for StepLLM.
	Config StepConfig `json:"config,omitempty"`

	// Retry is the step's authored retry policy (ADR-006); nil when the
	// source document had no retry key (engine defaults apply). Uniform
	// across step types, so it lives on the step envelope, not in the
	// per-type config.
	Retry *RetryPolicy `json:"retry,omitempty"`

	// Timeout is the step's per-attempt execution timeout (ticket 5.3), a
	// Go duration string; empty means the key was absent (no timeout). The
	// clock starts when the executor starts — queue wait and claim latency
	// do not count — and expiry cancels the executor's context; the attempt
	// records the `timeout` class and routes through the retry policy.
	// Uniform across step types, so it lives on the step envelope.
	Timeout string `json:"timeout,omitempty"`

	// Cache is the step's authored response-cache policy (ADR-011, ticket
	// 9.4); nil when the source document had no cache key (the engine's
	// default policy applies — cache deterministic cache-eligible steps,
	// bypass the rest). Uniform across step types, so it lives on the step
	// envelope, not in the per-type config.
	Cache *CachePolicy `json:"cache,omitempty"`

	// Budget is the step's authored budget caps (ADR-012, ticket 10.3); nil
	// when the source document had no budget key (only the run budget
	// applies). Uniform across step types, so it lives on the step envelope,
	// not in the per-type config.
	Budget *StepBudget `json:"budget,omitempty"`

	// Validation is the step's authored output-validation chain (ADR-013,
	// ticket 11.1); nil when the source document had no validation key (the
	// output is accepted as produced). Uniform across step types, so it
	// lives on the step envelope, not in the per-type config.
	Validation *ValidationPolicy `json:"validation,omitempty"`

	// Blackboard is the step's authored blackboard block (ADR-014, ticket
	// 12.2): declarative writes to the run-scoped blackboard applied on the
	// step's success. Nil when the source document had no blackboard key (no
	// declarative writes). Uniform across step types, so it lives on the step
	// envelope, not in the per-type config.
	Blackboard *BlackboardPolicy `json:"blackboard,omitempty"`

	// Context is the step's authored context-assembly spec (ADR-014, ticket
	// 12.3): an ordered list of sources (upstream step outputs, blackboard
	// entries by key/tag, retrieval results, literals) assembled into the
	// provider request before the call, with per-source token caps and pinned
	// entries always included. Nil when the source document had no context key
	// (the request is built from the config alone). llm-family only — a
	// context spec on a step type that makes no provider request can never
	// fire (Validate rejects it).
	Context *ContextSpec `json:"context,omitempty"`
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
