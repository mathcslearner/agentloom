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
	StepID             string `json:"step_id"`
	Attempt            int32  `json:"attempt"`
	CounterID          string `json:"counter_id"`
	ContextTokens      int    `json:"context_tokens"`
	PreflightTokens    int    `json:"preflight_tokens"`
	BudgetTokens       int    `json:"budget_tokens"`
	BudgetSource       string `json:"budget_source"`
	ContextWindow      int    `json:"context_window"`
	RawContextTokens   int    `json:"raw_context_tokens"`
	RawPreflightTokens int    `json:"raw_preflight_tokens"`
	Revisions          int    `json:"revisions"`
	Summaries          int    `json:"summaries"`
	Sources            []struct {
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

// contextRevisionPayload mirrors store.ContextRevisionEvent (ticket 12.4).
type contextRevisionPayload struct {
	StepID       string `json:"step_id"`
	Attempt      int32  `json:"attempt"`
	Index        int    `json:"index"`
	Strategy     string `json:"strategy"`
	Budget       int    `json:"budget"`
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
	Changed      bool   `json:"changed"`
	Error        string `json:"error"`
	Actions      []struct {
		SourceIndex  int    `json:"source_index"`
		Name         string `json:"name"`
		Action       string `json:"action"`
		TokensBefore int    `json:"tokens_before"`
		TokensAfter  int    `json:"tokens_after"`
	} `json:"actions"`
	Kept []string `json:"kept"`
}

// contextRevisions returns the context_revision events for a step, in seq order.
func contextRevisions(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) []contextRevisionPayload {
	t.Helper()
	events, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var out []contextRevisionPayload
	for _, ev := range events {
		if ev.Type != store.EventContextRevision {
			continue
		}
		var p contextRevisionPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("unmarshaling context_revision: %v", err)
		}
		if p.StepID == stepID {
			out = append(out, p)
		}
	}
	return out
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

// TestContextCompaction is the ticket 12.4 headline: the canonical
// context_compaction.json fixture runs offline on the mock; the summary step's
// assembled context exceeds its tight budget, so the pipeline (sliding_window →
// truncate_oldest → drop_lowest_priority) shrinks it below budget before the
// provider is called. The oldest turns are dropped, the pinned instruction is
// preserved, each strategy that ran is a context_revision event with
// non-increasing tokens, and the final pre-flight total is at or under budget
// and equals the attempt's reported input tokens exactly (mock).
func TestContextCompaction(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "context_compaction.json"))
	if err != nil {
		t.Fatalf("reading context_compaction.json: %v", err)
	}
	s, h, runID := setup(t, string(defJSON))
	d := startDispatcher(t, s, h.Queue())
	cxSpawn(t, s, h, d, "worker-a")
	cxSpawn(t, s, h, d, "worker-b")

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// The summary's echoed request carries the pinned instruction and the most
	// recent turn, but not the oldest turns the sliding window dropped.
	var summary struct {
		Text string `json:"text"`
	}
	unmarshalStepOutput(t, s, runID, "summary", &summary)
	for _, want := range []string{
		"You are the meeting scribe.", // pinned literal — never dropped
		"launch checklist",            // turn_5 — most recent, survives
	} {
		if !strings.Contains(summary.Text, want) {
			t.Errorf("summary output missing %q\nfull: %s", want, summary.Text)
		}
	}
	for _, dropped := range []string{
		"north-star metric",  // turn_1 — dropped by sliding_window
		"ingestion pipeline", // turn_2 — dropped by sliding_window
	} {
		if strings.Contains(summary.Text, dropped) {
			t.Errorf("summary output still contains dropped turn text %q", dropped)
		}
	}

	// The context_assembled event records the compaction: a budget, at least one
	// revision, a shrunk pre-flight total under budget, and the raw total above.
	ev := contextEvent(t, s, runID, "summary")
	if ev.BudgetTokens != 200 {
		t.Errorf("budget_tokens = %d, want 200", ev.BudgetTokens)
	}
	if ev.Revisions < 1 {
		t.Errorf("revisions = %d, want >= 1", ev.Revisions)
	}
	if ev.PreflightTokens > ev.BudgetTokens {
		t.Errorf("preflight %d over budget %d — compaction did not fit", ev.PreflightTokens, ev.BudgetTokens)
	}
	if ev.RawPreflightTokens <= ev.PreflightTokens {
		t.Errorf("raw preflight %d not greater than compacted %d — nothing was compacted", ev.RawPreflightTokens, ev.PreflightTokens)
	}
	droppedSources := 0
	for _, sr := range ev.Sources {
		if sr.Status == "dropped" {
			droppedSources++
		}
		if sr.Pinned && sr.Status != "included" {
			t.Errorf("pinned source %q disposition = %q, want included", sr.Name, sr.Status)
		}
	}
	if droppedSources < 2 {
		t.Errorf("dropped sources = %d, want >= 2 (turn_1, turn_2)", droppedSources)
	}

	// The pre-flight total equals the summary attempt's reported input tokens
	// exactly (mock counter mirrors the mock provider's estimator).
	a := latestAttempt(t, s, runID, "summary")
	var u exec.Usage
	if err := json.Unmarshal(a.Usage, &u); err != nil {
		t.Fatalf("unmarshaling summary usage: %v", err)
	}
	if int64(ev.PreflightTokens) != u.InputTokens {
		t.Errorf("preflight tokens %d != attempt input tokens %d", ev.PreflightTokens, u.InputTokens)
	}

	// The context_revision events are queryable per step attempt (the audit
	// contract): they name the summary step + its attempt, run in pipeline order,
	// start with sliding_window, and report non-increasing framed-request tokens.
	revs := contextRevisions(t, s, runID, "summary")
	if len(revs) != ev.Revisions {
		t.Fatalf("got %d context_revision events, assembled event says %d", len(revs), ev.Revisions)
	}
	if len(revs) == 0 {
		t.Fatal("no context_revision events recorded")
	}
	if revs[0].Strategy != "sliding_window" {
		t.Errorf("first revision strategy = %q, want sliding_window", revs[0].Strategy)
	}
	for i, r := range revs {
		if r.Attempt != a.AttemptNo {
			t.Errorf("revision %d attempt = %d, want %d", i, r.Attempt, a.AttemptNo)
		}
		if r.TokensAfter > r.TokensBefore {
			t.Errorf("revision %d (%s) tokens grew: %d -> %d", i, r.Strategy, r.TokensBefore, r.TokensAfter)
		}
		if r.Budget != 200 {
			t.Errorf("revision %d budget = %d, want 200", i, r.Budget)
		}
	}
	// The last revision brought the request at or under budget.
	if last := revs[len(revs)-1]; last.TokensAfter > 200 {
		t.Errorf("final revision left %d tokens, over budget 200", last.TokensAfter)
	}
}

// TestContextCompactionDeterministic: two runs of the compaction fixture on the
// same store state produce byte-identical summary output and identical revision
// counts — the deterministic-compaction guarantee.
func TestContextCompactionDeterministic(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "context_compaction.json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	run := func() (string, int) {
		s, h, runID := setup(t, string(defJSON))
		d := startDispatcher(t, s, h.Queue())
		cxSpawn(t, s, h, d, "w")
		waitRun(t, s, runID, store.RunStatusSucceeded)
		h.WaitQuiescent(ctx)
		var summary struct {
			Text string `json:"text"`
		}
		unmarshalStepOutput(t, s, runID, "summary", &summary)
		return summary.Text, contextEvent(t, s, runID, "summary").Revisions
	}
	t1, r1 := run()
	t2, r2 := run()
	if t1 != t2 {
		t.Errorf("compaction not deterministic:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", t1, t2)
	}
	if r1 != r2 {
		t.Errorf("revision count differs across runs: %d != %d", r1, r2)
	}
}

// overBudgetPinnedDef pins two large literals whose combined size exceeds the
// tiny budget, so no compaction can fit and the step fails permanently before
// any provider call.
const overBudgetPinnedDef = `{
	"schema_version": 1,
	"name": "context-over-budget",
	"steps": [
		{"id": "write", "type": "llm",
		 "config": {"model": "mock/sim-1", "prompt": "write a long paragraph about anything at all so this is not empty", "max_tokens": 128}},
		{"id": "summary", "type": "llm",
		 "config": {"model": "mock/sim-1", "prompt": "summarize", "max_tokens": 64},
		 "context": {
			"sources": [
				{"kind": "literal", "name": "big1", "text": "PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A PINNED-A", "pinned": true},
				{"kind": "literal", "name": "big2", "text": "PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B PINNED-B", "pinned": true}
			],
			"budget_tokens": 5,
			"compaction": [{"strategy": "drop_lowest_priority"}, {"strategy": "sliding_window", "n": 1}]
		 }}
	],
	"edges": [{"from": "write", "to": "summary"}]
}`

// TestContextOverBudgetPinnedDeadLetters: a pinned set that alone exceeds the
// budget cannot be compacted, so the step dead-letters permanently before any
// provider call — no context_assembled event, no usage, ADR-014's hard
// guarantee that overflow is never sent.
func TestContextOverBudgetPinnedDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setup(t, overBudgetPinnedDef)
	d := startDispatcher(t, s, h.Queue())
	cxSpawn(t, s, h, d, "worker-a")

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{
		"write":   store.StepStatusSucceeded,
		"summary": store.StepStatusDeadLettered,
	})
	if n := countEvents(t, s, runID, store.EventContextAssembled); n != 0 {
		t.Errorf("got %d context_assembled events, want 0 (compaction failed)", n)
	}
	a := latestAttempt(t, s, runID, "summary")
	if len(a.Usage) != 0 {
		t.Errorf("summary attempt recorded usage %s, want none (no provider call)", a.Usage)
	}
	if a.Outcome == nil || *a.Outcome != "permanent" {
		t.Errorf("summary attempt outcome = %v, want permanent", a.Outcome)
	}
}
