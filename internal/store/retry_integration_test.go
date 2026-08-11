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

// Ticket 5.2's store-level suite: the RetryStep transition, the widened
// claim CAS (retrying → running once due), the run-status guard, and the
// counted-failure budget derivation.

// twoEntrySteps has two independent entry steps — one to fail the run
// with, one to claim against the run-status guard.
const twoEntrySteps = `{
	"schema_version": 1,
	"name": "retry-store",
	"steps": [
		{"id": "a", "type": "noop"},
		{"id": "b", "type": "noop"}
	],
	"edges": []
}`

// retryStepAt runs RetryStep on step "a" (the only step these tests
// retry) in its own transaction at the given clock.
func retryStepAt(t *testing.T, s *store.Store, runID uuid.UUID, claim uuid.UUID, outcome string, errJSON json.RawMessage, next, now time.Time) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.RetryStep(ctx, q, store.RetryStepArgs{
			RunID: runID, StepID: "a", ClaimID: claim,
			Outcome: outcome, Error: errJSON, NextAttemptAt: next, Now: now,
		})
		return err
	})
	return step, err
}

// claimStepAt is claimStep with an explicit clock — the due-time guard on
// retrying steps is what these tests exercise.
func claimStepAt(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, now time.Time) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: stepID, Now: now})
		return err
	})
	return step, err
}

// takeoverStepAt runs TakeoverStep in its own transaction.
func takeoverStepAt(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, claim uuid.UUID, now time.Time) error {
	t.Helper()
	return s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.TakeoverStep(ctx, q, store.TakeoverStepArgs{
			RunID: runID, StepID: stepID, ClaimID: claim, Now: now,
		})
		return err
	})
}

func countCounted(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) int64 {
	t.Helper()
	n, err := s.Attempts().CountCountedFailures(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("CountCountedFailures: %v", err)
	}
	return n
}

// TestRetryStepLifecycle: running → retrying records the classed attempt
// and the due time without touching the run's failure aggregate, an early
// claim bounces, and the due-time claim re-runs the step with a fresh
// attempt and a cleared next_attempt_at.
func TestRetryStepLifecycle(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	claimed := mustClaim(t, s, run.ID, "a")
	due := testNow.Add(2 * time.Second)
	errJSON := json.RawMessage(`{"message": "rate limited", "class": "transient"}`)

	step, err := retryStepAt(t, s, run.ID, *claimed.ClaimID, store.AttemptOutcomeTransient, errJSON, due, testNow)
	if err != nil {
		t.Fatalf("RetryStep: %v", err)
	}
	if step.Status != store.StepStatusRetrying {
		t.Errorf("status = %q, want retrying", step.Status)
	}
	if step.ClaimID != nil {
		t.Errorf("claim_id = %v, want cleared — a retrying step holds no claim", step.ClaimID)
	}
	if step.NextAttemptAt == nil || !step.NextAttemptAt.Equal(due) {
		t.Errorf("next_attempt_at = %v, want %v", step.NextAttemptAt, due)
	}
	if step.FinishedAt != nil {
		t.Errorf("finished_at = %v, want NULL — retrying is not terminal", step.FinishedAt)
	}

	// The attempt closed with the class; the run's failure aggregate is
	// untouched (the rollup guard must still pass on eventual success).
	attempts, err := s.Attempts().ListByStep(t.Context(), run.ID, "a")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.AttemptOutcomeTransient {
		t.Fatalf("attempts = %+v, want one closed transient", attempts)
	}
	rerun, err := s.Runs().Get(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if rerun.StepsFailed != 0 {
		t.Errorf("steps_failed = %d, want 0 — retry routing must not bump the aggregate", rerun.StepsFailed)
	}
	types := eventTypes(t, s, run.ID)
	if !strings.Contains(strings.Join(types, ","), store.EventStepRetryScheduled) {
		t.Errorf("events = %v, want a step_retry_scheduled", types)
	}

	// Before the due time the widened claim guard refuses; the conflict
	// reads wrong_status from retrying (the engine drops it).
	_, err = claimStepAt(t, s, run.ID, "a", due.Add(-time.Millisecond))
	te := conflictError(t, err, store.ConflictWrongStatus)
	if te.From != store.StepStatusRetrying {
		t.Errorf("early claim conflict From = %q, want retrying", te.From)
	}

	// At the due time the claim admits it: attempt 2, fresh fence, due
	// time cleared.
	reclaimed, err := claimStepAt(t, s, run.ID, "a", due)
	if err != nil {
		t.Fatalf("due-time claim: %v", err)
	}
	if reclaimed.Status != store.StepStatusRunning || reclaimed.AttemptCount != 2 {
		t.Errorf("reclaimed = status %q attempt %d, want running attempt 2", reclaimed.Status, reclaimed.AttemptCount)
	}
	if reclaimed.NextAttemptAt != nil {
		t.Errorf("next_attempt_at = %v after claim, want cleared", reclaimed.NextAttemptAt)
	}
	if reclaimed.ClaimID == nil || *reclaimed.ClaimID == *claimed.ClaimID {
		t.Error("due-time claim must issue a fresh fencing token")
	}
}

// TestRetryStepGuards: the transition is claim-fenced and accepts only
// counted retryable classes.
func TestRetryStepGuards(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))
	claimed := mustClaim(t, s, run.ID, "a")
	due := testNow.Add(time.Second)

	// A stale claim is fenced exactly like the terminal transitions.
	stale := uuid.New()
	_, err := retryStepAt(t, s, run.ID, stale, store.AttemptOutcomeTransient, nil, due, testNow)
	wantConflict(t, err, store.ConflictClaimMismatch)

	// Never-retryable and administrative outcomes are rejected up front.
	for _, outcome := range []string{store.AttemptOutcomePermanent, store.AttemptOutcomeCancelled, store.AttemptOutcomeLost, ""} {
		if _, err := retryStepAt(t, s, run.ID, *claimed.ClaimID, outcome, nil, due, testNow); err == nil ||
			!strings.Contains(err.Error(), "not a counted retryable class") {
			t.Errorf("RetryStep(outcome=%q) error = %v, want counted-class rejection", outcome, err)
		}
	}
	if _, err := retryStepAt(t, s, run.ID, *claimed.ClaimID, store.AttemptOutcomeTransient, nil, time.Time{}, testNow); err == nil ||
		!strings.Contains(err.Error(), "zero NextAttemptAt") {
		t.Errorf("RetryStep(zero due time) error = %v, want zero-NextAttemptAt rejection", err)
	}
}

// TestClaimStepRunStatusGuard: once the run is terminal, new claims are
// refused with the dedicated reason — the mechanism ADR-006 gives 5.2 and
// 5.6's park/cancel reuses.
func TestClaimStepRunStatusGuard(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	// Fail the run through step a.
	claimed := mustClaim(t, s, run.ID, "a")
	if err := failStep(t, s, run.ID, "a", *claimed.ClaimID, nil); err != nil {
		t.Fatalf("FailStep: %v", err)
	}
	if _, err := failRun(t, s, run.ID); err != nil {
		t.Fatalf("FailRun: %v", err)
	}

	// b is still ready, but its run is not running.
	_, err := claimStepAt(t, s, run.ID, "b", testNow)
	te := conflictError(t, err, store.ConflictRunNotRunning)
	if te.From != store.RunStatusFailed {
		t.Errorf("conflict From = %q, want the run's status (failed)", te.From)
	}
	// The guard rejected before any write: no claim, no attempt row.
	b, err := s.Steps().Get(t.Context(), run.ID, "b")
	if err != nil {
		t.Fatalf("reading step b: %v", err)
	}
	if b.Status != store.StepStatusReady || b.AttemptCount != 0 {
		t.Errorf("step b = status %q attempts %d, want untouched ready/0", b.Status, b.AttemptCount)
	}
}

// TestCountCountedFailuresExcludesLost: the retry budget counts judged
// failures only — a lease-expiry takeover's lost closure never spends it.
func TestCountCountedFailuresExcludesLost(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	// Attempt 1: claimed, then taken over → outcome lost.
	claimed := mustClaim(t, s, run.ID, "a")
	if err := takeoverStepAt(t, s, run.ID, "a", *claimed.ClaimID, testNow); err != nil {
		t.Fatalf("TakeoverStep: %v", err)
	}
	if got := countCounted(t, s, run.ID, "a"); got != 0 {
		t.Errorf("counted failures after lost = %d, want 0", got)
	}

	// Attempt 2: claimed, retried transient → counts.
	claimed = mustClaim(t, s, run.ID, "a")
	if _, err := retryStepAt(t, s, run.ID, *claimed.ClaimID, store.AttemptOutcomeTransient, nil,
		testNow.Add(time.Second), testNow); err != nil {
		t.Fatalf("RetryStep: %v", err)
	}
	if got := countCounted(t, s, run.ID, "a"); got != 1 {
		t.Errorf("counted failures = %d, want 1", got)
	}

	// Attempt 3: claimed at the due time, failed terminally with the
	// class recorded → also counts.
	claimed, err := claimStepAt(t, s, run.ID, "a", testNow.Add(time.Second))
	if err != nil {
		t.Fatalf("due-time claim: %v", err)
	}
	if err := failStep(t, s, run.ID, "a", *claimed.ClaimID, nil); err != nil {
		t.Fatalf("FailStep: %v", err)
	}
	// failStep records permanent, which is not a counted class.
	if got := countCounted(t, s, run.ID, "a"); got != 1 {
		t.Errorf("counted failures after permanent = %d, want 1 (permanent is uncounted)", got)
	}
}
