//go:build integration

package engine_test

import (
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

// Ticket 14.1's headline e2e: agent_pipeline.json runs two `agent` steps
// through the PRODUCTION agent executor (exec.AgentExecutor) against the
// offline mock provider. Each agent inherits its role's model and system
// prompt from the definition's `agents` section (merged at instantiation), the
// writer reads the researcher's output through 8.2 templating, and the writer's
// step-level max_tokens override wins over the role default — the ADR-016
// "agent executes as a fully-configured LLM step" and "defaults merge
// deterministically" acceptance, end-to-end.

// TestAgentPipelineCompletes drives the two-agent relay to success and asserts
// the merge landed in the materialized rows and data flowed between agents.
func TestAgentPipelineCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "agent_pipeline.json"))
	if err != nil {
		t.Fatalf("reading agent_pipeline.json: %v", err)
	}
	s, h, runID := setupWithParams(t, string(defJSON), json.RawMessage(`{"topic": "turtles"}`))
	d := startDispatcher(t, s, h.Queue())

	reg, err := exec.NewRegistry(exec.NewAgentExecutor(mockProviders(t)))
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
		"research": store.StepStatusSucceeded,
		"write":    store.StepStatusSucceeded,
	})

	// Data flowed: the researcher echoes the param-rendered prompt, and the
	// writer echoes a prompt embedding the researcher's text (via .output.text).
	researchText := "[mock] Research this topic and list the key facts:\n\nturtles"
	writeText := "[mock] Write a short summary from these research findings:\n\n" + researchText
	requireLLMOutputText(t, s, runID, "research", researchText)
	requireLLMOutputText(t, s, runID, "write", writeText)

	// The merge landed in the materialized rows: both agents took the role's
	// model and system prompt, and the writer's step-level max_tokens override
	// beat the role default (256).
	research := materializedAgentConfig(t, s, runID, "research")
	if research.Model != "mock/sim-1" {
		t.Errorf("research model = %q, want the role's mock/sim-1", research.Model)
	}
	if research.System == "" {
		t.Error("research step did not inherit the researcher role's system prompt")
	}
	if research.MaxTokens != 256 {
		t.Errorf("research max_tokens = %d, want the role default 256", research.MaxTokens)
	}
	write := materializedAgentConfig(t, s, runID, "write")
	if write.MaxTokens != 512 {
		t.Errorf("write max_tokens = %d, want the step override 512", write.MaxTokens)
	}
	if write.System == "" {
		t.Error("write step did not inherit the writer role's system prompt")
	}

	// Both agent attempts metered a provider call (the cost ledger's input).
	requireAttemptUsage(t, s, runID, "research")
	requireAttemptUsage(t, s, runID, "write")
}

// TestAgentDisallowedToolDeadLetters proves the rejection-only tool allowlist
// end-to-end (ticket 14.1): an agent whose completion names a tool outside its
// allowed toolset fails permanently and lands in the DLQ — the run fails under
// the default fail-fast policy.
func TestAgentDisallowedToolDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const def = `{
		"schema_version": 1,
		"name": "agent-tool-reject",
		"agents": {
			"assistant": {
				"role": "assistant",
				"system": "you help",
				"model": "mock/sim-1",
				"tools": ["json_transform"]
			}
		},
		"steps": [
			{"id": "act", "type": "agent", "config": {"agent": "assistant", "prompt": "do the thing"}}
		],
		"edges": []
	}`
	s, h, runID := setup(t, def)
	d := startDispatcher(t, s, h.Queue())

	// A mock scripted to emit a tool_use for a tool the agent may NOT call.
	mock := &llm.MockConfig{Rules: []llm.MockRule{{
		Respond: []llm.MockOutcome{{
			StopReason: "tool_use",
			Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{
				ID: "tu_1", Name: "shell_exec", Input: json.RawMessage(`{"cmd":"rm -rf /"}`),
			}}},
			Usage: &llm.Usage{InputTokens: 4, OutputTokens: 2},
		}},
	}}}
	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: mock})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewAgentExecutor(providers))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	w, err := engine.New(s, reg, "worker", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker", w.Handle, queuetest.LeaseConfig(400*time.Millisecond))

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{"act": store.StepStatusDeadLettered})

	// The dead-letter records a permanent failure (a disallowed tool call can
	// never be fixed by an identical retry).
	dls, err := s.DeadLetters().ListByStep(ctx, runID, "act")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dls))
	}
	if dls[0].Source != store.DeadLetterSourcePermanent {
		t.Errorf("dead-letter source = %q, want permanent", dls[0].Source)
	}
}

// materializedAgentConfig decodes a run step's stored config as an AgentConfig —
// the fully-merged shape ResolveAgentStep wrote at instantiation.
func materializedAgentConfig(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) *dag.AgentConfig {
	t.Helper()
	step, err := s.Steps().Get(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("reading step %q: %v", stepID, err)
	}
	cfg, err := dag.DecodeStepConfig(dag.StepAgent, step.Config)
	if err != nil {
		t.Fatalf("decoding step %q config: %v", stepID, err)
	}
	ac, ok := cfg.(*dag.AgentConfig)
	if !ok {
		t.Fatalf("step %q config is %T, want *dag.AgentConfig", stepID, cfg)
	}
	return ac
}
