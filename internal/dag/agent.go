package dag

import "fmt"

// This file is the agent-role merge (ADR-016, ticket 14.1): the pure,
// deterministic function that resolves an `agent` step against its role in the
// definition's `agents` section into a fully-configured step. Validation runs
// it to check the merged model-call, and run instantiation runs it to
// materialize the resolved step's rows, so the executor and the engine's
// validate/context stages never re-read the agents section — an agent step is
// an ordinary llm-family step by the time it is claimed.

// ResolveAgentStep merges an `agent` step's overrides over its role's defaults
// (ADR-016) and returns the effective step. It is pure and deterministic:
// override precedence is per-field — a value present on the step wins,
// otherwise the role's default applies. The task text (prompt/messages) always
// comes from the step (a role has no prompt of its own); the config-level
// defaults (system, model, fallbacks, tools, max_tokens, temperature,
// output_format) come from the role unless the step overrides them; and the
// two envelope-level defaults (validation, context) are replaced wholesale by
// a step-level block when present (block-level override, not a deep merge —
// deterministic and simple). Retry/timeout/cache/budget/blackboard are
// step-level only and pass through unchanged.
//
// The returned step's config is a fully-populated *AgentConfig (so
// llmConfigView can project it onto an LLMConfig, exactly as the planner
// projects a PlannerConfig), and its envelope carries the merged
// validation/context so the ordinary materialization + engine stages apply the
// role's defaults with no agent-specific paths.
//
// It errors only when the step is not a resolvable agent step: a non-agent
// type, a missing/typed-wrong config, an empty agent ref, or a ref that names
// no role. Validate reports all of these as issues first, so instantiation
// (which runs on validated definitions) never expects an error — but the
// function is defensive.
func ResolveAgentStep(def *Definition, step Step) (Step, error) {
	if step.Type != StepAgent {
		return Step{}, fmt.Errorf("dag: ResolveAgentStep: step %q is type %q, not %q", step.ID, step.Type, StepAgent)
	}
	sc, ok := step.Config.(*AgentConfig)
	if !ok || sc == nil {
		return Step{}, fmt.Errorf("dag: ResolveAgentStep: step %q has no agent config", step.ID)
	}
	if sc.Agent == "" {
		return Step{}, fmt.Errorf("dag: ResolveAgentStep: step %q has an empty agent ref", step.ID)
	}
	role, declared := def.Agents[sc.Agent]
	if !declared || role == nil {
		return Step{}, fmt.Errorf("dag: ResolveAgentStep: step %q references undeclared agent %q", step.ID, sc.Agent)
	}

	merged := *sc // copy the authored overrides, then fill defaults from the role

	if merged.System == "" {
		merged.System = role.System
	}
	if merged.Model == "" {
		merged.Model = role.Model
	}
	if merged.MaxTokens == 0 {
		merged.MaxTokens = role.MaxTokens
	}
	if merged.Temperature == nil {
		merged.Temperature = role.Temperature
	}
	if len(merged.ModelFallbacks) == 0 {
		merged.ModelFallbacks = role.ModelFallbacks
	}
	if merged.OutputFormat == nil {
		merged.OutputFormat = role.OutputFormat
	}
	// Tools: a nil step toolset inherits the role's; an explicit list (even
	// empty) overrides. Prompt/Messages are never taken from the role.
	if merged.Tools == nil {
		merged.Tools = role.Tools
	}
	// Role is not authored on the step — it is the role's name, copied here so
	// the materialized row is self-describing (ticket 14.2's auto-thread append
	// reads it). A role with no explicit Role uses the agent ref (the map key).
	merged.Role = role.Role
	if merged.Role == "" {
		merged.Role = sc.Agent
	}

	out := step
	out.Config = &merged
	if out.Validation == nil {
		out.Validation = role.Validation
	}
	if out.Context == nil {
		out.Context = role.Context
	}
	return out, nil
}
