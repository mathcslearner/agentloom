//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// seedRunningApproval creates a run with one ready human_approval step,
// claims it, and returns the run id and the live claim — the state the
// executor's park path starts from.
func seedRunningApproval(t *testing.T, s *store.Store) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)
	if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
		RunID: run.ID, StepID: "gate", StepType: "human_approval",
		Status: store.StepStatusReady, UpdatedAt: testNow,
	}); err != nil {
		t.Fatalf("seeding approval step: %v", err)
	}
	step := mustClaim(t, s, run.ID, "gate")
	return run.ID, *step.ClaimID
}

func sampleApprovalRow() store.ApprovalRow {
	timeoutAt := testNow.Add(48 * time.Hour)
	return store.ApprovalRow{
		ID:               uuid.New(),
		Title:            "Publish this?",
		Description:      "Review before publishing.",
		Payload:          json.RawMessage(`{"text":"draft body"}`),
		AllowedDecisions: []string{"approve", "reject"},
		AllowEdit:        true,
		EditSchema:       json.RawMessage(`{"type":"object"}`),
		TimeoutAt:        &timeoutAt,
	}
}

// TestAwaitHumanStepParks is 15.2 criterion (a)/(c) at the store level: the
// park CAS moves the step to awaiting_human without a claim, writes the
// pending approvals row, appends approval_requested, and leaves the attempt
// open (the attempt spans the wait).
func TestAwaitHumanStepParks(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, claim := seedRunningApproval(t, s)
	row := sampleApprovalRow()

	var parked gen.RunStep
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		parked, err = store.AwaitHumanStep(ctx, q, store.AwaitHumanStepArgs{
			RunID: runID, StepID: "gate", ClaimID: claim, Approval: row, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("AwaitHumanStep: %v", err)
	}

	if parked.Status != store.StepStatusAwaitingHuman {
		t.Errorf("step status = %q, want awaiting_human", parked.Status)
	}
	if parked.ClaimID != nil {
		t.Errorf("claim not cleared at park: %v", parked.ClaimID)
	}

	// The approvals row is pending and carries the rendered snapshot.
	approvals, err := s.Approvals().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(approvals) != 1 {
		t.Fatalf("got %d approvals, want 1", len(approvals))
	}
	a := approvals[0]
	if a.Status != store.ApprovalStatusPending {
		t.Errorf("approval status = %q, want pending", a.Status)
	}
	if !jsonEqual(t, a.Payload, json.RawMessage(`{"text":"draft body"}`)) {
		t.Errorf("approval payload = %s, want the rendered draft", a.Payload)
	}
	if a.AllowEdit != true || len(a.AllowedDecisions) != 2 {
		t.Errorf("approval edit/decisions = %v/%v, want true/[approve reject]", a.AllowEdit, a.AllowedDecisions)
	}
	if a.TimeoutAt == nil || !a.TimeoutAt.Equal(testNow.Add(48*time.Hour)) {
		t.Errorf("approval timeout_at = %v, want %v", a.TimeoutAt, testNow.Add(48*time.Hour))
	}

	// The attempt is still open — it spans the human wait.
	attempts, err := s.Attempts().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("got %d attempts, want 1", len(attempts))
	}
	if attempts[0].Outcome != nil || attempts[0].FinishedAt != nil {
		t.Errorf("attempt closed at park (outcome %v, finished %v) — the attempt should span the wait",
			attempts[0].Outcome, attempts[0].FinishedAt)
	}

	if got := eventTypes(t, s, runID); !containsEvent(got, store.EventApprovalRequested) {
		t.Errorf("events = %v, want an approval_requested", got)
	}

	// CountPending sees it.
	if n, err := s.Approvals().CountPending(ctx); err != nil || n != 1 {
		t.Errorf("CountPending = %d, %v, want 1, nil", n, err)
	}
}

// TestAwaitHumanStepFenced: a stale claim's park is rejected and writes
// nothing — the zombie's park never lands (the abandonFenced path).
func TestAwaitHumanStepFenced(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, _ := seedRunningApproval(t, s)
	stale := uuid.New()

	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.AwaitHumanStep(ctx, q, store.AwaitHumanStepArgs{
			RunID: runID, StepID: "gate", ClaimID: stale, Approval: sampleApprovalRow(), Now: testNow,
		})
		return err
	})
	_ = conflictError(t, err, store.ConflictClaimMismatch)

	// No approvals row was written, and the step is still running.
	if approvals, aerr := s.Approvals().ListByRun(ctx, runID); aerr != nil || len(approvals) != 0 {
		t.Errorf("got %d approvals (err %v), want 0 — a fenced park writes nothing", len(approvals), aerr)
	}
	step, err := s.Steps().Get(ctx, runID, "gate")
	if err != nil {
		t.Fatalf("Get step: %v", err)
	}
	if step.Status != store.StepStatusRunning {
		t.Errorf("step status = %q, want still running", step.Status)
	}
}

// TestAwaitHumanUniquePendingIndex proves the unique partial index rejects a
// second pending approval for the same step — the one-open-approval invariant.
func TestAwaitHumanUniquePendingIndex(t *testing.T) {
	t.Parallel()
	pool := storetest.NewDB(t)
	s := store.NewFromPool(pool)
	ctx := t.Context()
	runID, claim := seedRunningApproval(t, s)
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.AwaitHumanStep(ctx, q, store.AwaitHumanStepArgs{
			RunID: runID, StepID: "gate", ClaimID: claim, Approval: sampleApprovalRow(), Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("first park: %v", err)
	}

	// A second pending row for the same (run_id, step_id) violates the index.
	_, err := pool.Exec(ctx,
		`INSERT INTO approvals (id, run_id, step_id, attempt, title, allowed_decisions)
		 VALUES ($1, $2, 'gate', 1, 'dup', ARRAY['approve'])`,
		uuid.New(), runID)
	wantPgError(t, err, pgUniqueViolation, "second pending approval for the same step")
}

// TestCancelAwaitingHumanStep: the run-cancel sweep settles a parked gate —
// step cancelled, attempt closed cancelled, approval cancelled, both events.
func TestCancelAwaitingHumanStep(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, claim := seedRunningApproval(t, s)
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.AwaitHumanStep(ctx, q, store.AwaitHumanStepArgs{
			RunID: runID, StepID: "gate", ClaimID: claim, Approval: sampleApprovalRow(), Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("park: %v", err)
	}

	var cancelled gen.RunStep
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		cancelled, err = store.CancelAwaitingHumanStep(ctx, q, store.CancelAwaitingHumanStepArgs{
			RunID: runID, StepID: "gate", Reason: store.CancelReasonRunCancelled, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("CancelAwaitingHumanStep: %v", err)
	}
	if cancelled.Status != store.StepStatusCancelled {
		t.Errorf("step status = %q, want cancelled", cancelled.Status)
	}

	// The approval is cancelled and no longer pending.
	approvals, _ := s.Approvals().ListByRun(ctx, runID)
	if len(approvals) != 1 || approvals[0].Status != store.ApprovalStatusCancelled {
		t.Errorf("approval = %+v, want one cancelled row", approvals)
	}
	if n, _ := s.Approvals().CountPending(ctx); n != 0 {
		t.Errorf("CountPending = %d, want 0 after cancel", n)
	}

	// The attempt closed cancelled (it spanned the wait).
	attempts, _ := s.Attempts().ListByRun(ctx, runID)
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.AttemptOutcomeCancelled {
		t.Errorf("attempt outcome = %v, want cancelled", attempts[0].Outcome)
	}

	got := eventTypes(t, s, runID)
	if !containsEvent(got, store.EventStepCancelled) || !containsEvent(got, store.EventApprovalCancelled) {
		t.Errorf("events = %v, want step_cancelled and approval_cancelled", got)
	}
}

// containsEvent reports whether the event-type slice contains typ.
func containsEvent(types []string, typ string) bool {
	for _, e := range types {
		if e == typ {
			return true
		}
	}
	return false
}

// parkGate seeds a running gate, parks it, and returns the run id and the
// pending approval — the state 15.3's decide path starts from.
func parkGate(t *testing.T, s *store.Store) (uuid.UUID, store.ApprovalRow) {
	t.Helper()
	ctx := t.Context()
	runID, claim := seedRunningApproval(t, s)
	row := sampleApprovalRow()
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.AwaitHumanStep(ctx, q, store.AwaitHumanStepArgs{
			RunID: runID, StepID: "gate", ClaimID: claim, Approval: row, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("parkGate: %v", err)
	}
	return runID, row
}

// TestDecideApprovalCAS: the decide CAS moves pending → approved, records the
// decision fields, appends approval_decided, and a second decide on the now
// non-pending row returns *ApprovalNotPendingError (the single arbiter).
func TestDecideApprovalCAS(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, row := parkGate(t, s)

	var decided gen.Approval
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		decided, err = store.DecideApproval(ctx, q, store.DecideApprovalArgs{
			ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
			Status: store.ApprovalStatusApproved, Decision: "approve",
			EditedPayload: json.RawMessage(`{"text":"edited"}`), Comment: "ok",
			DecidedBy: "key_op", Source: store.ApprovalSourceHuman, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if decided.Status != store.ApprovalStatusApproved || decided.Decision == nil || *decided.Decision != "approve" {
		t.Errorf("decided = %+v, want approved/approve", decided)
	}
	if decided.DecidedBy == nil || *decided.DecidedBy != "key_op" || len(decided.EditedPayload) == 0 {
		t.Errorf("decision fields not recorded: %+v", decided)
	}
	if got := eventTypes(t, s, runID); !containsEvent(got, store.EventApprovalDecided) {
		t.Errorf("events = %v, want approval_decided", got)
	}

	// A second decide on the non-pending row loses the CAS.
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, derr := store.DecideApproval(ctx, q, store.DecideApprovalArgs{
			ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
			Status: store.ApprovalStatusRejected, Decision: "reject",
			DecidedBy: "key_op2", Source: store.ApprovalSourceHuman, Now: testNow,
		})
		return derr
	})
	var notPending *store.ApprovalNotPendingError
	if !errors.As(err, &notPending) {
		t.Fatalf("second decide err = %v, want *ApprovalNotPendingError", err)
	}
	if notPending.Status != store.ApprovalStatusApproved {
		t.Errorf("not-pending status = %q, want approved", notPending.Status)
	}
}

// TestSucceedAwaitingHumanStep: settles a parked gate awaiting_human →
// succeeded, closes the open attempt succeeded, bumps steps_succeeded, and
// appends step_succeeded.
func TestSucceedAwaitingHumanStep(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, _ := parkGate(t, s)

	output := json.RawMessage(`{"decision":"approve","payload":{"text":"x"}}`)
	var settled gen.RunStep
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		settled, err = store.SucceedAwaitingHumanStep(ctx, q, store.SucceedAwaitingHumanStepArgs{
			RunID: runID, StepID: "gate", Output: output, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("SucceedAwaitingHumanStep: %v", err)
	}
	if settled.Status != store.StepStatusSucceeded {
		t.Errorf("step status = %q, want succeeded", settled.Status)
	}
	run, _ := s.Runs().Get(ctx, runID)
	if run.StepsSucceeded != 1 {
		t.Errorf("steps_succeeded = %d, want 1", run.StepsSucceeded)
	}
	attempts, _ := s.Attempts().ListByRun(ctx, runID)
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.StepStatusSucceeded {
		t.Errorf("attempt outcome = %v, want succeeded", attempts)
	}
}

// TestDeadLetterAwaitingHumanStep: settles a rejected gate awaiting_human →
// dead_lettered with a permanent attempt outcome and a permanent DLQ record.
func TestDeadLetterAwaitingHumanStep(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, _ := parkGate(t, s)

	errPayload := json.RawMessage(`{"message":"approval_rejected: no","class":"permanent"}`)
	var settled gen.RunStep
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		settled, err = store.DeadLetterAwaitingHumanStep(ctx, q, store.DeadLetterAwaitingHumanStepArgs{
			RunID: runID, StepID: "gate", Error: errPayload, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("DeadLetterAwaitingHumanStep: %v", err)
	}
	if settled.Status != store.StepStatusDeadLettered {
		t.Errorf("step status = %q, want dead_lettered", settled.Status)
	}
	dls, _ := s.DeadLetters().ListByRun(ctx, runID)
	if len(dls) != 1 || dls[0].Source != store.DeadLetterSourcePermanent {
		t.Fatalf("dead letters = %+v, want one permanent", dls)
	}
	attempts, _ := s.Attempts().ListByRun(ctx, runID)
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.AttemptOutcomePermanent {
		t.Errorf("attempt outcome = %v, want permanent", attempts)
	}
}
