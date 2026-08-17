//go:build integration

package engine_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 14.3 headline e2e (ADR-016): loop-edge unrolling. A marked loop edge is
// executed by cloning the loop body into fresh {id}#k instances through
// store.ExpandRun, chaining one iteration onto the last, with the critic's
// verdict threaded into each revision via the 14.2 blackboard thread. These
// tests prove the three acceptance criteria — writer⇄critic converges after two
// rejections (3 writer instances, feedback threaded); the iteration cap is
// enforced with a loop_exhausted event under both on_exhausted policies; and a
// crash mid-iteration (a failpoint after ExpandRun) rolls back with no duplicate
// iteration.

// loopReviseThenApprove scripts the critic to reject twice then approve (a
// per-rule sticky-last sequence, matched by the critic's user prompt), while the
// writer's calls fall through to the mock's default echo. Structured JSON so the
// loop condition output.json.verdict resolves.
func loopReviseThenApprove() *llm.MockConfig {
	return &llm.MockConfig{Rules: []llm.MockRule{{
		Substring: "Review the latest draft",
		Respond: []llm.MockOutcome{
			{Structured: json.RawMessage(`{"verdict":"revise","notes":["add detail"]}`)},
			{Structured: json.RawMessage(`{"verdict":"revise","notes":["tighten"]}`)},
			{Structured: json.RawMessage(`{"verdict":"approve"}`)},
		},
	}}}
}

// loopAlwaysRevise scripts the critic to reject on every turn — the runaway loop
// the iteration cap must halt.
func loopAlwaysRevise() *llm.MockConfig {
	return &llm.MockConfig{Rules: []llm.MockRule{{
		Substring: "Review the latest draft",
		Respond:   []llm.MockOutcome{{Structured: json.RawMessage(`{"verdict":"revise","notes":["more"]}`)}},
	}}}
}

// loopSpawn wires an engine with a scripted mock, the agent + echo executors,
// the blackboard (auto-thread + the writer's thread context source), and the
// json_schema validator (the critic's implicit output_format validator).
func loopSpawn(t *testing.T, s *store.Store, h *queuetest.Harness, d *engine.Dispatcher, id string, mc *llm.MockConfig) {
	t.Helper()
	mock, err := llm.NewMock(*mc)
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewAgentExecutor(providers), exec.EchoExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validate.NewJSONSchema())
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	w, err := engine.New(s, reg, id,
		engine.WithDispatchNudge(d.Nudge),
		engine.WithBlackboard(board),
		engine.WithValidators(vreg),
		engine.WithRetryScheduler(h.Delayed()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn(id, w.Handle, queuetest.LeaseConfig(400*time.Millisecond))
}

// loopExhaustedEvents returns the run's loop_exhausted event payloads in seq
// order.
func loopExhaustedEvents(t *testing.T, s *store.Store, runID uuid.UUID) []store.LoopExhaustedEvent {
	t.Helper()
	evs, err := s.Events().List(t.Context(), runID, 0, 500)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var out []store.LoopExhaustedEvent
	for _, ev := range evs {
		if ev.Type != store.EventLoopExhausted {
			continue
		}
		var le store.LoopExhaustedEvent
		if err := json.Unmarshal(ev.Payload, &le); err != nil {
			t.Fatalf("decoding loop_exhausted payload: %v", err)
		}
		out = append(out, le)
	}
	return out
}

func criticLoopDef(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "critic_loop.json"))
	if err != nil {
		t.Fatalf("reading critic_loop.json: %v", err)
	}
	return string(data)
}

// capLoopDef is a minimal writer⇄critic loop with a configurable iteration cap
// and on_exhausted policy, for the exhaustion tests. The critic uses
// output_format json so the loop condition output.json.verdict resolves; the
// exit edge is unconditioned (the before-splice keeps publish pending until the
// terminal iteration).
func capLoopDef(policy string, maxIter int) string {
	return `{
		"schema_version": 1,
		"name": "cap-loop",
		"agents": {
			"writer": {"role": "writer", "system": "you write", "model": "mock/sim-1", "max_tokens": 256},
			"critic": {"role": "critic", "system": "you review", "model": "mock/sim-1", "max_tokens": 256, "output_format": {"type": "json"}}
		},
		"params": {"brief": {"type": "string", "required": true}},
		"steps": [
			{"id": "draft", "type": "agent", "config": {"agent": "writer", "prompt": "write about ${{ run.params.brief }}"}},
			{"id": "critique", "type": "agent", "config": {"agent": "critic", "prompt": "Review the latest draft against the brief:\n\n${{ steps.draft.output.text }}"}},
			{"id": "publish", "type": "echo", "config": {"input": {"status": "published"}}}
		],
		"edges": [
			{"from": "draft", "to": "critique"},
			{"from": "critique", "to": "draft", "type": "loop", "condition": "output.json.verdict == 'revise'", "max_iterations": ` + strconv.Itoa(maxIter) + `, "on_exhausted": "` + policy + `"},
			{"from": "critique", "to": "publish"}
		]
	}`
}

// TestLoopWriterCriticConverges is criterion 1: the mock critic rejects twice
// then approves, so the loop unrolls two extra iterations — three writer
// instances total (draft, draft#1, draft#2) — and each revision's prompt carries
// the critic's threaded feedback. The run then succeeds through the exit edge.
func TestLoopWriterCriticConverges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setupWithParams(t, criticLoopDef(t), json.RawMessage(`{"brief": "write about turtles"}`))
	d := startDispatcher(t, s, h.Queue())
	loopSpawn(t, s, h, d, "loop-a", loopReviseThenApprove())
	loopSpawn(t, s, h, d, "loop-b", loopReviseThenApprove())

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	// Three writer instances and three critic instances ran; publish exited.
	requireStepStatuses(t, s, runID, map[string]string{
		"draft":      store.StepStatusSucceeded,
		"draft#1":    store.StepStatusSucceeded,
		"draft#2":    store.StepStatusSucceeded,
		"critique":   store.StepStatusSucceeded,
		"critique#1": store.StepStatusSucceeded,
		"critique#2": store.StepStatusSucceeded,
		"publish":    store.StepStatusSucceeded,
	})

	// Two expansions moved the graph to version 3; each injected instance carries
	// loop provenance (origin kind + depth).
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.GraphVersion != 3 {
		t.Errorf("graph_version = %d, want 3 (two loop iterations)", run.GraphVersion)
	}
	steps := requireSteps(t, s, runID)
	for _, id := range []string{"draft#1", "critique#1", "draft#2", "critique#2"} {
		st := steps[id]
		if st.OriginKind == nil || *st.OriginKind != string(dag.OriginLoop) {
			t.Errorf("step %q origin_kind = %v, want loop", id, st.OriginKind)
		}
		// Every iteration carries the same constant depth (1): loop iterations are
		// sequential, not nested, so they do not accumulate depth (ticket 14.3).
		if st.Depth != 1 {
			t.Errorf("step %q depth = %d, want 1 (loops do not nest depth)", id, st.Depth)
		}
	}

	// Feedback threaded: each revised draft's prompt (echoed back as its output)
	// carries the critic's prior verdict from the thread context source.
	for _, id := range []string{"draft#1", "draft#2"} {
		var out struct {
			Text string `json:"text"`
		}
		unmarshalStepOutput(t, s, runID, id, &out)
		if !strings.Contains(out.Text, "revise") {
			t.Errorf("draft %q prompt did not carry the critic's threaded feedback: %q", id, out.Text)
		}
	}
}

// TestLoopCapExhaustedProceed is criterion 2 (proceed): a critic that never
// approves is halted at max_iterations=2 with a loop_exhausted event, and the
// unconditioned exit edge routes to publish — the run still succeeds.
func TestLoopCapExhaustedProceed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setupWithParams(t, capLoopDef("proceed", 2), json.RawMessage(`{"brief": "b"}`))
	d := startDispatcher(t, s, h.Queue())
	loopSpawn(t, s, h, d, "loop-a", loopAlwaysRevise())

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{
		"draft":      store.StepStatusSucceeded,
		"draft#1":    store.StepStatusSucceeded,
		"draft#2":    store.StepStatusSucceeded,
		"critique":   store.StepStatusSucceeded,
		"critique#1": store.StepStatusSucceeded,
		"critique#2": store.StepStatusSucceeded,
		"publish":    store.StepStatusSucceeded,
	})
	evs := loopExhaustedEvents(t, s, runID)
	if len(evs) != 1 {
		t.Fatalf("loop_exhausted events = %d, want 1", len(evs))
	}
	le := evs[0]
	if le.LoopSourceStep != "critique" || le.Iteration != 2 || le.MaxIterations != 2 {
		t.Errorf("loop_exhausted = %+v, want source critique, iteration 2, max 2", le)
	}
	if le.Policy != "proceed" || le.Action != "proceed" {
		t.Errorf("loop_exhausted policy/action = %q/%q, want proceed/proceed", le.Policy, le.Action)
	}
}

// TestLoopCapExhaustedFail is criterion 2 (fail): the same runaway loop under
// on_exhausted=fail records a loop_exhausted event and fails the run permanently
// (the loop source dead-letters).
func TestLoopCapExhaustedFail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setupWithParams(t, capLoopDef("fail", 2), json.RawMessage(`{"brief": "b"}`))
	d := startDispatcher(t, s, h.Queue())
	loopSpawn(t, s, h, d, "loop-a", loopAlwaysRevise())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	evs := loopExhaustedEvents(t, s, runID)
	if len(evs) != 1 {
		t.Fatalf("loop_exhausted events = %d, want 1", len(evs))
	}
	if evs[0].Action != "fail" || evs[0].Policy != "fail" {
		t.Errorf("loop_exhausted policy/action = %q/%q, want fail/fail", evs[0].Policy, evs[0].Action)
	}
	// The exhausting critic instance dead-lettered; publish never ran.
	steps := requireSteps(t, s, runID)
	if st := steps["critique#2"]; st.Status != store.StepStatusDeadLettered {
		t.Errorf("critique#2 status = %q, want dead_lettered", st.Status)
	}
	if st, ok := steps["publish"]; ok && st.Status == store.StepStatusSucceeded {
		t.Error("publish succeeded — the run should have failed at the cap")
	}
}

// TestLoopExpansionAtomicOnFailpoint is criterion 3's atomicity core: aborting
// right after the loop's ExpandRun rolls the whole completion back — no injected
// iteration, graph unmoved, the loop source not succeeded — so a crash mid-
// iteration cannot leave a half-expanded or duplicated iteration (the fencing
// guarantee that resume completes the same iteration once, inherited from 13.3).
func TestLoopExpansionAtomicOnFailpoint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	mock, err := llm.NewMock(*loopReviseThenApprove())
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewAgentExecutor(providers), exec.EchoExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validate.NewJSONSchema())
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "loop-fp",
		engine.WithBlackboard(board), engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, criticLoopDef(t)), Params: json.RawMessage(`{"brief": "b"}`), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID

	// Run draft(0) to completion first (it does not expand), so the next claim is
	// critique(0) — the loop source whose completion expands. draft's fan-out
	// readies and outboxes critique.
	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("draft drain dispatched %d rows, want 1", n)
	}
	h.Enqueue(ctx, stepEnvelope(runID, "draft"))
	dmsg := h.ReadOne(ctx, "loop-fp")
	if err := eng.Handle(ctx, queue.Delivery{ID: dmsg.ID, Envelope: stepEnvelope(runID, "draft"), DeliveryCount: 1}); err != nil {
		t.Fatalf("handling draft(0): %v", err)
	}

	// Abort right after the loop's ExpandRun on critique's completion.
	engine.SetCompleteFailpoint(t, func(stage string) error {
		if stage == engine.StageAfterExpand {
			return errors.New("injected: abort after loop expand")
		}
		return nil
	})
	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("critique drain dispatched %d rows, want 1", n)
	}
	h.Enqueue(ctx, stepEnvelope(runID, "critique"))
	msg := h.ReadOne(ctx, "loop-fp")
	err = eng.Handle(ctx, queue.Delivery{ID: msg.ID, Envelope: stepEnvelope(runID, "critique"), DeliveryCount: 1})
	if err == nil {
		t.Fatal("Handle returned nil — the failpoint abort should have surfaced as a redeliver error")
	}

	// Nothing committed: no injected iteration, graph_version unchanged, the loop
	// source still running (its completion rolled back).
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.GraphVersion != 1 {
		t.Errorf("graph_version = %d, want 1 (rolled back)", run.GraphVersion)
	}
	steps := requireSteps(t, s, runID)
	if _, ok := steps["draft#1"]; ok {
		t.Error("draft#1 exists — a rolled-back loop expansion must leave no injected iteration")
	}
	if steps["critique"].Status == store.StepStatusSucceeded {
		t.Error("critique status = succeeded — the completion must have rolled back")
	}
}
