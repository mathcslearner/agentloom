//go:build integration

package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
)

// bbSpawn wires an engine with the blackboard and the mock llm provider (so
// blackboard_write and llm steps both run) and joins it to the harness.
func bbSpawn(t *testing.T, s *store.Store, h *queuetest.Harness, d *engine.Dispatcher, id string) {
	t.Helper()
	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	reg := exec.Builtins(mockProviders(t), nil, nil)
	w, err := engine.New(s, reg, id,
		engine.WithDispatchNudge(d.Nudge),
		engine.WithBlackboard(board),
		// Wire the delayed-retry scheduler so a transient CAS conflict's
		// backoff re-dispatch is promoted by the harness's promoter duty.
		engine.WithRetryScheduler(h.Delayed()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn(id, w.Handle, queuetest.LeaseConfig(400*time.Millisecond))
}

func bbHead(t *testing.T, s *store.Store, runID uuid.UUID, key string) blackboard.Entry {
	t.Helper()
	row, err := s.Blackboard().Head(t.Context(), runID, key)
	if err != nil {
		t.Fatalf("Head(%q): %v", key, err)
	}
	return blackboard.Entry{
		Key: row.Key, Version: int(row.Version), Value: row.Value,
		TokenCount: int(row.TokenCount), TokenCounter: row.TokenCounter, Tags: row.Tags,
	}
}

// blackboardFlow fans out two distinct-key writes plus a declarative llm
// write, joins, then a CAS-success write that reads another key back.
const blackboardFlow = `{
	"schema_version": 1,
	"name": "blackboard-flow",
	"steps": [
		{"id": "start", "type": "noop"},
		{"id": "wa", "type": "blackboard_write", "config": {"key": "ka", "value": 1, "tags": ["writer"]}},
		{"id": "wb", "type": "blackboard_write", "config": {"key": "kb", "value": {"n": 2}}},
		{"id": "draft", "type": "llm", "config": {"model": "mock/sim-1", "prompt": "hello"}, "blackboard": {"write": [{"key": "draft_text", "from": "/text", "pinned": true}]}},
		{"id": "gather", "type": "join", "config": {"mode": "all"}},
		{"id": "capstone", "type": "blackboard_write", "config": {"key": "ka", "value": 99, "expected_version": 1, "read_key": "kb"}}
	],
	"edges": [
		{"from": "start", "to": "wa"},
		{"from": "start", "to": "wb"},
		{"from": "start", "to": "draft"},
		{"from": "wa", "to": "gather"},
		{"from": "wb", "to": "gather"},
		{"from": "draft", "to": "gather"},
		{"from": "gather", "to": "capstone"}
	]
}`

// TestBlackboardFlow: distinct-key parallel writes both land at v1, a
// declarative llm write publishes a pinned, model-counted entry, and a
// downstream CAS write with the correct expected version succeeds at v2.
func TestBlackboardFlow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, blackboardFlow)
	d := startDispatcher(t, s, h.Queue())
	bbSpawn(t, s, h, d, "worker-a")
	bbSpawn(t, s, h, d, "worker-b")

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// Distinct-key parallel writes: both at v1, no interference.
	ka := bbHead(t, s, runID, "ka")
	kb := bbHead(t, s, runID, "kb")
	if kb.Version != 1 {
		t.Fatalf("kb version = %d, want 1", kb.Version)
	}
	// ka was written v1 by wa, then v2 by the CAS capstone.
	if ka.Version != 2 || string(ka.Value) != "99" {
		t.Fatalf("ka head = v%d %s, want v2 99", ka.Version, ka.Value)
	}
	hist, err := s.Blackboard().History(ctx, runID, "ka")
	if err != nil || len(hist) != 2 {
		t.Fatalf("ka history = %d versions (%v), want 2", len(hist), err)
	}

	// The declarative llm write: pinned, counted with a real (non-empty)
	// counter fingerprint, value is the completion text.
	draft := bbHead(t, s, runID, "draft_text")
	if !draft.Pinned() {
		t.Fatalf("draft_text not pinned: tags %v", draft.Tags)
	}
	if draft.TokenCounter == "" || draft.TokenCount <= 0 {
		t.Fatalf("draft_text not token-counted: count=%d counter=%q", draft.TokenCount, draft.TokenCounter)
	}
	if string(draft.Value) == "" || draft.Value[0] != '"' {
		t.Fatalf("draft_text value not a JSON string: %s", draft.Value)
	}
}

// blackboardSameKey has two independent entry steps writing the SAME key
// unconditionally — the parallel same-key case.
const blackboardSameKey = `{
	"schema_version": 1,
	"name": "blackboard-same-key",
	"steps": [
		{"id": "one", "type": "blackboard_write", "config": {"key": "shared", "value": {"from": "one"}}},
		{"id": "two", "type": "blackboard_write", "config": {"key": "shared", "value": {"from": "two"}}}
	],
	"edges": []
}`

// TestBlackboardParallelSameKey: two parallel unconditional writes to one key
// both land — history has exactly two versions, no lost update — per ADR-014.
func TestBlackboardParallelSameKey(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, blackboardSameKey)
	d := startDispatcher(t, s, h.Queue())
	bbSpawn(t, s, h, d, "worker-a")
	bbSpawn(t, s, h, d, "worker-b")

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	hist, err := s.Blackboard().History(ctx, runID, "shared")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 2 || hist[0].Version != 1 || hist[1].Version != 2 {
		t.Fatalf("shared history = %d versions, want exactly 2 (v1, v2)", len(hist))
	}
	// Both writers' values survive (no lost update); order is nondeterministic.
	seen := map[string]bool{}
	for _, e := range hist {
		var v struct {
			From string `json:"from"`
		}
		_ = json.Unmarshal(e.Value, &v)
		seen[v.From] = true
	}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("both writers' values must survive: %v", seen)
	}
}

// TestBlackboardCASConflictDeadLetters: a write whose expected_version can
// never match dead-letters after its transient budget — the conflict is
// honored end-to-end.
func TestBlackboardCASConflictDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// expected_version 5 on a fresh key (head 0) conflicts forever.
	def := `{
		"schema_version": 1,
		"name": "blackboard-cas-conflict",
		"steps": [
			{"id": "bad", "type": "blackboard_write", "config": {"key": "k", "value": 1, "expected_version": 5}, "retry": {"max_attempts": 2, "backoff": {"initial": "1ms", "cap": "2ms", "multiplier": 1}}}
		],
		"edges": []
	}`
	s, h, runID := setup(t, def)
	d := startDispatcher(t, s, h.Queue())
	bbSpawn(t, s, h, d, "worker-a")

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	requireStepStatuses(t, s, runID, map[string]string{"bad": store.StepStatusDeadLettered})
	// Nothing was written — every attempt conflicted.
	if _, err := s.Blackboard().Head(ctx, runID, "k"); err == nil {
		t.Fatal("key k should not exist — every CAS attempt conflicted")
	}
}
