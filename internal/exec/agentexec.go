package exec

import (
	"context"
	"encoding/json"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// agentVersion is the agent executor's plugin version (ADR-009). Distinct
// plugin identity from the llm and planner executors (same version string,
// different name) so an agent step never shares a response-cache entry with an
// llm or planner step (ticket 14.1).
const agentVersion = "1.0.0"

// AgentExecutor runs agent steps (ticket 14.1, ADR-016): an llm-family step
// bound to a named role from the definition's `agents` section. By the time a
// step is claimed its config is the fully-merged AgentConfig
// (ResolveAgentStep ran at instantiation), so the model call, request framing,
// routing, response-cache, rate limiting, cost estimate, semantic-retry
// feedback, context assembly, and window guard are all the embedded
// LLMExecutor's, reused verbatim via the llmConfigView agent projection —
// exactly like the planner. The one behavioral addition is the tool allowlist:
// a model response that names a tool outside the role's allowed toolset fails
// the step permanently (rejection-only enforcement; offering tools to the
// model and executing them is deferred past 14.1).
type AgentExecutor struct {
	LLMExecutor
}

// NewAgentExecutor builds the agent executor over a provider registry, exactly
// like NewLLMExecutor (a nil/empty registry is valid — an agent step then
// fails permanent at resolve time).
func NewAgentExecutor(providers *llm.Registry) AgentExecutor {
	return AgentExecutor{LLMExecutor: NewLLMExecutor(providers)}
}

// Type implements Executor: the agent step type. Shadows the embedded
// LLMExecutor.Type so the registry keys this executor under "agent".
func (AgentExecutor) Type() string { return string(dag.StepAgent) }

// PluginManifest implements SelfDescribing (ADR-009). Cacheable and
// cost-bearing, exactly like the llm executor it embeds.
func (AgentExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepAgent, agentVersion,
		"An llm step bound to a named agent role from the definition's agents section (ADR-016).",
		plugin.Capabilities{Cacheable: true, CostBearing: true})
}

// Execute runs the underlying llm completion, then enforces the role's tool
// allowlist over the model's response (ticket 14.1). The completion path is
// the embedded LLMExecutor's verbatim — a cache hit never reaches here (the
// engine's cache middleware short-circuits before Execute), so the allowlist
// is enforced on every fresh completion. A tool call outside the allowlist is
// a permanent failure: the config that produced it is deterministic, so no
// identical retry would pass, and the allowlist is a hard authoring boundary.
func (e AgentExecutor) Execute(ctx context.Context, sc StepContext) (Output, error) {
	out, err := e.LLMExecutor.Execute(ctx, sc)
	if err != nil {
		return out, err
	}
	if err := enforceToolAllowlist(sc, out.Data); err != nil {
		return Output{}, err
	}
	return out, nil
}

// enforceToolAllowlist fails permanently when the completion names a tool
// outside the agent's allowed toolset. It reads the allowlist off the merged
// AgentConfig and the tool calls off the persisted llm output. A config that
// cannot be decoded is corrupt materialized state — permanent, like any other
// config-decode miss on this path. An empty/absent allowlist forbids every
// tool (the role offered none), so any tool call is rejected; the tools are
// not executed regardless (14.1 rejection-only).
func enforceToolAllowlist(sc StepContext, data json.RawMessage) error {
	cfg, err := configAs[*dag.AgentConfig](sc)
	if err != nil {
		return Permanentf("decoding agent config: %w", err)
	}
	allowed := make(map[string]bool, len(cfgTools(cfg)))
	for _, t := range cfgTools(cfg) {
		allowed[t] = true
	}
	// The persisted output carries the model's tool calls (llmOutput.ToolCalls);
	// decode only that field.
	var shaped struct {
		ToolCalls []struct {
			Name string `json:"name"`
		} `json:"tool_calls"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &shaped); err != nil {
			// The output was just produced by this executor's marshaller, so a
			// decode failure is an internal invariant break — permanent.
			return Permanentf("decoding agent output for tool-allowlist check: %w", err)
		}
	}
	for _, tc := range shaped.ToolCalls {
		if !allowed[tc.Name] {
			return Permanentf("agent called tool %q, which is not in its allowed toolset", tc.Name)
		}
	}
	return nil
}

// cfgTools returns the agent config's toolset, tolerating a nil config.
func cfgTools(cfg *dag.AgentConfig) []string {
	if cfg == nil {
		return nil
	}
	return cfg.Tools
}
