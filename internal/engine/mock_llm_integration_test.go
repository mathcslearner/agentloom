//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 8.5's e2e: the mock provider, addressed as model "mock/sim-1"
// through the 8.4 registry's namespace form, drives two chained llm steps
// that pass data between them via 8.2 templating — the converted M4
// linear chain (examples/definitions/mock_pipeline.json), reproducibly
// and with zero network.
//
// The real llm step executor is 8.6's; until it lands, this suite uses a
// minimal in-test executor (mockLLMExecutor) that does exactly what 8.6's
// production executor will do in miniature — resolve the model to a
// provider and call Chat — so the mock is proven registered-like-any-
// provider and deterministic end-to-end. 8.6 replaces this shim with the
// production executor and supersedes this test with its own compose e2e.

// mockLLMExecutor is the stand-in llm executor for the 8.5 e2e: it decodes
// the (already template-rendered) LLMConfig, routes the model through an
// llm.Registry, calls Chat, and writes {model, text, usage} so downstream
// steps can template on `.output.text`.
type mockLLMExecutor struct {
	reg *llm.Registry
}

func (mockLLMExecutor) Type() string { return string(dag.StepLLM) }

func (e mockLLMExecutor) Execute(ctx context.Context, sc exec.StepContext) (exec.Output, error) {
	cfg, err := dag.DecodeStepConfig(sc.StepType, sc.Config)
	if err != nil {
		return exec.Output{}, err
	}
	c := cfg.(*dag.LLMConfig)

	provider, model, err := e.reg.Resolve("", c.Model)
	if err != nil {
		return exec.Output{}, err
	}
	prompt := c.Prompt
	if prompt == "" && len(c.Messages) > 0 {
		prompt = c.Messages[len(c.Messages)-1].Content
	}
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 256
	}
	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  []llm.Message{llm.UserText(prompt)},
	})
	if err != nil {
		return exec.Output{}, err
	}
	data, err := json.Marshal(struct {
		Model string    `json:"model"`
		Text  string    `json:"text"`
		Usage llm.Usage `json:"usage"`
	}{resp.Model, resp.Text(), resp.Usage})
	if err != nil {
		return exec.Output{}, err
	}
	return exec.Output{Data: data}, nil
}

// mockLLMRegistry builds an exec registry with the mock-backed llm shim
// plus the echo executor the fixture's record step needs.
func mockLLMRegistry(t *testing.T) *exec.Registry {
	t.Helper()
	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	reg, err := exec.NewRegistry(mockLLMExecutor{reg: providers}, exec.EchoExecutor{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// TestMockLLMPipelineCompletes is the ticket 8.5 headline: the canonical
// mock_pipeline.json fixture runs to success on the mock provider, and the
// recorded outputs prove data flowed from the first llm step into the
// second via templating and on into the echo step.
func TestMockLLMPipelineCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "mock_pipeline.json"))
	if err != nil {
		t.Fatalf("reading mock_pipeline.json: %v", err)
	}
	s, h, runID := setupWithParams(t, string(defJSON), json.RawMessage(`{"topic": "turtles"}`))
	d := startDispatcher(t, s, h.Queue())

	reg := mockLLMRegistry(t)
	spawn := func(id string) {
		w, werr := engine.New(s, reg, id, engine.WithDispatchNudge(d.Nudge))
		if werr != nil {
			t.Fatalf("engine.New: %v", werr)
		}
		h.Spawn(id, w.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	}
	spawn("worker-a")
	spawn("worker-b")

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"draft":  store.StepStatusSucceeded,
		"refine": store.StepStatusSucceeded,
		"record": store.StepStatusSucceeded,
	})
	requireSingleAttempts(t, s, runID, []string{"draft", "refine", "record"})

	// The mock's default echo makes the chain fully deterministic: the
	// draft step echoes the param-rendered prompt, the refine step echoes a
	// prompt that embeds the draft's text, and the echo step captures it.
	draftText := "[mock] Draft a note about turtles."
	refineText := "[mock] Improve this draft: " + draftText
	requireLLMOutputText(t, s, runID, "draft", draftText)
	requireLLMOutputText(t, s, runID, "refine", refineText)
	requireStepOutput(t, s, runID, "record", `{"final": `+jsonString(refineText)+`}`)

	// Usage is present on every llm output (the mandatory-usage contract),
	// so 8.6/M10 can persist it.
	requirePositiveUsage(t, s, runID, "draft")
	requirePositiveUsage(t, s, runID, "refine")
}

// requireLLMOutputText asserts an llm step's output text equals want.
func requireLLMOutputText(t *testing.T, s *store.Store, runID uuid.UUID, stepID, want string) {
	t.Helper()
	var out struct {
		Text string `json:"text"`
	}
	unmarshalStepOutput(t, s, runID, stepID, &out)
	if out.Text != want {
		t.Errorf("step %q text = %q, want %q", stepID, out.Text, want)
	}
}

// requirePositiveUsage asserts an llm step recorded positive token usage.
func requirePositiveUsage(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) {
	t.Helper()
	var out struct {
		Usage llm.Usage `json:"usage"`
	}
	unmarshalStepOutput(t, s, runID, stepID, &out)
	if out.Usage.InputTokens <= 0 || out.Usage.OutputTokens <= 0 {
		t.Errorf("step %q usage = %+v, want positive token counts", stepID, out.Usage)
	}
}

// unmarshalStepOutput reads a step's stored output into dst.
func unmarshalStepOutput(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, dst any) {
	t.Helper()
	step, err := s.Steps().Get(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("reading step %q: %v", stepID, err)
	}
	if err := json.Unmarshal(step.Output, dst); err != nil {
		t.Fatalf("unmarshaling step %q output %s: %v", stepID, step.Output, err)
	}
}

// jsonString renders s as a JSON string literal for inline expectations.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
