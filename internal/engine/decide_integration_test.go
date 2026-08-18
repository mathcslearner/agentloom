//go:build integration

package engine_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// parkedGate spins up a fleet on a fixture, waits for its human_approval step
// to park, and returns the engine, run id, gate step id, and the pending
// approval id — the shared setup for every decision-matrix test.
func parkedGate(t *testing.T, fixture, gateStep string) (*store.Store, *engine.Engine, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	s, h, runID := setupWithParams(t, readDef(t, fixture), json.RawMessage(`{"topic": "turtles"}`))
	d := startDispatcher(t, s, h.Queue())
	reg := approvalRegistry(t)
	eng, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))

	waitStep(t, s, runID, gateStep, "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	approvals, err := s.Approvals().ListByRun(ctx, runID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("ListByRun approvals = %+v, err %v; want one pending", approvals, err)
	}
	return s, eng, runID, approvals[0].ID
}

// TestDecideApproveCompletes: an approve decision succeeds the gate with the
// original payload and dispatches the downstream publish step to completion.
func TestDecideApproveCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, eng, runID, approvalID := parkedGate(t, "approval_gate.json", "approve_publish")

	res, err := eng.Decide(ctx, approvalID, engine.DecideRequest{
		Decision: dag.ApprovalApprove, DecidedBy: "key_operator", Source: store.ApprovalSourceHuman,
	})
	if err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	if res.Approval.Status != store.ApprovalStatusApproved {
		t.Errorf("approval status = %q, want approved", res.Approval.Status)
	}

	waitRun(t, s, runID, store.RunStatusSucceeded)

	// The gate succeeded with the decision output; publish consumed the payload.
	gate, _ := s.Steps().Get(ctx, runID, "approve_publish")
	if gate.Status != store.StepStatusSucceeded {
		t.Fatalf("gate status = %q, want succeeded", gate.Status)
	}
	var out struct {
		Decision  string          `json:"decision"`
		Payload   json.RawMessage `json:"payload"`
		Edited    bool            `json:"edited"`
		DecidedBy string          `json:"decided_by"`
		Source    string          `json:"source"`
	}
	if err := json.Unmarshal(gate.Output, &out); err != nil {
		t.Fatalf("gate output not a decision: %v (%s)", err, gate.Output)
	}
	if out.Decision != "approve" || out.Edited || out.DecidedBy != "key_operator" || out.Source != "human" {
		t.Errorf("decision output = %+v, want approve/human/key_operator/not-edited", out)
	}
	publish, _ := s.Steps().Get(ctx, runID, "publish")
	var pub struct {
		Published json.RawMessage `json:"published"`
	}
	if err := json.Unmarshal(publish.Output, &pub); err != nil {
		t.Fatalf("publish output: %v (%s)", err, publish.Output)
	}
	if string(pub.Published) != string(out.Payload) {
		t.Errorf("publish.published = %s, want the approved payload %s", pub.Published, out.Payload)
	}

	// Audit: one immutable approval_decided event and the decided row.
	if n := countEvents(t, s, runID, store.EventApprovalDecided); n != 1 {
		t.Errorf("approval_decided events = %d, want 1", n)
	}
	got, _ := s.Approvals().Get(ctx, approvalID)
	if got.DecidedBy == nil || *got.DecidedBy != "key_operator" {
		t.Errorf("decided_by = %v, want key_operator", got.DecidedBy)
	}
}

// TestDecideApproveWithEdit: an edited payload (valid against the edit schema)
// replaces the original as the step's output payload.
func TestDecideApproveWithEdit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, eng, runID, approvalID := parkedGate(t, "approval_gate.json", "approve_publish")

	edit := json.RawMessage(`{"text":"EDITED ARTICLE"}`)
	if _, err := eng.Decide(ctx, approvalID, engine.DecideRequest{
		Decision: dag.ApprovalApprove, EditedPayload: edit,
		DecidedBy: "key_editor", Source: store.ApprovalSourceHuman,
	}); err != nil {
		t.Fatalf("Decide approve+edit: %v", err)
	}

	waitRun(t, s, runID, store.RunStatusSucceeded)

	publish, _ := s.Steps().Get(ctx, runID, "publish")
	var pub struct {
		Published struct {
			Text string `json:"text"`
		} `json:"published"`
	}
	if err := json.Unmarshal(publish.Output, &pub); err != nil {
		t.Fatalf("publish output: %v (%s)", err, publish.Output)
	}
	if pub.Published.Text != "EDITED ARTICLE" {
		t.Errorf("publish.published.text = %q, want the edited text", pub.Published.Text)
	}
	got, _ := s.Approvals().Get(ctx, approvalID)
	if got.Decision == nil || *got.Decision != "approve" || len(got.EditedPayload) == 0 {
		t.Errorf("approval row = %+v, want approve with edited_payload stored", got)
	}
}

// TestDecideRejectFailsAndRequeues: a reject under the default on_reject: fail
// dead-letters the gate (permanent, approval_rejected message) and fails the
// run; a DLQ requeue re-runs the gate, producing a fresh pending approval.
func TestDecideRejectFailsAndRequeues(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, eng, runID, approvalID := parkedGate(t, "approval_gate.json", "approve_publish")

	if _, err := eng.Decide(ctx, approvalID, engine.DecideRequest{
		Decision: dag.ApprovalReject, Comment: "not ready", DecidedBy: "key_reviewer",
		Source: store.ApprovalSourceHuman,
	}); err != nil {
		t.Fatalf("Decide reject: %v", err)
	}

	waitRun(t, s, runID, store.RunStatusFailed)
	gate, _ := s.Steps().Get(ctx, runID, "approve_publish")
	if gate.Status != store.StepStatusDeadLettered {
		t.Fatalf("gate status = %q, want dead_lettered", gate.Status)
	}
	dls, _ := s.DeadLetters().ListByRun(ctx, runID)
	if len(dls) != 1 || dls[0].Source != store.DeadLetterSourcePermanent {
		t.Fatalf("dead letters = %+v, want one permanent", dls)
	}

	// Requeue re-runs the gate: a fresh pending approval appears (distinct id).
	if _, err := eng.Requeue(ctx, runID, "approve_publish"); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	if n, _ := s.Approvals().CountPending(ctx); n != 1 {
		t.Errorf("CountPending after requeue = %d, want 1 (fresh gate)", n)
	}
	fresh, _ := s.Approvals().ListByRun(ctx, runID)
	var pending int
	for _, a := range fresh {
		if a.Status == store.ApprovalStatusPending && a.ID != approvalID {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("fresh pending approvals = %d, want 1 distinct from the rejected one", pending)
	}
}

// TestDecideRejectRoutes: a reject under on_reject: route succeeds the gate
// with decision: reject, fires only the reject edge (notify_rejected), skips
// the approve edge (publish), and the run completes.
func TestDecideRejectRoutes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, eng, runID, approvalID := parkedGate(t, "approval_reject_route.json", "review")

	if _, err := eng.Decide(ctx, approvalID, engine.DecideRequest{
		Decision: dag.ApprovalReject, Comment: "send back", DecidedBy: "key_reviewer",
		Source: store.ApprovalSourceHuman,
	}); err != nil {
		t.Fatalf("Decide reject-route: %v", err)
	}

	waitRun(t, s, runID, store.RunStatusSucceeded)
	gate, _ := s.Steps().Get(ctx, runID, "review")
	if gate.Status != store.StepStatusSucceeded {
		t.Errorf("gate status = %q, want succeeded (route)", gate.Status)
	}
	notify, _ := s.Steps().Get(ctx, runID, "notify_rejected")
	if notify.Status != store.StepStatusSucceeded {
		t.Errorf("notify_rejected status = %q, want succeeded (reject edge fired)", notify.Status)
	}
	publish, _ := s.Steps().Get(ctx, runID, "publish")
	if publish.Status != store.StepStatusSkipped {
		t.Errorf("publish status = %q, want skipped (approve edge not fired)", publish.Status)
	}
}

// TestDecideInvalidEditRejected: an edited payload violating the edit schema
// is a *DecisionInvalidError with issues, and the approval stays pending.
func TestDecideInvalidEditRejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, eng, _, approvalID := parkedGate(t, "approval_gate.json", "approve_publish")

	// The edit schema requires text to be a string; a number violates it.
	_, err := eng.Decide(ctx, approvalID, engine.DecideRequest{
		Decision: dag.ApprovalApprove, EditedPayload: json.RawMessage(`{"text": 123}`),
		DecidedBy: "key_editor", Source: store.ApprovalSourceHuman,
	})
	var invalid *engine.DecisionInvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("Decide invalid edit err = %v, want *DecisionInvalidError", err)
	}
	if len(invalid.Issues) == 0 {
		t.Errorf("invalid decision carried no issues, want at least one schema violation")
	}
	got, _ := s.Approvals().Get(ctx, approvalID)
	if got.Status != store.ApprovalStatusPending {
		t.Errorf("approval status = %q after invalid edit, want still pending", got.Status)
	}
}

// TestDecideDoubleDecideConflicts: two concurrent decisions on one approval —
// exactly one wins the CAS; the other gets *ApprovalNotPendingError. Exactly
// one approval_decided event, no double fan-out.
func TestDecideDoubleDecideConflicts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, eng, runID, approvalID := parkedGate(t, "approval_gate.json", "approve_publish")

	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := eng.Decide(ctx, approvalID, engine.DecideRequest{
				Decision: dag.ApprovalApprove, DecidedBy: "key_racer", Source: store.ApprovalSourceHuman,
			})
			results[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	var wins, conflicts int
	for _, err := range results {
		var notPending *store.ApprovalNotPendingError
		switch {
		case err == nil:
			wins++
		case errors.As(err, &notPending):
			conflicts++
		default:
			t.Fatalf("unexpected decide error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("double decide: %d wins, %d conflicts; want exactly 1 each", wins, conflicts)
	}
	if n := countEvents(t, s, runID, store.EventApprovalDecided); n != 1 {
		t.Errorf("approval_decided events = %d, want exactly 1", n)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
}

// TestDecideParkedRunResumes: a manually parked run still accepts a decision
// (ADR-017 fail-fast note allows running|parked). The gate succeeds; unpark
// resumes dispatch of the downstream step.
func TestDecideParkedRunResumes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, eng, runID, approvalID := parkedGate(t, "approval_gate.json", "approve_publish")

	if _, err := eng.Park(ctx, runID, store.ParkReasonManual); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if _, err := eng.Decide(ctx, approvalID, engine.DecideRequest{
		Decision: dag.ApprovalApprove, DecidedBy: "key_operator", Source: store.ApprovalSourceHuman,
	}); err != nil {
		t.Fatalf("Decide on parked run: %v", err)
	}
	// The gate settled; the downstream publish is ready-without-dispatch while
	// parked. Unpark re-dispatches it.
	if _, err := eng.Unpark(ctx, runID); err != nil {
		t.Fatalf("Unpark: %v", err)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
}
