package exec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// stubVersion marks the dev stubs' manifests (ticket 8.1, ADR-009): a
// 0.x pre-release so a stubbed plugin is unmistakable in any listing.
// The capability flags, by contrast, describe the REAL semantics that
// replace each stub in place (8.3–8.8) — middleware written against the
// flags must behave correctly across that swap. The swap itself is the
// behavior change that mandates the bump to 1.0.0 (it must bust any M9
// cache entries keyed on the stub's outputs).
const stubVersion = "0.1.0-stub"

// Dev-stub executors for the AI-native step types (tool, retrieve).
//
// Ticket 4.6's acceptance runs the canonical example definitions
// end-to-end on the compose stack, but the real executors arrive with
// the tool/retrieval SPIs (tickets 8.7–8.8). Until then these stubs make
// the fixtures executable: each one succeeds immediately with a
// deterministic output derived only from the step's config, so runs are
// reproducible and no network or external state is ever touched. The
// `"stub": true` marker makes a stubbed output unmistakable in run state
// and demos. (The llm stub was replaced in place by the production
// executor in ticket 8.6 — see llmexec.go.)
//
// They are wired into Builtins() and will be replaced there — same types,
// real semantics — when their milestones land; nothing else may grow
// behavior on top of the stub outputs.

// StubToolExecutor runs tool steps as a dev stub: succeed with the
// configured input echoed back untouched.
type StubToolExecutor struct{}

// Type implements Executor.
func (StubToolExecutor) Type() string { return string(dag.StepTool) }

// PluginManifest implements SelfDescribing (ticket 8.1). Side-effectful:
// an unknown tool must be assumed to act on the world (specific built-in
// tools may declare otherwise in 8.7).
func (StubToolExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepTool, stubVersion,
		"One invocation through the tool SPI (dev stub until 8.7).",
		plugin.Capabilities{SideEffectful: true})
}

// Execute implements Executor.
func (StubToolExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.ToolConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || c.Tool == "" {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "tool")}
	}
	input := c.Input
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	return marshalOutput(struct {
		Stub  bool            `json:"stub"`
		Tool  string          `json:"tool"`
		Input json.RawMessage `json:"input"`
	}{true, c.Tool, input})
}

// StubRetrieveExecutor runs retrieve steps as a dev stub: succeed with an
// empty result list for the configured query.
type StubRetrieveExecutor struct{}

// Type implements Executor.
func (StubRetrieveExecutor) Type() string { return string(dag.StepRetrieve) }

// PluginManifest implements SelfDescribing (ticket 8.1).
func (StubRetrieveExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepRetrieve, stubVersion,
		"A retrieval query through the retriever SPI (dev stub until 8.8).",
		plugin.Capabilities{Cacheable: true})
}

// Execute implements Executor.
func (StubRetrieveExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.RetrieveConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || c.Retriever == "" {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "retriever")}
	}
	return marshalOutput(struct {
		Stub      bool     `json:"stub"`
		Retriever string   `json:"retriever"`
		Query     string   `json:"query"`
		Results   []string `json:"results"`
	}{true, c.Retriever, c.Query, []string{}})
}

// marshalOutput wraps json.Marshal for the stub output shapes above, which
// cannot fail on these plain structs but must not panic if they ever grow.
func marshalOutput(v any) (Output, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return Output{}, fmt.Errorf("marshaling stub output: %w", err)
	}
	return Output{Data: data}, nil
}
