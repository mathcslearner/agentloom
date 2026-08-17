//go:build integration

package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestAgentThreadRelay is the ticket 14.2 headline: a two-agent relay over the
// blackboard handoff thread. The researcher agent's turn is auto-appended to
// the run thread; the writer agent's role carries a "conversation view" context
// preset (a `thread` source), so the researcher's findings reach the writer
// automatically — the writer's task never names the topic, yet its output
// carries it (the mock echoes the assembled context). The thread entries carry
// author/role/iteration metadata.
func TestAgentThreadRelay(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "agent_handoff.json"))
	if err != nil {
		t.Fatalf("reading agent_handoff.json: %v", err)
	}
	s, h, runID := setupWithParams(t, string(defJSON), json.RawMessage(`{"topic": "sea turtles"}`))
	d := startDispatcher(t, s, h.Queue())

	// cxSpawn wires the blackboard + agent executor + (empty) retriever registry
	// so the thread auto-append (write side) and the `thread` context source
	// (read side) both run.
	cxSpawn(t, s, h, d, "worker-a")
	cxSpawn(t, s, h, d, "worker-b")

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{
		"research": store.StepStatusSucceeded,
		"write":    store.StepStatusSucceeded,
	})

	// The relay: the topic reaches the writer's output ONLY via the thread — the
	// writer's own task ("Write a short article from the research conversation
	// above.") never names it, so a writer output carrying "sea turtles" proves
	// the researcher's turn flowed through the thread context preset.
	writeOut := llmOutputText(t, s, runID, "write")
	if !strings.Contains(writeOut, "sea turtles") {
		t.Errorf("writer output does not carry the researcher's finding via the thread:\n%s", writeOut)
	}

	// The thread carries the researcher's turn with author/role/iteration.
	hist, err := s.Blackboard().History(ctx, runID, "thread")
	if err != nil {
		t.Fatalf("thread History: %v", err)
	}
	if len(hist) == 0 {
		t.Fatal("thread is empty; the agent turn was not auto-appended")
	}
	found := false
	for _, e := range hist {
		var msg blackboardThreadMessage
		if uerr := json.Unmarshal(e.Value, &msg); uerr != nil {
			t.Fatalf("thread entry %d value is not a ThreadMessage: %v", e.Version, uerr)
		}
		if msg.Author != "research" {
			continue
		}
		found = true
		if msg.Role != "researcher" {
			t.Errorf("research turn role = %q, want researcher", msg.Role)
		}
		if msg.Iteration != 0 {
			t.Errorf("research turn iteration = %d, want 0 (authored step)", msg.Iteration)
		}
		if len(msg.Content) == 0 {
			t.Error("research turn has no content")
		}
		// The entry row also carries the step attribution and the thread tag.
		if e.AuthorStepID == nil || *e.AuthorStepID != "research" {
			t.Errorf("thread entry author_step_id = %v, want research", e.AuthorStepID)
		}
		if !hasTag(e.Tags, "thread") {
			t.Errorf("thread entry tags = %v, want the thread tag", e.Tags)
		}
	}
	if !found {
		t.Error("no thread turn authored by the research step")
	}
}

// llmOutputText returns an llm-family step's output text.
func llmOutputText(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) string {
	t.Helper()
	step, err := s.Steps().Get(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("reading step %q: %v", stepID, err)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(step.Output, &out); err != nil {
		t.Fatalf("decoding step %q output: %v", stepID, err)
	}
	return out.Text
}

// blackboardThreadMessage mirrors blackboard.ThreadMessage for read-side asserts.
type blackboardThreadMessage struct {
	Author    string          `json:"author"`
	Role      string          `json:"role"`
	Iteration int             `json:"iteration"`
	Content   json.RawMessage `json:"content"`
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
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
