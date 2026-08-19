//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// TestClaimStampsWorkerID (ticket 18.3): the claim CAS records the claiming
// worker's consumer name on the attempt row and the step_claimed event, and a
// reclaim + re-claim on a different worker leaves two attempts naming both
// workers — the durable evidence the inspector's claim history renders (DoD-3).
func TestClaimStampsWorkerID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	run := mustCreateRun(t, s, nil)
	if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
		RunID: run.ID, StepID: "s", StepType: "noop",
		Status: store.StepStatusReady, FiredDeps: 1, UpdatedAt: testNow,
	}); err != nil {
		t.Fatalf("seeding step: %v", err)
	}

	claimWith := func(worker string) (gen.RunStep, error) {
		var step gen.RunStep
		err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			var err error
			step, err = store.ClaimStep(ctx, q, store.ClaimStepArgs{
				RunID: run.ID, StepID: "s", Now: testNow, WorkerID: worker,
			})
			return err
		})
		return step, err
	}

	// Worker A claims; the attempt row records worker-alpha.
	claimedA, err := claimWith("worker-alpha")
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	attempts, err := s.Attempts().ListByStep(ctx, run.ID, "s")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].WorkerID == nil || *attempts[0].WorkerID != "worker-alpha" {
		t.Fatalf("attempt 1 worker_id = %+v, want worker-alpha", attempts[0].WorkerID)
	}

	// The step_claimed event carries the worker id too (event-sourced live path).
	if wid := claimedWorkerID(t, s, run.ID); wid != "worker-alpha" {
		t.Fatalf("step_claimed event worker_id = %q, want worker-alpha", wid)
	}

	// A takeover strands A's attempt, then worker B re-claims.
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.TakeoverStep(ctx, q, store.TakeoverStepArgs{
			RunID: run.ID, StepID: "s", ClaimID: *claimedA.ClaimID, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := claimWith("worker-bravo"); err != nil {
		t.Fatalf("claim B: %v", err)
	}

	attempts, err = s.Attempts().ListByStep(ctx, run.ID, "s")
	if err != nil {
		t.Fatalf("listing attempts after re-claim: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	got := map[string]bool{}
	for _, a := range attempts {
		if a.WorkerID != nil {
			got[*a.WorkerID] = true
		}
	}
	if !got["worker-alpha"] || !got["worker-bravo"] {
		t.Errorf("attempt workers = %v, want both worker-alpha and worker-bravo", got)
	}

	// An empty worker id stores NULL (a programmatic claim without identity).
	run2 := mustCreateRun(t, s, nil)
	if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
		RunID: run2.ID, StepID: "s", StepType: "noop",
		Status: store.StepStatusReady, FiredDeps: 1, UpdatedAt: testNow,
	}); err != nil {
		t.Fatalf("seeding step 2: %v", err)
	}
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: run2.ID, StepID: "s", Now: testNow})
		return err
	}); err != nil {
		t.Fatalf("claim without worker id: %v", err)
	}
	a2, err := s.Attempts().ListByStep(ctx, run2.ID, "s")
	if err != nil {
		t.Fatalf("listing attempts run2: %v", err)
	}
	if len(a2) != 1 || a2[0].WorkerID != nil {
		t.Errorf("attempt with no worker id = %+v, want NULL worker_id", a2[0].WorkerID)
	}
}

// claimedWorkerID reads the worker_id off the run's step_claimed event payload.
func claimedWorkerID(t *testing.T, s *store.Store, runID uuid.UUID) string {
	t.Helper()
	evs, err := s.Events().List(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	for _, ev := range evs {
		if ev.Type != store.EventStepClaimed {
			continue
		}
		var p struct {
			WorkerID string `json:"worker_id"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decoding step_claimed payload: %v", err)
		}
		return p.WorkerID
	}
	return ""
}
