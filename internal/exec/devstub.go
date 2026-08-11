package exec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// Dev-stub executors for the AI-native step types (llm, tool, retrieve).
//
// Ticket 4.6's acceptance runs the canonical example definitions —
// fanout.json carries llm, tool, and retrieve steps — end-to-end on the
// compose stack, but the real executors arrive with the provider interface
// (M9) and the tool/retrieval SPIs (M8/M16). Until then these stubs make
// the fixtures executable: each one succeeds immediately with a
// deterministic output derived only from the step's config, so runs are
// reproducible and no network, provider key, or external state is ever
// touched. The `"stub": true` marker makes a stubbed output unmistakable
// in run state and demos.
//
// They are wired into Builtins() and will be replaced there — same types,
// real semantics — when their milestones land; nothing else may grow
// behavior on top of the stub outputs.

// StubLLMExecutor runs llm steps as a dev stub: echo the resolved prompt
// back as the completion text. Requires model and one of prompt/messages,
// mirroring validation (1.3) so hand-built configs fail loudly.
type StubLLMExecutor struct{}

// Type implements Executor.
func (StubLLMExecutor) Type() string { return string(dag.StepLLM) }

// Execute implements Executor.
func (StubLLMExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.LLMConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || c.Model == "" {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "model")}
	}
	prompt := c.Prompt
	if prompt == "" {
		if len(c.Messages) == 0 {
			return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("requires one of %q or %q", "prompt", "messages")}
		}
		prompt = c.Messages[len(c.Messages)-1].Content
	}
	return marshalOutput(struct {
		Stub  bool   `json:"stub"`
		Model string `json:"model"`
		Text  string `json:"text"`
	}{true, c.Model, "[stub llm output] " + prompt})
}

// StubToolExecutor runs tool steps as a dev stub: succeed with the
// configured input echoed back untouched.
type StubToolExecutor struct{}

// Type implements Executor.
func (StubToolExecutor) Type() string { return string(dag.StepTool) }

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
