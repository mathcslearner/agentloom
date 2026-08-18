//go:build integration

package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// approvalRegistry is the builtin set with a mock provider — enough to run
// approval_gate.json (an llm draft, a human_approval gate, an echo publish).
func approvalRegistry(t *testing.T) *exec.Registry {
	t.Helper()
	return exec.CoreBuiltins(mockProviders(t), nil, nil)
}

// readDef loads an example definition document.
func readDef(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", name)) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

// TestApprovalParksWithoutLease is 15.2 criteria (a) and (c): a
// human_approval step parks its branch without holding a lease — the PEL
// empties while it waits — and the fleet keeps executing other runs; the
// pending approval is visible in run state and events.
func TestApprovalParksWithoutLease(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setupWithParams(t, readDef(t, "approval_gate.json"), json.RawMessage(`{"topic": "turtles"}`))
	d := startDispatcher(t, s, h.Queue())

	reg := approvalRegistry(t)
	spawn := func(id string) {
		w, err := engine.New(s, reg, id, engine.WithDispatchNudge(d.Nudge))
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		h.Spawn(id, w.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	}
	spawn("worker-a")
	spawn("worker-b")

	// The gate parks: approve_publish reaches awaiting_human, the run stays
	// running.
	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	if run, _ := s.Runs().Get(ctx, runID); run.Status != store.RunStatusRunning {
		t.Fatalf("run status = %q while a gate is parked, want running", run.Status)
	}

	// (c) The pending approval is visible in run state...
	approvals, err := s.Approvals().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun approvals: %v", err)
	}
	if len(approvals) != 1 || approvals[0].Status != store.ApprovalStatusPending {
		t.Fatalf("approvals = %+v, want one pending", approvals)
	}
	if approvals[0].StepID != "approve_publish" || approvals[0].Title != "Publish this article?" {
		t.Errorf("approval = %+v, want the rendered gate", approvals[0])
	}
	// The payload is the rendered draft output (an llm result with text).
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(approvals[0].Payload, &payload); err != nil {
		t.Fatalf("approval payload not a draft output: %v (%s)", err, approvals[0].Payload)
	}
	if payload.Text == "" {
		t.Errorf("approval payload text empty, want the rendered draft (%s)", approvals[0].Payload)
	}
	// ...and in the event log.
	if !hasEventType(t, s, runID, store.EventApprovalRequested) {
		t.Errorf("no approval_requested event")
	}

	// (a) No lease held: the PEL empties. The draft entry acked on success and
	// the gate acked on park, so nothing is in flight while it waits.
	stats := h.WaitStats(ctx, func(st queue.StreamStats) bool { return st.Pending == 0 })
	if stats.Pending != 0 {
		t.Fatalf("PEL has %d in-flight entries while parked, want 0 (no lease held)", stats.Pending)
	}

	// (a) The fleet keeps executing other work: a fresh run completes on the
	// same two workers while the gate stays parked.
	other, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: mustDecode(t, readDef(t, "mock_pipeline.json")),
		Params:     json.RawMessage(`{"topic": "otters"}`), Now: testNow,
	})
	if err != nil {
		t.Fatalf("CreateRun (throughput probe): %v", err)
	}
	waitRun(t, s, other.Run.ID, store.RunStatusSucceeded)

	// The gate is still parked throughout — it never resumed on its own.
	if st, _ := s.Steps().Get(ctx, runID, "approve_publish"); st.Status != store.StepStatusAwaitingHuman {
		t.Errorf("gate status = %q after other work completed, want still awaiting_human", st.Status)
	}
}

// TestApprovalCrashBeforeAckConverges is 15.2 criterion (b): a worker that
// commits the park then dies before ACK leaves a durable awaiting_human step
// and an un-acked PEL entry; a survivor reclaims it, sees awaiting_human, and
// ACK-and-drops — exactly one approval, no double execution.
func TestApprovalCrashBeforeAckConverges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setupWithParams(t, readDef(t, "approval_gate.json"), json.RawMessage(`{"topic": "turtles"}`))
	d := startDispatcher(t, s, h.Queue())
	reg := approvalRegistry(t)

	// Worker A processes the draft, then parks the gate and dies pre-ACK.
	wa, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New A: %v", err)
	}
	a := h.Spawn("worker-a", wa.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	a.KillAt(queue.PhasePreAck, "approve_publish")

	// The kill fires once the park committed but before the ACK.
	select {
	case <-a.Killed():
	case <-time.After(15 * time.Second):
		t.Fatal("worker A never reached the pre-ACK crash point on approve_publish")
	}

	// The park is durable and the entry is still pending (un-acked).
	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})

	// A survivor reclaims the pending entry, sees awaiting_human, ACK-drops.
	wb, err := engine.New(s, reg, "worker-b", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New B: %v", err)
	}
	h.Spawn("worker-b", wb.Handle, queuetest.LeaseConfig(400*time.Millisecond))

	// Convergence: the PEL empties (the reclaimed duplicate was dropped).
	stats := h.WaitStats(ctx, func(st queue.StreamStats) bool { return st.Pending == 0 })
	if stats.Pending != 0 {
		t.Fatalf("PEL has %d in-flight after reclaim, want 0 (ack-drop)", stats.Pending)
	}

	// Exactly one approval, still pending; the step is still awaiting_human —
	// no double execution, no second park.
	approvals, err := s.Approvals().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun approvals: %v", err)
	}
	if len(approvals) != 1 || approvals[0].Status != store.ApprovalStatusPending {
		t.Fatalf("approvals = %+v, want exactly one pending (no double park)", approvals)
	}
	if n := countEvents(t, s, runID, store.EventApprovalRequested); n != 1 {
		t.Errorf("approval_requested events = %d, want exactly 1", n)
	}
	if st, _ := s.Steps().Get(ctx, runID, "approve_publish"); st.Status != store.StepStatusAwaitingHuman {
		t.Errorf("gate status = %q after reclaim, want still awaiting_human", st.Status)
	}
}

// TestApprovalCancelSweepsParkedGate: cancelling a run whose gate is parked
// sweeps the awaiting_human step to cancelled (attempt closed cancelled,
// approval cancelled) and finalizes the run — the 5.6 sweep, widened (15.2).
func TestApprovalCancelSweepsParkedGate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setupWithParams(t, readDef(t, "approval_gate.json"), json.RawMessage(`{"topic": "turtles"}`))
	d := startDispatcher(t, s, h.Queue())
	reg := approvalRegistry(t)

	eng, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))

	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})

	// The gate holds no lease, so nothing is in flight — the cancel sweep
	// settles the parked step directly and finalizes.
	res, err := eng.Cancel(ctx, runID, store.RunCancelReasonManual)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !res.Finalized {
		t.Errorf("cancel not finalized, want finalized (no step in flight)")
	}
	found := false
	for _, id := range res.Cancelled {
		if id == "approve_publish" {
			found = true
		}
	}
	if !found {
		t.Errorf("sweep cancelled %v, want the parked gate", res.Cancelled)
	}

	run := waitRunStatus(t, s, runID, store.RunStatusCancelled)
	_ = run
	// The gate is cancelled and the approval is no longer pending.
	if st, _ := s.Steps().Get(ctx, runID, "approve_publish"); st.Status != store.StepStatusCancelled {
		t.Errorf("gate status = %q, want cancelled", st.Status)
	}
	approvals, _ := s.Approvals().ListByRun(ctx, runID)
	if len(approvals) != 1 || approvals[0].Status != store.ApprovalStatusCancelled {
		t.Errorf("approval = %+v, want one cancelled", approvals)
	}
	if n, _ := s.Approvals().CountPending(ctx); n != 0 {
		t.Errorf("CountPending = %d, want 0 after cancel", n)
	}
	if !hasEventType(t, s, runID, store.EventApprovalCancelled) {
		t.Errorf("no approval_cancelled event")
	}
}
