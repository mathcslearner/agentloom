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
	StepLLM:           func() StepConfig { return &LLMConfig{} },
	StepTool:          func() StepConfig { return &ToolConfig{} },
	StepRetrieve:      func() StepConfig { return &RetrieveConfig{} },
	StepMap:           func() StepConfig { return &MapConfig{} },
	StepPlanner:       func() StepConfig { return &PlannerConfig{} },
	StepAgent:         func() StepConfig { return &AgentConfig{} },
	StepHumanApproval: func() StepConfig { return &HumanApprovalConfig{} },
	StepJoin:          func() StepConfig { return &JoinConfig{} },
	StepBranch:        func() StepConfig { return &BranchConfig{} },
	StepNoop:          func() StepConfig { return &NoopConfig{} },
	StepEcho:          func() StepConfig { return &EchoConfig{} },
	StepSleep:         func() StepConfig { return &SleepConfig{} },
	StepFailNTimes:    func() StepConfig { return &FailNTimesConfig{} },
	StepCounter:       func() StepConfig { return &CounterConfig{} },
	StepEffectfulEcho: func() StepConfig { return &EffectfulEchoConfig{} },
}

// LLMMessage is one entry of an llm step's messages array.
type LLMMessage struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// LLMConfig configures an llm step: one model call (executor: M8).
// Requires model and exactly one of prompt or messages (enforced in 1.3).
type LLMConfig struct {
	Model     string       `json:"model,omitempty"`
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
// (executor: M13, ADR-015; provisional shape).
type MapConfig struct {
	Items string `json:"items,omitempty"`
	Body  string `json:"body,omitempty"`
}

// PlannerConfig configures a planner step: an LLM call whose validated
// output injects steps into the running graph (executor: M13, ADR-015;
// provisional shape — M1 requires the same keys as llm).
type PlannerConfig struct {
	Model       string       `json:"model,omitempty"`
	Prompt      string       `json:"prompt,omitempty"`
	Messages    []LLMMessage `json:"messages,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
}

// AgentConfig configures an agent step: an LLM step bound to a named role
// from the agents section (executor: M14, ADR-016; provisional shape).
type AgentConfig struct {
	Agent  string `json:"agent,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

// HumanApprovalConfig configures a human_approval step: parks the run
// until a human decides (executor: M15; provisional shape).
type HumanApprovalConfig struct {
	Prompt string `json:"prompt,omitempty"`
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

func (*LLMConfig) stepConfig()           {}
func (*ToolConfig) stepConfig()          {}
func (*RetrieveConfig) stepConfig()      {}
func (*MapConfig) stepConfig()           {}
func (*PlannerConfig) stepConfig()       {}
func (*AgentConfig) stepConfig()         {}
func (*HumanApprovalConfig) stepConfig() {}
func (*JoinConfig) stepConfig()          {}
func (*BranchConfig) stepConfig()        {}
func (*NoopConfig) stepConfig()          {}
func (*EchoConfig) stepConfig()          {}
func (*SleepConfig) stepConfig()         {}
func (*FailNTimesConfig) stepConfig()    {}
func (*CounterConfig) stepConfig()       {}
func (*EffectfulEchoConfig) stepConfig() {}
