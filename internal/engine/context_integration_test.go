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

	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
)

// cxSpawn wires an engine with the mock llm provider, the blackboard, and a
// pg_fulltext retriever registry (empty corpus by default) so context
// assembly's four source kinds all resolve.
func cxSpawn(t *testing.T, s *store.Store, h *queuetest.Harness, d *engine.Dispatcher, id string) {
	t.Helper()
	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	reg := exec.Builtins(mockProviders(t), nil, retrievers)
	w, err := engine.New(s, reg, id,
		engine.WithDispatchNudge(d.Nudge),
		engine.WithBlackboard(board),
		engine.WithRetrievers(retrievers),
		engine.WithRetryScheduler(h.Delayed()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn(id, w.Handle, queuetest.LeaseConfig(400*time.Millisecond))
}

// contextAssembledPayload mirrors store.ContextAssembledEvent for read-side
// assertions (the test package cannot see the store's unexported round-trip).
type contextAssembledPayload struct {
	StepID          string `json:"step_id"`
	Attempt         int32  `json:"attempt"`
	CounterID       string `json:"counter_id"`
	ContextTokens   int    `json:"context_tokens"`
	PreflightTokens int    `json:"preflight_tokens"`
	Sources         []struct {
		Index  int    `json:"index"`
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Ref    string `json:"ref"`
		Status string `json:"status"`
		Reason string `json:"reason"`
		Tokens int    `json:"tokens"`
		Pinned bool   `json:"pinned"`
	} `json:"sources"`
}

func contextEvent(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) contextAssembledPayload {
	t.Helper()
	events, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	for _, ev := range events {
		if ev.Type != store.EventContextAssembled {
			continue
		}
		var p contextAssembledPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("unmarshaling context_assembled: %v", err)
		}
		if p.StepID == stepID {
			return p
		}
	}
	t.Fatalf("no context_assembled event for step %q", stepID)
	return contextAssembledPayload{}
}

// TestContextAssembly is the ticket 12.3 headline: the canonical
// context_assembly.json fixture runs offline on the mock through the
// production pipeline; the assembled sources reach the provider (visible in the
// mock echo), the context_assembled event records each source's disposition,
// the retrieval source skips on an empty corpus, and the recorded pre-flight
// token total equals the mock's reported input tokens exactly (the ±5%
// accuracy criterion, exact here by construction).
func TestContextAssembly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "context_assembly.json"))
	if err != nil {
		t.Fatalf("reading context_assembly.json: %v", err)
	}
	s, h, runID := setup(t, string(defJSON))
	d := startDispatcher(t, s, h.Queue())
	cxSpawn(t, s, h, d, "worker-a")
	cxSpawn(t, s, h, d, "worker-b")

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// The draft step's echoed output carries the assembled context: the pinned
	// literal, the research step's output, and the blackboard findings, all
	// ahead of the original prompt.
	var draft struct {
		Text string `json:"text"`
	}
	unmarshalStepOutput(t, s, runID, "draft", &draft)
	researchText := "[mock] Research the topic and list the key findings."
	for _, want := range []string{
		"You are a careful writer.",    // literal source
		researchText,                   // step_output + blackboard sources
		`<context name="instructions"`, // rendered wrapper
		"Write a first draft grounded", // the original prompt, after the context
	} {
		if !strings.Contains(draft.Text, want) {
			t.Errorf("draft output missing %q\nfull: %s", want, draft.Text)
		}
	}

	// The context_assembled event records four sources: literal, step_output,
	// blackboard (all included) and retrieval (skipped — empty corpus).
	ev := contextEvent(t, s, runID, "draft")
	if len(ev.Sources) != 4 {
		t.Fatalf("context event has %d sources, want 4", len(ev.Sources))
	}
	wantStatus := []string{"included", "included", "included", "skipped"}
	for i, sr := range ev.Sources {
		if sr.Status != wantStatus[i] {
			t.Errorf("source %d (%s) status = %q, want %q", i, sr.Kind, sr.Status, wantStatus[i])
		}
	}
	if !ev.Sources[0].Pinned {
		t.Error("literal source should be recorded pinned")
	}
	if ev.ContextTokens <= 0 || ev.PreflightTokens <= 0 {
		t.Fatalf("context/preflight tokens not positive: %+v", ev)
	}

	// The pre-flight total equals the draft attempt's reported input tokens
	// exactly (mock counter mirrors the mock provider's estimator).
	a := latestAttempt(t, s, runID, "draft")
	var u exec.Usage
	if err := json.Unmarshal(a.Usage, &u); err != nil {
		t.Fatalf("unmarshaling draft usage: %v", err)
	}
	if int64(ev.PreflightTokens) != u.InputTokens {
		t.Errorf("preflight tokens %d != attempt input tokens %d", ev.PreflightTokens, u.InputTokens)
	}
}

// TestContextAssemblyDeterministic asserts two runs of the same fixture on the
// same store state produce byte-identical assembled output (the golden
// acceptance criterion), and the recorded context tokens match.
func TestContextAssemblyDeterministic(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "context_assembly.json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	run := func() (string, int) {
		s, h, runID := setup(t, string(defJSON))
		d := startDispatcher(t, s, h.Queue())
		cxSpawn(t, s, h, d, "w")
		waitRun(t, s, runID, store.RunStatusSucceeded)
		h.WaitQuiescent(ctx)
		var draft struct {
			Text string `json:"text"`
		}
		unmarshalStepOutput(t, s, runID, "draft", &draft)
		return draft.Text, contextEvent(t, s, runID, "draft").ContextTokens
	}
	t1, tok1 := run()
	t2, tok2 := run()
	if t1 != t2 {
		t.Errorf("assembly not deterministic:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", t1, t2)
	}
	if tok1 != tok2 {
		t.Errorf("context tokens differ across runs: %d != %d", tok1, tok2)
	}
}

// missingContextDef drives a draft step whose retrieval source uses the
// default (error) missing-policy against an empty corpus, so assembly fails
// permanently before the provider call.
const missingContextDef = `{
	"schema_version": 1,
	"name": "context-missing",
	"steps": [
		{"id": "draft", "type": "llm",
		 "config": {"model": "mock/sim-1", "prompt": "write", "max_tokens": 64},
		 "context": {"sources": [
			{"kind": "retrieval", "retriever": "pg_fulltext", "query": "nothing matches"}
		 ]}}
	],
	"edges": []
}`

// TestContextMissingSourceErrorDeadLetters: a required source that resolves to
// nothing fails the step permanently before any provider call — the step dead-
// letters (source permanent), with no recorded output usage.
func TestContextMissingSourceErrorDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setup(t, missingContextDef)
	d := startDispatcher(t, s, h.Queue())
	cxSpawn(t, s, h, d, "worker-a")

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{"draft": store.StepStatusDeadLettered})
	// No context_assembled event (assembly errored before recording), and no
	// usage recorded (the provider was never called).
	if n := countEvents(t, s, runID, store.EventContextAssembled); n != 0 {
		t.Errorf("got %d context_assembled events, want 0 (assembly failed)", n)
	}
	a := latestAttempt(t, s, runID, "draft")
	if len(a.Usage) != 0 {
		t.Errorf("draft attempt recorded usage %s, want none (no provider call)", a.Usage)
	}
	if a.Outcome == nil || *a.Outcome != "permanent" {
		t.Errorf("draft attempt outcome = %v, want permanent", a.Outcome)
	}
}
