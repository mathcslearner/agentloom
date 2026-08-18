package dag

import "encoding/json"

// StepConfig is implemented by every per-step-type config struct. Ticket
// 1.2 enforces config *shape* only (unknown fields, field types); which
// fields are required per type is structural validation (ticket 1.3), so
// every field below is optional at the decoding level.
type StepConfig interface {
	stepConfig()
}

// stepConfigTypes registers the config struct for each step type. It
// drives config decoding (decode.go) and JSON Schema generation
// (schema.go); adding a step type means adding a struct and one entry
// here.
var stepConfigTypes = map[StepType]func() StepConfig{
	StepLLM:             func() StepConfig { return &LLMConfig{} },
	StepTool:            func() StepConfig { return &ToolConfig{} },
	StepRetrieve:        func() StepConfig { return &RetrieveConfig{} },
	StepMap:             func() StepConfig { return &MapConfig{} },
	StepGather:          func() StepConfig { return &GatherConfig{} },
	StepPlanner:         func() StepConfig { return &PlannerConfig{} },
	StepAgent:           func() StepConfig { return &AgentConfig{} },
	StepHumanApproval:   func() StepConfig { return &HumanApprovalConfig{} },
	StepJoin:            func() StepConfig { return &JoinConfig{} },
	StepBranch:          func() StepConfig { return &BranchConfig{} },
	StepNoop:            func() StepConfig { return &NoopConfig{} },
	StepEcho:            func() StepConfig { return &EchoConfig{} },
	StepSleep:           func() StepConfig { return &SleepConfig{} },
	StepFailNTimes:      func() StepConfig { return &FailNTimesConfig{} },
	StepCounter:         func() StepConfig { return &CounterConfig{} },
	StepEffectfulEcho:   func() StepConfig { return &EffectfulEchoConfig{} },
	StepBlackboardWrite: func() StepConfig { return &BlackboardWriteConfig{} },
}

// LLMMessage is one entry of an llm step's messages array.
type LLMMessage struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// LLMConfig configures an llm step: one model call (executor: M8).
// Requires model and exactly one of prompt or messages (enforced in 1.3).
type LLMConfig struct {
	Model string `json:"model,omitempty"`

	// System is the optional system prompt, projected onto the provider
	// request's system field (ADR-016). Empty means no system prompt — the
	// prior behavior every unset step keeps. It coexists with prompt/messages
	// (the system prompt is not one of the two conversational inputs), so the
	// prompt-xor-messages rule is unaffected.
	System string `json:"system,omitempty"`

	Prompt    string       `json:"prompt,omitempty"`
	Messages  []LLMMessage `json:"messages,omitempty"`
	MaxTokens int          `json:"max_tokens,omitempty"`

	// Temperature is a pointer because an explicit 0 (deterministic
	// sampling) must survive canonical re-encoding; nil means absent.
	Temperature *float64 `json:"temperature,omitempty"`

	// ModelFallbacks is the ordered cheaper-model chain the M10.4 budget
	// middleware routes a claim to when the run approaches its budget
	// (ADR-012). Entries are primary → cheaper → cheapest; the deepest
	// entry whose soft threshold (at_budget_fraction) is met, or the first
	// entry whose projected spend fits, wins. Empty (the common case) means
	// a step never downgrades — the run parks or fails at its cap instead.
	// A different model is a different cost/cache resource, so a downgrade
	// re-keys the response cache automatically.
	ModelFallbacks []ModelFallback `json:"model_fallbacks,omitempty"`

	// OutputFormat opts the step into structured output (ADR-013, ticket
	// 11.3): a provider-native structured-output request and/or a
	// deterministic JSON-repair pass over the completion, plus an implicit
	// json_schema validator that enforces the declared shape. Nil (the
	// common case) leaves the completion as plain text. The block is
	// llm-only, like model_fallbacks.
	OutputFormat *OutputFormat `json:"output_format,omitempty"`
}

// OutputFormatType selects how an llm step's output is shaped (ADR-013).
type OutputFormatType string

// The output-format types (ADR-013, ticket 11.3).
const (
	// OutputFormatJSON requires the completion to be valid JSON of any
	// shape — a parseability check only. The provider is asked for a
	// JSON-object response when it supports one (mode auto), and the
	// completion is JSON-repaired before an implicit parseability validator
	// runs.
	OutputFormatJSON OutputFormatType = "json"

	// OutputFormatJSONSchema requires the completion to satisfy a specific
	// JSON Schema (the Schema field). The provider is asked to emit output
	// matching the schema when it supports one (mode auto), the completion
	// is JSON-repaired, and an implicit json_schema validator enforces the
	// schema.
	OutputFormatJSONSchema OutputFormatType = "json_schema"
)

// outputFormatTypes enumerates the valid OutputFormatType values in
// documentation order.
var outputFormatTypes = []OutputFormatType{OutputFormatJSON, OutputFormatJSONSchema}

// OutputFormatMode selects how aggressively the engine reshapes an llm step's
// output (ADR-013).
type OutputFormatMode string

// The output-format modes (ADR-013, ticket 11.3).
const (
	// OutputFormatAuto (the default when mode is absent) asks the provider
	// for native structured output when it can, and JSON-repairs whatever
	// text comes back otherwise.
	OutputFormatAuto OutputFormatMode = "auto"

	// OutputFormatRepairOnly never changes the provider request — the model
	// answers exactly as it would without a format — and only applies the
	// deterministic JSON-repair pass afterward. The escape hatch for models
	// whose native structured mode suppresses reasoning the workflow needs.
	OutputFormatRepairOnly OutputFormatMode = "repair_only"
)

// outputFormatModes enumerates the valid OutputFormatMode values in
// documentation order.
var outputFormatModes = []OutputFormatMode{OutputFormatAuto, OutputFormatRepairOnly}

// OutputFormat is an llm step's authored structured-output policy (ADR-013,
// ticket 11.3). Nil means plain text (today's behavior). The Schema is a
// runtime artifact (its compilability is checked at claim pre-flight by the
// implicit validator, not at submit — the tool-args / validator-config
// precedent, ADR-009), so submit-time validation checks only presence and
// shape.
type OutputFormat struct {
	// Type selects the output shape; required when an `output_format` block
	// is present (an empty block is meaningless).
	Type OutputFormatType `json:"type,omitempty"`

	// Schema is the JSON Schema (2020-12) the output must satisfy; required
	// when Type is json_schema, and must be a JSON object. Ignored (and
	// rejected as an authoring mistake) when Type is json.
	Schema json.RawMessage `json:"schema,omitempty"`

	// Mode selects native-vs-repair-only behavior; empty means auto.
	Mode OutputFormatMode `json:"mode,omitempty"`
}

// ModelFallback is one tier in an llm step's downgrade chain (ADR-012,
// ticket 10.4): a cheaper Model to route to, and an optional soft trigger.
type ModelFallback struct {
	// Model is the fallback's model id (routed through the same provider
	// registry as the primary). Required, and distinct from the primary and
	// from every other fallback.
	Model string `json:"model,omitempty"`

	// AtBudgetFraction is the soft downgrade trigger: once the run's spend
	// reaches this fraction of budget_usd, claims route to this tier (or a
	// deeper one). In (0, 1); nil means this tier is reached only by the
	// hard trigger (a projection that would exceed the budget). Requires the
	// definition to carry a budget_usd (a fraction of nothing cannot fire).
	AtBudgetFraction *float64 `json:"at_budget_fraction,omitempty"`
}

// ToolConfig configures a tool step: one invocation through the tool SPI
// (executor: M8). Input is the tool's opaque input payload.
type ToolConfig struct {
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// RetrieveConfig configures a retrieve step: a retrieval query through the
// retriever SPI (executor: M8). TopK zero means absent (executor default).
type RetrieveConfig struct {
	Retriever string `json:"retriever,omitempty"`
	Query     string `json:"query,omitempty"`
	TopK      int    `json:"top_k,omitempty"`
}

// MapConfig configures a map step: runtime-sized fan-out over a list
// (executor: M13, ADR-015). On the map step's completion the engine reads the
// resolved Items list, instantiates the named Body sub-template once per item
// (rewriting `${{ item }}`/`${{ item_index }}` and internal step refs per
// instance), and generates a gather join collecting the ordered instance
// results — all applied atomically through store.ExpandRun.
type MapConfig struct {
	// Items is the runtime list to map over. Usually a whole-expression
	// template (`"${{ steps.list.output.ids }}"`) that renders to a JSON
	// array before the map executor runs; a literal array is also valid. Held
	// as raw JSON so the rendered array (not a string) decodes into it.
	Items json.RawMessage `json:"items,omitempty"`

	// Body names the sub-template (definition `templates` section) to
	// instantiate per item. Required.
	Body string `json:"body,omitempty"`

	// MaxItems caps how many items this map may fan out over (ADR-015). Zero
	// means the key was absent — the run's per-expansion cap applies. An
	// explicit value must be positive; a list longer than it fails the step
	// permanently at execution time, before any expansion.
	MaxItems int `json:"max_items,omitempty"`

	// OnItemFailure is what happens when an instance fails terminally
	// (ADR-015, ticket 13.4b). Empty means the key was absent — the default,
	// FailFast (an item failure fails the run per the run's on_failure policy;
	// the gather never gathers). CollectErrors instead settles the failed
	// instance as `collected` with an error marker, keeps the run alive, and
	// lets the gather emit the ordered array with an error slot for the failed
	// item. CollectErrors requires a single-step body (13.4b restriction).
	OnItemFailure ItemFailurePolicy `json:"on_item_failure,omitempty"`
}

// ItemFailurePolicy selects how a map handles an instance's terminal failure
// (ADR-015, ticket 13.4b).
type ItemFailurePolicy string

// The item-failure policies.
const (
	// ItemFailFast (the default when absent) lets an item failure fail the run
	// per the run's on_failure policy — the gather never produces a result.
	ItemFailFast ItemFailurePolicy = "fail_fast"

	// ItemCollectErrors settles a failed instance as `collected` with an error
	// marker, keeps the run alive, and lets the gather emit the ordered result
	// array with an error slot for the failed item.
	ItemCollectErrors ItemFailurePolicy = "collect_errors"
)

// itemFailurePolicies enumerates the valid ItemFailurePolicy values in
// documentation order.
var itemFailurePolicies = []ItemFailurePolicy{ItemFailFast, ItemCollectErrors}

// GatherConfig configures a gather step: a fan-in barrier that emits its
// resolved Items array as output (executor: M13, ADR-015). Gather steps are
// normally engine-generated by a map expansion — the map fills Items with an
// ordered list of `${{ steps.<sink>#k.output }}` references that resolve to the
// per-instance results — so an authored gather is rare (Items may also be a
// literal array).
type GatherConfig struct {
	// Items is the ordered result array to emit. Held as raw JSON so a
	// rendered array of instance outputs decodes into it.
	Items json.RawMessage `json:"items,omitempty"`
}

// PlannerConfig configures a planner step: an LLM call whose validated
// output (a PlanOutput document, ADR-015) injects steps and edges into the
// running graph atomically with the planner's completion (executor: M13).
// M1 requires the same keys as llm; ADR-015 adds the per-expansion cap.
type PlannerConfig struct {
	Model       string       `json:"model,omitempty"`
	Prompt      string       `json:"prompt,omitempty"`
	Messages    []LLMMessage `json:"messages,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`

	// MaxAddedSteps caps how many steps this planner's plan may inject in a
	// single expansion (ADR-015). Zero means the key was absent — the run's
	// resolved per-expansion cap applies (ExpansionPolicy, else the compiled
	// default). An explicit value must be positive and may only tighten the
	// run cap, never loosen it (enforced in Validate). Exceeding it is a
	// plan-attributable rejection → validation_failed → semantic retry.
	MaxAddedSteps int `json:"max_added_steps,omitempty"`
}

// AgentConfig configures an agent step (ADR-016, ticket 14.1): an llm-family
// step bound to a named role from the definition's `agents` section. The
// authored form carries the agent ref plus the task (prompt/messages) and any
// per-field overrides of the role's defaults. At run instantiation
// ResolveAgentStep merges the role's defaults under these overrides and
// materializes the fully-populated config onto the step's row, so at claim
// time llmConfigView projects this config onto an LLMConfig exactly as the
// planner projects a PlannerConfig — the whole llm pipeline is reused.
//
// Every field except Agent is an override: present on the step → used; absent
// → the role's default (or, for prompt/messages, the step is the sole source —
// a role has no prompt of its own). The step-level validation/context
// overrides live on the step envelope (Step.Validation / Step.Context), not
// here.
type AgentConfig struct {
	// Agent names the role in the definition's agents section. Required.
	Agent string `json:"agent,omitempty"`

	// System overrides the role's system prompt when set.
	System string `json:"system,omitempty"`

	// Model overrides the role's default model when set.
	Model string `json:"model,omitempty"`

	// Prompt is the task, as a single user message. Mutually exclusive with
	// Messages; the merged step requires exactly one of the two (a role
	// supplies no prompt of its own).
	Prompt string `json:"prompt,omitempty"`

	// Messages is the task as a conversation. Mutually exclusive with Prompt.
	Messages []LLMMessage `json:"messages,omitempty"`

	// MaxTokens overrides the role's completion bound when non-zero.
	MaxTokens int `json:"max_tokens,omitempty"`

	// Temperature overrides the role's sampling temperature when non-nil.
	Temperature *float64 `json:"temperature,omitempty"`

	// ModelFallbacks overrides the role's downgrade chain wholesale when
	// non-empty.
	ModelFallbacks []ModelFallback `json:"model_fallbacks,omitempty"`

	// OutputFormat overrides the role's structured-output policy when non-nil.
	OutputFormat *OutputFormat `json:"output_format,omitempty"`

	// Tools overrides the role's allowed toolset wholesale when non-nil (an
	// explicit empty list forbids all tools).
	Tools []string `json:"tools,omitempty"`

	// Role is the resolved role name, copied from the referenced AgentDef by
	// ResolveAgentStep (falling back to the agent ref when the role declares no
	// Role) so the materialized step row is self-describing. It is not authored
	// on the step and is ignored by llmConfigView; the engine's auto-thread
	// append (ticket 14.2) reads it to tag each turn with its role.
	Role string `json:"role,omitempty"`
}

// ApprovalDecision names one decision a human (or the timeout policy) may
// record on a human_approval step (ADR-017). It is also the value carried
// by an outgoing edge's `decision` marker, selecting which edges fire when
// the step is decided under the `route` reject policy.
type ApprovalDecision string

// Permitted approval decisions.
const (
	ApprovalApprove ApprovalDecision = "approve"
	ApprovalReject  ApprovalDecision = "reject"
)

// approvalDecisions enumerates the valid ApprovalDecision values in
// declaration order for schema generation and error messages.
var approvalDecisions = []ApprovalDecision{ApprovalApprove, ApprovalReject}

// ApprovalTimeoutPolicy selects what happens when a human_approval step's
// `timeout` elapses with no decision (ADR-017). The decision the policy
// records goes through the same compare-and-swap as a human decision, so
// exactly one of human/timeout ever wins.
type ApprovalTimeoutPolicy string

// Permitted timeout policies.
const (
	// ApprovalTimeoutReject records a reject decision on expiry (the safe
	// default): the step routes exactly as a human reject would.
	ApprovalTimeoutReject ApprovalTimeoutPolicy = "reject"
	// ApprovalTimeoutApprove auto-approves the original (never edited)
	// payload on expiry. Deliberately available but documented as dangerous —
	// it lets a side effect fire with no human in the loop.
	ApprovalTimeoutApprove ApprovalTimeoutPolicy = "approve"
	// ApprovalTimeoutPark leaves the approval pending and parks the run
	// (park_reason = awaiting_human) as an escalation. The approval stays
	// decidable; unpark resumes dispatch of the run's other work.
	ApprovalTimeoutPark ApprovalTimeoutPolicy = "park"
)

// approvalTimeoutPolicies enumerates the valid ApprovalTimeoutPolicy values
// in declaration order for schema generation and error messages.
var approvalTimeoutPolicies = []ApprovalTimeoutPolicy{ApprovalTimeoutReject, ApprovalTimeoutApprove, ApprovalTimeoutPark}

// ApprovalRejectPolicy selects how a rejected human_approval step routes
// (ADR-017).
type ApprovalRejectPolicy string

// Permitted reject policies.
const (
	// ApprovalRejectFail dead-letters the step permanently (the default):
	// the run then follows its on_failure disposition. A requeue re-runs the
	// gate, producing a fresh pending approval.
	ApprovalRejectFail ApprovalRejectPolicy = "fail"
	// ApprovalRejectRoute makes the step succeed with a reject decision and
	// fires only its outgoing edges marked `decision: reject`; the approve
	// edges are skipped (ordinary skip propagation).
	ApprovalRejectRoute ApprovalRejectPolicy = "route"
)

// approvalRejectPolicies enumerates the valid ApprovalRejectPolicy values in
// declaration order for schema generation and error messages.
var approvalRejectPolicies = []ApprovalRejectPolicy{ApprovalRejectFail, ApprovalRejectRoute}

// HumanApprovalConfig configures a human_approval step: it parks the run —
// holding no lease or worker slot — until a human (or the timeout policy)
// decides, then resumes through the normal dispatch path (ADR-017,
// executor: ticket 15.2). Title/Description/Payload are ordinary templated
// config values (ticket 8.2): Payload is typically a whole-expression
// reference to the upstream step's output ("${{ steps.draft.output }}").
type HumanApprovalConfig struct {
	// Title is the short headline shown to the approver; required. It is
	// rendered (8.2 templating) before the approval is surfaced.
	Title string `json:"title,omitempty"`

	// Description is optional longer context, also rendered.
	Description string `json:"description,omitempty"`

	// Payload is the proposed action shown for review and — on approval —
	// carried into the step's output (edited in place when an edit is
	// supplied). Opaque JSON; typically a templated reference to an upstream
	// output. Absent means no structured payload.
	Payload json.RawMessage `json:"payload,omitempty"`

	// AllowedDecisions restricts which decisions the API accepts; empty means
	// the engine default [approve, reject]. Values must be distinct and drawn
	// from the ApprovalDecision enum.
	AllowedDecisions []ApprovalDecision `json:"allowed_decisions,omitempty"`

	// AllowEdit permits an approver to supply an edited_payload with an
	// approve decision. Requires `approve` to be an allowed decision.
	AllowEdit bool `json:"allow_edit,omitempty"`

	// EditSchema is an optional JSON Schema (2020-12, must be a JSON object)
	// constraining an edited payload; requires AllowEdit. Compiled at claim
	// pre-flight and enforced at decide time (the 11.3 output_format
	// precedent). Absent means any JSON edit is accepted.
	EditSchema json.RawMessage `json:"edit_schema,omitempty"`

	// Timeout is how long the step waits for a decision before OnTimeout
	// fires; a Go duration string, positive and at most MaxApprovalTimeout.
	// Empty means wait indefinitely (bounded only by the run's
	// max_wall_clock).
	Timeout string `json:"timeout,omitempty"`

	// OnTimeout is the policy applied when Timeout elapses; requires Timeout.
	// Empty defaults to reject when a Timeout is set. When OnTimeout records
	// a decision (reject/approve), that decision must be allowed.
	OnTimeout ApprovalTimeoutPolicy `json:"on_timeout,omitempty"`

	// OnReject routes a reject decision: fail (default, dead-letter) or route
	// (succeed and fire the reject edges). `route` requires `reject` to be an
	// allowed decision and at least one outgoing `decision: reject` edge.
	OnReject ApprovalRejectPolicy `json:"on_reject,omitempty"`
}

// JoinConfig configures a join step: a fan-in synchronization barrier.
type JoinConfig struct {
	Mode JoinMode `json:"mode,omitempty"`
}

// BranchConfig configures a branch step. The step is a pass-through whose
// output is its rendered Input (or its primary upstream output when Input
// is absent); the exclusive routing lives in the edge-firing rule on its
// outgoing edges (ADR-003).
type BranchConfig struct {
	Input json.RawMessage `json:"input,omitempty"`
}

// NoopConfig configures a noop test step, which takes no configuration; an
// empty object is tolerated so `"config": {}` decodes.
type NoopConfig struct{}

// EchoConfig configures an echo test step, which returns Input as output.
type EchoConfig struct {
	Input json.RawMessage `json:"input,omitempty"`
}

// SleepConfig configures a sleep test step (executor: M4), which waits for
// Duration and succeeds. Duration is a Go duration string ("1500ms",
// "2s"); presence and parseability are enforced in validation.
type SleepConfig struct {
	Duration string `json:"duration,omitempty"`
}

// FailNTimesConfig configures a fail_n_times test step (executor: M4),
// which fails attempts 1..N and succeeds from attempt N+1 on — its state
// is the attempt number, so behavior survives process restarts. Zero means
// the key was absent (invalid; enforced in validation, mirroring
// Edge.MaxIterations).
type FailNTimesConfig struct {
	N int `json:"n,omitempty"`
}

// CounterConfig configures a counter test step (executor: M4), which
// appends one line to the file at Path — an externally observable side
// effect. The crash and chaos suites count those lines to prove effects
// fired exactly once across kills, reclaims, and retries (M4.7, M5).
type CounterConfig struct {
	Path string `json:"path,omitempty"`
}

// EffectfulEchoConfig configures an effectful_echo test step (executor:
// ticket 5.5), which increments an external counter — one line appended to
// the file at Path — through the side-effect journal, then echoes Input as
// output. Unlike counter, the append is journaled: retries and reclaims
// short-circuit to the journaled result and never re-fire it. FailTimes
// (optional, default 0) makes attempts 1..N fail *after* journaling, so a
// retrying step proves the counter stays at exactly one line.
type EffectfulEchoConfig struct {
	Path      string          `json:"path,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	FailTimes int             `json:"fail_times,omitempty"`
}

// BlackboardWriteConfig configures a blackboard_write test step (executor:
// ticket 12.2), which writes Value to the run-scoped blackboard under Key
// (optionally under a version compare-and-swap guard), then echoes the
// written entry — plus, when ReadKey is set, the current head of that key —
// as its output. It exists so the engine integration tests exercise the
// programmatic StepContext.Blackboard read/write API and the CAS path
// without a real llm/tool executor.
type BlackboardWriteConfig struct {
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
	Tags  []string        `json:"tags,omitempty"`
	// ExpectedVersion, when non-nil, is the compare-and-swap guard: the write
	// requires the current head to equal it (0 = the key must not exist). Nil
	// writes unconditionally.
	ExpectedVersion *int `json:"expected_version,omitempty"`
	// ReadKey, when set, names a key whose head the step also reads and
	// includes in its output (the programmatic-read half of the API).
	ReadKey string `json:"read_key,omitempty"`
}

func (*LLMConfig) stepConfig()             {}
func (*ToolConfig) stepConfig()            {}
func (*RetrieveConfig) stepConfig()        {}
func (*MapConfig) stepConfig()             {}
func (*GatherConfig) stepConfig()          {}
func (*PlannerConfig) stepConfig()         {}
func (*AgentConfig) stepConfig()           {}
func (*HumanApprovalConfig) stepConfig()   {}
func (*JoinConfig) stepConfig()            {}
func (*BranchConfig) stepConfig()          {}
func (*NoopConfig) stepConfig()            {}
func (*EchoConfig) stepConfig()            {}
func (*SleepConfig) stepConfig()           {}
func (*FailNTimesConfig) stepConfig()      {}
func (*CounterConfig) stepConfig()         {}
func (*EffectfulEchoConfig) stepConfig()   {}
func (*BlackboardWriteConfig) stepConfig() {}
