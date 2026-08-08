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
