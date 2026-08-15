//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// TestRecordModelDowngrade appends the model_downgraded event under a live
// claim, rejects a stale claim with a *TransitionError, and requires a
// transaction.
func TestRecordModelDowngrade(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	step, err := claimStep(t, s, run.ID, "a")
	if err != nil {
		t.Fatalf("claimStep: %v", err)
	}
	claim := *step.ClaimID

	event := store.ModelDowngradedEvent{
		FromModel: "mock/expensive", ToModel: "mock/cheap",
		FromResource: "mock:expensive", ToResource: "mock:cheap",
		Trigger: store.DowngradeTriggerThreshold, ThresholdFraction: 0.8,
		SpentNanoUSD: 4_000_000, BudgetNanoUSD: 5_000_000,
	}
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		return store.RecordModelDowngrade(ctx, q, store.RecordModelDowngradeArgs{
			RunID: run.ID, StepID: "a", ClaimID: claim, Event: event, Now: testNow,
		})
	}); err != nil {
		t.Fatalf("RecordModelDowngrade: %v", err)
	}

	events, err := s.Events().List(ctx, run.ID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var seen int
	for _, e := range events {
		if e.Type == store.EventModelDowngraded {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("model_downgraded events = %d, want 1", seen)
	}

	// A stale claim (not the live holder) is fenced.
	err = s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		return store.RecordModelDowngrade(ctx, q, store.RecordModelDowngradeArgs{
			RunID: run.ID, StepID: "a", ClaimID: uuid.New(), Event: event, Now: testNow,
		})
	})
	var te *store.TransitionError
	if !errors.As(err, &te) {
		t.Errorf("stale-claim RecordModelDowngrade = %v, want *TransitionError", err)
	}

	// Off-transaction → ErrNoTx.
	err = store.RecordModelDowngrade(context.Background(), s, store.RecordModelDowngradeArgs{
		RunID: run.ID, StepID: "a", ClaimID: claim, Event: event, Now: testNow,
	})
	if !errors.Is(err, store.ErrNoTx) {
		t.Errorf("RecordModelDowngrade off-transaction = %v, want ErrNoTx", err)
	}
}
