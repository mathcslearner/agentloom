//go:build integration

package engine_test

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 14.4 integration suite (ADR-016): run guards & termination policies.
// A runaway writer⇄critic loop (a critic that never approves) is halted by each
// guard class in isolation — an expansion cap (max_total_steps / max_expansions)
// dead-letters the loop source permanently with a guard_tripped event carrying
// which limit / current / cap; the wall-clock deadline cancels the run at the
// next claim. The opt-in no-progress detector forces an early exit when
// consecutive iterations produce identical output, and is disabled by default.

// guardCapLoopDef is a runaway writer⇄critic loop (the critic always revises)
// carrying a run-level expansion cap block, for the cap-guard tests. The writer
// has no context spec, so its prompt — and output — is identical every
// iteration; the loop is bounded only by the guard under test.
func guardCapLoopDef(expansionBlock string, maxIter int) string {
	return `{
		"schema_version": 1,
		"name": "guard-cap-loop",
		` + expansionBlock + `
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
			{"from": "critique", "to": "draft", "type": "loop", "condition": "output.json.verdict == 'revise'", "max_iterations": ` + strconv.Itoa(maxIter) + `},
			{"from": "critique", "to": "publish"}
		]
	}`
}

// noProgressLoopDef is a runaway writer⇄critic loop whose loop edge carries a
// no_progress guard on the draft's /text. The writer output is identical every
// iteration (no context spec, constant prompt), so the guard fires at the second
// iteration. policy is "proceed" or "fail".
func noProgressLoopDef(policy string, maxIter int) string {
	return `{
		"schema_version": 1,
		"name": "no-progress-loop",
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
			{"from": "critique", "to": "draft", "type": "loop", "condition": "output.json.verdict == 'revise'", "max_iterations": ` + strconv.Itoa(maxIter) + `,
				"no_progress": {"step": "draft", "path": "/text", "policy": "` + policy + `"}},
			{"from": "critique", "to": "publish"}
		]
	}`
}

// guardTrippedEvents returns the run's guard_tripped event payloads in seq order.
func guardTrippedEvents(t *testing.T, s *store.Store, runID uuid.UUID) []store.GuardTrippedEvent {
	t.Helper()
	evs, err := s.Events().List(t.Context(), runID, 0, 500)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var out []store.GuardTrippedEvent
	for _, ev := range evs {
		if ev.Type != store.EventGuardTripped {
			continue
		}
		var g store.GuardTrippedEvent
		if err := json.Unmarshal(ev.Payload, &g); err != nil {
			t.Fatalf("decoding guard_tripped payload: %v", err)
		}
		out = append(out, g)
	}
	return out
}

// loopNoProgressEvents returns the run's loop_no_progress event payloads.
func loopNoProgressEvents(t *testing.T, s *store.Store, runID uuid.UUID) []store.LoopNoProgressEvent {
	t.Helper()
	evs, err := s.Events().List(t.Context(), runID, 0, 500)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var out []store.LoopNoProgressEvent
	for _, ev := range evs {
		if ev.Type != store.EventLoopNoProgress {
			continue
		}
		var np store.LoopNoProgressEvent
		if err := json.Unmarshal(ev.Payload, &np); err != nil {
			t.Fatalf("decoding loop_no_progress payload: %v", err)
		}
		out = append(out, np)
	}
	return out
}

// requireGuard asserts exactly one guard_tripped event with the given guard,
// current, and cap fired, returning it.
func requireGuard(t *testing.T, s *store.Store, runID uuid.UUID, guard string) store.GuardTrippedEvent {
	t.Helper()
	var matched []store.GuardTrippedEvent
	for _, g := range guardTrippedEvents(t, s, runID) {
		if g.Guard == guard {
			matched = append(matched, g)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("guard_tripped[%s] events = %d, want 1 (all: %+v)", guard, len(matched), guardTrippedEvents(t, s, runID))
	}
	return matched[0]
}

// TestGuardMaxTotalStepsHaltsRunawayLoop: a runaway loop whose next iteration
// would exceed max_total_steps dead-letters the loop source permanently with a
// guard_tripped(max_total_steps) event, and the run fails.
func TestGuardMaxTotalStepsHaltsRunawayLoop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Initial 3 steps (draft, critique, publish); each iteration adds 2 (draft#k,
	// critique#k). Cap 5: iteration 1 lands at 5 (ok), iteration 2 would be 7 → rejected.
	s, h, runID := setupWithParams(t, guardCapLoopDef(`"expansion": {"max_total_steps": 5},`, 20), json.RawMessage(`{"brief": "b"}`))
	d := startDispatcher(t, s, h.Queue())
	loopSpawn(t, s, h, d, "guard-a", loopAlwaysRevise())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	g := requireGuard(t, s, runID, "max_total_steps")
	if g.Current != 7 || g.Cap != 5 || g.Unit != "steps" || g.Action != "fail" {
		t.Errorf("guard = %+v, want current 7 cap 5 unit steps action fail", g)
	}
	steps := requireSteps(t, s, runID)
	if st := steps["critique#1"]; st.Status != store.StepStatusDeadLettered {
		t.Errorf("critique#1 status = %q, want dead_lettered", st.Status)
	}
}

// TestGuardMaxExpansionsHaltsRunawayLoop: the same runaway loop under a
// max_expansions=1 cap halts after one iteration with a guard_tripped
// (max_expansions) event and a failed run.
func TestGuardMaxExpansionsHaltsRunawayLoop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setupWithParams(t, guardCapLoopDef(`"expansion": {"max_expansions": 1},`, 20), json.RawMessage(`{"brief": "b"}`))
	d := startDispatcher(t, s, h.Queue())
	loopSpawn(t, s, h, d, "guard-a", loopAlwaysRevise())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	g := requireGuard(t, s, runID, "max_expansions")
	if g.Current != 2 || g.Cap != 1 || g.Unit != "expansions" || g.Action != "fail" {
		t.Errorf("guard = %+v, want current 2 cap 1 unit expansions action fail", g)
	}
	steps := requireSteps(t, s, runID)
	if st := steps["critique#1"]; st.Status != store.StepStatusDeadLettered {
		t.Errorf("critique#1 status = %q, want dead_lettered", st.Status)
	}
}

// guardLoopEngine builds an engine wired like loopSpawn (agent + echo, board,
// json_schema validator) but on the supplied clock and without spawning it, for
// the manually-driven wall-clock test.
func guardLoopEngine(t *testing.T, s *store.Store, id string, mc *llm.MockConfig, now func() time.Time) *engine.Engine {
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
	eng, err := engine.New(s, reg, id,
		engine.WithBlackboard(board), engine.WithValidators(vreg), engine.WithClock(now))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return eng
}

// TestGuardWallClockHaltsLoopAtClaim: a running loop whose wall-clock deadline
// passes mid-flight is cancelled at the next claim — no reconciler involved. The
// run reaches cancelled with reason deadline_exceeded and a guard_tripped
// (max_wall_clock) event carrying elapsed-vs-cap seconds. Driven manually on a
// fake clock so the deadline crossing is deterministic.
func TestGuardWallClockHaltsLoopAtClaim(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	clk := newFakeClock(testNow)
	eng := guardLoopEngine(t, s, "guard-wc", loopAlwaysRevise(), clk.Now)

	// deadline_at = created-at (testNow) + 1h. High max_iterations so the loop
	// would run forever; only the wall-clock guard stops it.
	def := guardCapLoopDef(``, 100)
	// Inject the wall-clock deadline into the definition.
	def = injectField(def, `"max_wall_clock": "1h",`)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, def), Params: json.RawMessage(`{"brief": "b"}`), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID

	// Drive one full iteration at fake-time testNow (before the deadline): draft(0)
	// then critique(0), whose completion expands iteration 1 (draft#1 becomes ready).
	driveStep(t, s, h, eng, "guard-wc", runID, "draft")
	driveStep(t, s, h, eng, "guard-wc", runID, "critique")
	steps := requireSteps(t, s, runID)
	if _, ok := steps["draft#1"]; !ok {
		t.Fatal("draft#1 not injected — the loop did not iterate before the deadline test")
	}

	// The deadline passes. The next claim (draft#1) trips the wall-clock guard.
	clk.Set(testNow.Add(2 * time.Hour))
	driveStep(t, s, h, eng, "guard-wc", runID, "draft#1")

	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.Status != store.RunStatusCancelled ||
		run.CancelReason == nil || *run.CancelReason != store.RunCancelReasonDeadlineExceeded {
		t.Fatalf("run = status %q reason %v, want cancelled/deadline_exceeded", run.Status, run.CancelReason)
	}
	g := requireGuard(t, s, runID, "max_wall_clock")
	if g.Unit != "seconds" || g.Action != "cancel" || g.Cap != 3600 || g.Current < 3600 {
		t.Errorf("guard = %+v, want unit seconds action cancel cap 3600 current >= 3600", g)
	}
	if st := requireSteps(t, s, runID)["draft#1"]; st.Status != store.StepStatusCancelled {
		t.Errorf("draft#1 status = %q, want cancelled", st.Status)
	}
}

// TestLoopNoProgressProceedExits: identical consecutive drafts trigger the
// no-progress guard; under proceed the loop exits early to publish and the run
// succeeds, with exactly one loop_no_progress event and no loop_exhausted.
func TestLoopNoProgressProceedExits(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setupWithParams(t, noProgressLoopDef("proceed", 20), json.RawMessage(`{"brief": "b"}`))
	d := startDispatcher(t, s, h.Queue())
	loopSpawn(t, s, h, d, "np-a", loopAlwaysRevise())

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// The guard fired at iteration 1 (comparing draft#1 to draft), so the loop
	// exited to publish after exactly one extra iteration.
	requireStepStatuses(t, s, runID, map[string]string{
		"draft":      store.StepStatusSucceeded,
		"draft#1":    store.StepStatusSucceeded,
		"critique":   store.StepStatusSucceeded,
		"critique#1": store.StepStatusSucceeded,
		"publish":    store.StepStatusSucceeded,
	})
	np := loopNoProgressEvents(t, s, runID)
	if len(np) != 1 {
		t.Fatalf("loop_no_progress events = %d, want 1", len(np))
	}
	e := np[0]
	if e.LoopSourceStep != "critique" || e.ComparedStep != "draft" || e.Iteration != 1 ||
		e.PrevInstance != "draft" || e.CurInstance != "draft#1" || e.Path != "/text" {
		t.Errorf("loop_no_progress = %+v, want source critique / compared draft / iter 1 / draft→draft#1 / /text", e)
	}
	if e.Policy != "proceed" || e.Action != "proceed" || e.Hash == "" {
		t.Errorf("loop_no_progress policy/action/hash = %q/%q/%q", e.Policy, e.Action, e.Hash)
	}
	if got := len(loopExhaustedEvents(t, s, runID)); got != 0 {
		t.Errorf("loop_exhausted events = %d, want 0 (no-progress fired first)", got)
	}
}

// TestLoopNoProgressFailDeadLetters: under the fail policy the no-progress guard
// dead-letters the loop source and fails the run.
func TestLoopNoProgressFailDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setupWithParams(t, noProgressLoopDef("fail", 20), json.RawMessage(`{"brief": "b"}`))
	d := startDispatcher(t, s, h.Queue())
	loopSpawn(t, s, h, d, "np-a", loopAlwaysRevise())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	np := loopNoProgressEvents(t, s, runID)
	if len(np) != 1 || np[0].Action != "fail" || np[0].Policy != "fail" {
		t.Fatalf("loop_no_progress = %+v, want exactly one fail/fail", np)
	}
	steps := requireSteps(t, s, runID)
	if st := steps["critique#1"]; st.Status != store.StepStatusDeadLettered {
		t.Errorf("critique#1 status = %q, want dead_lettered", st.Status)
	}
	if st, ok := steps["publish"]; ok && st.Status == store.StepStatusSucceeded {
		t.Error("publish succeeded — the run should have failed under no_progress: fail")
	}
}

// TestLoopNoProgressDisabledByDefault: the same identical-output runaway loop
// WITHOUT a no_progress guard runs to the iteration cap (loop_exhausted), never
// firing a no-progress event — proving the detector is opt-in.
func TestLoopNoProgressDisabledByDefault(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// No no_progress guard; on_exhausted defaults to proceed, so it exits to publish.
	s, h, runID := setupWithParams(t, guardCapLoopDef(``, 2), json.RawMessage(`{"brief": "b"}`))
	d := startDispatcher(t, s, h.Queue())
	loopSpawn(t, s, h, d, "np-a", loopAlwaysRevise())

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	if got := len(loopNoProgressEvents(t, s, runID)); got != 0 {
		t.Errorf("loop_no_progress events = %d, want 0 (guard is opt-in)", got)
	}
	if got := len(loopExhaustedEvents(t, s, runID)); got != 1 {
		t.Errorf("loop_exhausted events = %d, want 1 (the loop ran to its cap)", got)
	}
}

// driveStep manually drains, enqueues, reads, and handles one step delivery on
// eng — the deterministic single-step driver (the failpoint-test pattern),
// used where a fake clock must advance at a precise point between steps.
func driveStep(t *testing.T, s *store.Store, h *queuetest.Harness, eng *engine.Engine, consumer string, runID uuid.UUID, stepID string) {
	t.Helper()
	ctx := t.Context()
	loseDispatch(t, s) // clear the outbox row(s) so the manual enqueue is the only delivery
	h.Enqueue(ctx, stepEnvelope(runID, stepID))
	msg := h.ReadOne(ctx, consumer)
	if err := eng.Handle(ctx, queue.Delivery{ID: msg.ID, Envelope: stepEnvelope(runID, stepID), DeliveryCount: 1}); err != nil {
		t.Fatalf("handling %s: %v", stepID, err)
	}
}

// injectField splices a top-level JSON field (e.g. `"max_wall_clock": "1h",`)
// right after the opening brace of a definition literal.
func injectField(def, field string) string {
	i := 0
	for i < len(def) && def[i] != '{' {
		i++
	}
	return def[:i+1] + "\n\t\t" + field + def[i+1:]
}
