//go:build integration

package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Ticket 8.6's headline e2e: two chained `llm` steps run through the
// PRODUCTION llm executor (exec.LLMExecutor, registered in the builtin
// registry) against the offline mock provider, addressed as model
// "mock/sim-1" via the 8.4 registry namespace form. Data passes between
// the steps through 8.2 templating on the canonical mock_pipeline.json
// fixture, and the provider's token usage is persisted on each attempt
// row (feeding M10's cost ledger) — the 8.6 acceptance criteria realized
// end-to-end with zero network. (8.5's in-test executor shim is gone;
// this suite drives the real executor.)

// mockProviders builds a mock-only provider registry, the offline default
// for llm-step integration tests — the same wiring cmd/worker does from
// AGENTLOOM_LLM_MOCK_ENABLED.
func mockProviders(t *testing.T) *llm.Registry {
	t.Helper()
	reg, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	return reg
}

// TestMockLLMPipelineCompletes is the ticket 8.6 headline: mock_pipeline.json
// runs to success through the production llm executor on the mock provider,
// the recorded outputs prove data flowed step-to-step via templating, and
// usage is stamped on each llm attempt row.
func TestMockLLMPipelineCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "mock_pipeline.json"))
	if err != nil {
		t.Fatalf("reading mock_pipeline.json: %v", err)
	}
	s, h, runID := setupWithParams(t, string(defJSON), json.RawMessage(`{"topic": "turtles"}`))
	d := startDispatcher(t, s, h.Queue())

	reg, err := exec.NewRegistry(exec.NewLLMExecutor(mockProviders(t)), exec.EchoExecutor{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
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
	// prompt that embeds the draft's text (through `.output.text`), and the
	// echo step captures it.
	draftText := "[mock] Draft a note about turtles."
	refineText := "[mock] Improve this draft: " + draftText
	requireLLMOutputText(t, s, runID, "draft", draftText)
	requireLLMOutputText(t, s, runID, "refine", refineText)
	requireStepOutput(t, s, runID, "record", `{"final": `+jsonString(refineText)+`}`)

	// Usage is present on every llm output (the mandatory-usage contract)
	// AND persisted on the attempt row — the durable per-try record M10's
	// cost ledger meters and the run-status API surfaces.
	requirePositiveOutputUsage(t, s, runID, "draft")
	requirePositiveOutputUsage(t, s, runID, "refine")
	requireAttemptUsage(t, s, runID, "draft")
	requireAttemptUsage(t, s, runID, "refine")
	// A non-llm step records no usage on its attempt.
	requireNoAttemptUsage(t, s, runID, "record")
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

// requirePositiveOutputUsage asserts an llm step embedded positive token
// usage in its output payload.
func requirePositiveOutputUsage(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) {
	t.Helper()
	var out struct {
		Usage exec.Usage `json:"usage"`
	}
	unmarshalStepOutput(t, s, runID, stepID, &out)
	if out.Usage.InputTokens <= 0 || out.Usage.OutputTokens <= 0 {
		t.Errorf("step %q output usage = %+v, want positive token counts", stepID, out.Usage)
	}
}

// requireAttemptUsage asserts the step's succeeded attempt row carries
// positive token usage — the durable place 8.6 persists it.
func requireAttemptUsage(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) {
	t.Helper()
	a := latestAttempt(t, s, runID, stepID)
	if len(a.Usage) == 0 {
		t.Fatalf("step %q attempt %d has no usage", stepID, a.AttemptNo)
	}
	var u exec.Usage
	if err := json.Unmarshal(a.Usage, &u); err != nil {
		t.Fatalf("unmarshaling step %q attempt usage %s: %v", stepID, a.Usage, err)
	}
	if u.InputTokens <= 0 || u.OutputTokens <= 0 {
		t.Errorf("step %q attempt usage = %+v, want positive token counts", stepID, u)
	}
}

// requireNoAttemptUsage asserts a non-llm step's attempt records no usage.
func requireNoAttemptUsage(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) {
	t.Helper()
	if a := latestAttempt(t, s, runID, stepID); len(a.Usage) != 0 {
		t.Errorf("step %q attempt %d recorded usage %s, want none", stepID, a.AttemptNo, a.Usage)
	}
}

// latestAttempt returns the step's highest-numbered attempt row.
func latestAttempt(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) gen.StepAttempt {
	t.Helper()
	atts, err := s.Attempts().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	var latest *gen.StepAttempt
	for i := range atts {
		if atts[i].StepID == stepID {
			latest = &atts[i]
		}
	}
	if latest == nil {
		t.Fatalf("step %q has no attempts", stepID)
	}
	return *latest
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
