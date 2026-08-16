package exec

import (
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// plannerVersion is the production planner executor's plugin version
// (ADR-009). Distinct plugin identity from the llm executor (same version
// string, different name) so a planner and an llm step never share a
// response-cache entry (ticket 13.3).
const plannerVersion = "1.0.0"

// PlannerExecutor runs planner steps (ticket 13.3, ADR-015): a model call
// whose validated output — a dag.PlanOutput document shaped into the
// completion's output.json — is applied to the running graph atomically with
// the step's completion (store.ExpandRun, composed in the engine's completion
// transaction). It is an llm-family executor: the model call, request framing,
// routing, response-cache, rate limiting, cost estimate, semantic-retry
// feedback, context assembly, and window guard are all the LLMExecutor's,
// reused verbatim by embedding — llmConfigView projects the PlannerConfig onto
// an LLMConfig carrying the implicit plan output_format, so a planner is an llm
// call whose output happens to be a plan. Only the plugin identity (type,
// manifest) differs, and the engine layers the plan validation + graph
// mutation on top of the ordinary llm completion.
type PlannerExecutor struct {
	LLMExecutor
}

// NewPlannerExecutor builds the planner executor over a provider registry,
// exactly like NewLLMExecutor (a nil/empty registry is valid — a planner step
// then fails permanent at resolve time).
func NewPlannerExecutor(providers *llm.Registry) PlannerExecutor {
	return PlannerExecutor{LLMExecutor: NewLLMExecutor(providers)}
}

// Type implements Executor: the planner step type. Shadows the embedded
// LLMExecutor.Type so the registry keys this executor under "planner".
func (PlannerExecutor) Type() string { return string(dag.StepPlanner) }

// PluginManifest implements SelfDescribing (ADR-009). Cacheable and
// cost-bearing, exactly like the llm executor it embeds: a plan is a
// non-deterministic completion the response cache may reuse (keyed on
// temperature==0), and its provider call is what M10 meters.
func (PlannerExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepPlanner, plannerVersion,
		"A model call whose validated output (a PlanOutput) injects steps and edges into the running graph (ADR-015).",
		plugin.Capabilities{Cacheable: true, CostBearing: true})
}
