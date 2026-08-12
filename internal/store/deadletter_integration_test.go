//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 5.4's store-level suite: the dead-letter transitions
// (DeadLetterStep / PoisonDeadLetterStep), the write-off pair
// (CancelStep / ReviveStep), the requeue pair (RequeueStep / ResumeRun),
// the all-terminal failure rollup, and the requeue-baseline budget
// derivation across a die → requeue → die cycle.

func deadLetterStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, claim uuid.UUID, source, outcome string, errJSON json.RawMessage) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.DeadLetterStep(ctx, q, store.DeadLetterStepArgs{
			RunID: runID, StepID: stepID, ClaimID: claim,
			Source: source, Outcome: outcome, Error: errJSON, Now: testNow,
		})
		return err
	})
	return step, err
}

func poisonDeadLetterStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, errJSON, payload json.RawMessage) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.PoisonDeadLetterStep(ctx, q, store.PoisonDeadLetterStepArgs{
			RunID: runID, StepID: stepID, Error: errJSON, Payload: payload, Now: testNow,
		})
		return err
	})
	return step, err
}

func requeueStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.RequeueStep(ctx, q, store.RequeueStepArgs{RunID: runID, StepID: stepID, Now: testNow})
		return err
	})
	return step, err
}

// TestDeadLetterStepRecordsFullContext: the fenced terminal path — status,
// counters, attempt outcome, the dead_letters row, and the event, all in
// one transaction; a stale claim is fenced and the source vocabulary is
// enforced up front.
func TestDeadLetterStepRecordsFullContext(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))
	claimed := mustClaim(t, s, run.ID, "b")
	errJSON := json.RawMessage(`{"message": "boom", "class": "transient"}`)

	// Bad source and stale claim reject before anything lands.
	if _, err := deadLetterStep(t, s, run.ID, "b", *claimed.ClaimID,
		store.DeadLetterSourcePoison, store.AttemptOutcomeTransient, nil); err == nil ||
		!strings.Contains(err.Error(), "not a fenced dead-letter source") {
		t.Errorf("poison source on the fenced path: %v, want source rejection", err)
	}
	_, err := deadLetterStep(t, s, run.ID, "b", uuid.New(),
		store.DeadLetterSourceRetriesExhausted, store.AttemptOutcomeTransient, errJSON)
	wantConflict(t, err, store.ConflictClaimMismatch)

	step, err := deadLetterStep(t, s, run.ID, "b", *claimed.ClaimID,
		store.DeadLetterSourceRetriesExhausted, store.AttemptOutcomeTransient, errJSON)
	if err != nil {
		t.Fatalf("DeadLetterStep: %v", err)
	}
	if step.Status != store.StepStatusDeadLettered || step.FinishedAt == nil {
		t.Errorf("step = status %q finished %v, want dead_lettered with finished_at", step.Status, step.FinishedAt)
	}
	got, err := s.Runs().Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if got.StepsFailed != 1 {
		t.Errorf("steps_failed = %d, want 1 — dead_lettered is the terminal failure counter", got.StepsFailed)
	}
	attempts, err := s.Attempts().ListByStep(ctx, run.ID, "b")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.AttemptOutcomeTransient {
		t.Errorf("attempts = %+v, want one closed with the judged class transient", attempts)
	}
	// The death moved the requeue baseline past the counted transient.
	if got := countCounted(t, s, run.ID, "b"); got != 0 {
		t.Errorf("counted failures after dead-letter = %d, want 0 (baseline)", got)
	}
	dls, err := s.DeadLetters().ListByStep(ctx, run.ID, "b")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 {
		t.Fatalf("dead_letters rows = %d, want 1", len(dls))
	}
	dl := dls[0]
	if dl.Seq != 1 || dl.Source != store.DeadLetterSourceRetriesExhausted ||
		dl.Class == nil || *dl.Class != store.AttemptOutcomeTransient || dl.AttemptsAtDeath != 1 {
		t.Errorf("dead letter = %+v, want seq 1, retries_exhausted, class transient, attempts 1", dl)
	}
	if !jsonEqual(t, dl.Error, errJSON) {
		t.Errorf("dead letter error = %s, want %s", dl.Error, errJSON)
	}
	types := eventTypes(t, s, run.ID)
	found := false
	for _, typ := range types {
		if typ == store.EventStepDeadLettered {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %v, want a step_dead_lettered", types)
	}
}

// TestPoisonDeadLetterStep: the unfenced path accepts every non-terminal
// status, closes a running holder's dangling attempt as lost, preserves
// the raw payload, and rejects terminal steps typed.
func TestPoisonDeadLetterStep(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	payload := json.RawMessage(`{"run_id": "raw", "v": "1"}`)

	t.Run("running holder loses its fence and attempt", func(t *testing.T) {
		t.Parallel()
		run := instantiate(t, s, decodeDef(t, twoEntrySteps))
		claimed := mustClaim(t, s, run.ID, "a")
		step, err := poisonDeadLetterStep(t, s, run.ID, "a", nil, payload)
		if err != nil {
			t.Fatalf("PoisonDeadLetterStep: %v", err)
		}
		if step.Status != store.StepStatusDeadLettered || step.ClaimID != nil {
			t.Errorf("step = status %q claim %v, want dead_lettered with the claim cleared", step.Status, step.ClaimID)
		}
		attempts, err := s.Attempts().ListByStep(ctx, run.ID, "a")
		if err != nil {
			t.Fatalf("listing attempts: %v", err)
		}
		if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.AttemptOutcomeLost {
			t.Errorf("attempts = %+v, want the dangling attempt closed lost", attempts)
		}
		// The displaced holder's completion is fenced off the cleared claim.
		_, err = succeedStep(t, s, run.ID, "a", *claimed.ClaimID, nil)
		wantConflict(t, err, store.ConflictWrongStatus)
		dls, err := s.DeadLetters().ListByStep(ctx, run.ID, "a")
		if err != nil || len(dls) != 1 {
			t.Fatalf("dead letters = %v (err %v), want 1 row", dls, err)
		}
		if dls[0].Source != store.DeadLetterSourcePoison || dls[0].Class != nil {
			t.Errorf("dead letter = %+v, want source poison with NULL class", dls[0])
		}
		if !jsonEqual(t, dls[0].Payload, payload) {
			t.Errorf("payload = %s, want the raw envelope %s", dls[0].Payload, payload)
		}
	})

	t.Run("ready step dead-letters with zero attempts", func(t *testing.T) {
		t.Parallel()
		run := instantiate(t, s, decodeDef(t, twoEntrySteps))
		step, err := poisonDeadLetterStep(t, s, run.ID, "b", nil, payload)
		if err != nil {
			t.Fatalf("PoisonDeadLetterStep on ready: %v", err)
		}
		if step.Status != store.StepStatusDeadLettered || step.AttemptCount != 0 {
			t.Errorf("step = status %q attempts %d, want dead_lettered/0", step.Status, step.AttemptCount)
		}
		dls, err := s.DeadLetters().ListByStep(ctx, run.ID, "b")
		if err != nil || len(dls) != 1 || dls[0].AttemptsAtDeath != 0 {
			t.Errorf("dead letters = %v (err %v), want one row with attempts_at_death 0", dls, err)
		}
	})

	t.Run("terminal step rejects typed", func(t *testing.T) {
		t.Parallel()
		run := instantiate(t, s, decodeDef(t, twoEntrySteps))
		claimed := mustClaim(t, s, run.ID, "a")
		mustSucceed(t, s, run.ID, "a", *claimed.ClaimID)
		_, err := poisonDeadLetterStep(t, s, run.ID, "a", nil, nil)
		te := conflictError(t, err, store.ConflictWrongStatus)
		if te.From != store.StepStatusSucceeded {
			t.Errorf("conflict From = %q, want succeeded", te.From)
		}
	})
}

// TestRequeueRoundTripAndBaseline: die → requeue → die again. The second
// death gets seq 2, the budget re-arms from the baseline, steps_failed
// un-bumps on requeue, and the failed run resumes.
func TestRequeueRoundTripAndBaseline(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	// Death 1: two counted failures, then exhausted → dead-lettered.
	claimed := mustClaim(t, s, run.ID, "a")
	if _, err := retryStepAt(t, s, run.ID, *claimed.ClaimID, store.AttemptOutcomeTransient, nil,
		testNow.Add(time.Second), testNow); err != nil {
		t.Fatalf("RetryStep: %v", err)
	}
	claimed, err := claimStepAt(t, s, run.ID, "a", testNow.Add(time.Second))
	if err != nil {
		t.Fatalf("due claim: %v", err)
	}
	if _, err := deadLetterStep(t, s, run.ID, "a", *claimed.ClaimID,
		store.DeadLetterSourceRetriesExhausted, store.AttemptOutcomeTransient, nil); err != nil {
		t.Fatalf("DeadLetterStep: %v", err)
	}
	if _, err := failRun(t, s, run.ID); err != nil {
		t.Fatalf("FailRun: %v", err)
	}
	if got := countCounted(t, s, run.ID, "a"); got != 0 {
		t.Errorf("counted failures after death 1 = %d, want 0 (baseline moved)", got)
	}

	// Requeue: step back to ready with schedule state cleared,
	// steps_failed un-bumped, run resumed.
	step, err := requeueStep(t, s, run.ID, "a")
	if err != nil {
		t.Fatalf("RequeueStep: %v", err)
	}
	if step.Status != store.StepStatusReady || step.Error != nil ||
		step.NextAttemptAt != nil || step.FinishedAt != nil {
		t.Errorf("requeued step = %+v, want ready with error/schedule/finish cleared", step)
	}
	got, err := s.Runs().Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if got.StepsFailed != 0 {
		t.Errorf("steps_failed after requeue = %d, want 0", got.StepsFailed)
	}
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.ResumeRun(ctx, q, store.ResumeRunArgs{RunID: run.ID, Now: testNow})
		return err
	}); err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	got, err = s.Runs().Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("re-reading run: %v", err)
	}
	if got.Status != store.RunStatusRunning || got.FinishedAt != nil {
		t.Errorf("run after resume = status %q finished %v, want running with finished_at cleared", got.Status, got.FinishedAt)
	}

	// Death 2: attempt 3 claims (the baseline re-armed the claim path),
	// dead-letters again → seq 2, attempts_at_death 3.
	claimed = mustClaim(t, s, run.ID, "a")
	if claimed.AttemptCount != 3 {
		t.Errorf("attempt_count after requeue claim = %d, want 3 (history immutable)", claimed.AttemptCount)
	}
	if _, err := deadLetterStep(t, s, run.ID, "a", *claimed.ClaimID,
		store.DeadLetterSourcePermanent, store.AttemptOutcomePermanent, nil); err != nil {
		t.Fatalf("second DeadLetterStep: %v", err)
	}
	dls, err := s.DeadLetters().ListByStep(ctx, run.ID, "a")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 2 || dls[1].Seq != 2 || dls[1].AttemptsAtDeath != 3 {
		t.Errorf("dead letters = %+v, want seq 1 and seq 2 with attempts_at_death 3", dls)
	}
	if got := countCounted(t, s, run.ID, "a"); got != 0 {
		t.Errorf("counted failures after death 2 = %d, want 0 (baseline is the max)", got)
	}

	// A double requeue conflicts typed once the step is ready again.
	if _, err := requeueStep(t, s, run.ID, "a"); err != nil {
		t.Fatalf("second RequeueStep: %v", err)
	}
	_, err = requeueStep(t, s, run.ID, "a")
	wantConflict(t, err, store.ConflictWrongStatus)
}

// TestCancelReviveAndFailRunRollup: the write-off pair maintains
// steps_cancelled symmetrically, and the all-terminal rollup fires only
// when every step is accounted for with at least one terminal failure.
func TestCancelReviveAndFailRunRollup(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	// b hangs off a so it instantiates pending — CancelStep's from-status.
	const linear = `{
		"schema_version": 1,
		"name": "cancel-rollup",
		"steps": [{"id": "a", "type": "noop"}, {"id": "b", "type": "noop"}],
		"edges": [{"from": "a", "to": "b"}]
	}`
	run := instantiate(t, s, decodeDef(t, linear))

	failRunRollup := func() (gen.Run, error) {
		var got gen.Run
		err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			var err error
			got, err = store.FailRunRollup(ctx, q, store.FailRunArgs{RunID: run.ID, Now: testNow})
			return err
		})
		return got, err
	}

	// Nothing failed yet: guard rejects.
	_, err := failRunRollup()
	wantConflict(t, err, store.ConflictGuardFailed)

	// a dead-letters; b still live → still guarded.
	claimed := mustClaim(t, s, run.ID, "a")
	if _, err := deadLetterStep(t, s, run.ID, "a", *claimed.ClaimID,
		store.DeadLetterSourcePermanent, store.AttemptOutcomePermanent, nil); err != nil {
		t.Fatalf("DeadLetterStep: %v", err)
	}
	_, err = failRunRollup()
	wantConflict(t, err, store.ConflictGuardFailed)

	// b cancels (write-off): all steps terminal → rollup fires.
	if err := cancelStep(t, s, run.ID, "b"); err != nil {
		t.Fatalf("CancelStep: %v", err)
	}
	got, err := s.Runs().Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if got.StepsCancelled != 1 {
		t.Errorf("steps_cancelled = %d, want 1", got.StepsCancelled)
	}
	rolled, err := failRunRollup()
	if err != nil {
		t.Fatalf("FailRunRollup: %v", err)
	}
	if rolled.Status != store.RunStatusFailed {
		t.Errorf("run = %q, want failed", rolled.Status)
	}

	// Revive undoes the write-off symmetrically.
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.ReviveStep(ctx, q, store.ReviveStepArgs{
			RunID: run.ID, StepID: "b", Reason: store.OutboxReasonDLQRequeue, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("ReviveStep: %v", err)
	}
	b, err := s.Steps().Get(ctx, run.ID, "b")
	if err != nil {
		t.Fatalf("reading step b: %v", err)
	}
	if b.Status != store.StepStatusPending {
		t.Errorf("revived step = %q, want pending", b.Status)
	}
	got, err = s.Runs().Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("re-reading run: %v", err)
	}
	if got.StepsCancelled != 0 {
		t.Errorf("steps_cancelled after revive = %d, want 0", got.StepsCancelled)
	}
}

// TestOnFailureMaterialized: instantiation freezes the workflow failure
// policy onto the run row — explicit values verbatim, absent means
// fail_fast.
func TestOnFailureMaterialized(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))

	run := instantiate(t, s, decodeDef(t, twoEntrySteps))
	if run.OnFailure != "fail_fast" {
		t.Errorf("absent on_failure materialized as %q, want fail_fast", run.OnFailure)
	}

	const continueDef = `{
		"schema_version": 1,
		"name": "continue-policy",
		"on_failure": "continue_independent_branches",
		"steps": [{"id": "a", "type": "noop"}],
		"edges": []
	}`
	run = instantiate(t, s, decodeDef(t, continueDef))
	if run.OnFailure != "continue_independent_branches" {
		t.Errorf("on_failure materialized as %q, want continue_independent_branches", run.OnFailure)
	}
}
