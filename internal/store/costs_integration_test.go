//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 10.2: ApplyAttemptCost writes a cost_ledger row and folds it into the
// run aggregate atomically, and appends the cost_unknown_model event when a
// fallback-priced attempt carries a warning. The materialized runs aggregate
// always equals the exact sum of the rows.

func TestApplyAttemptCostAggregatesAndEvents(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	rate := json.RawMessage(`{"input_per_mtok":1,"output_per_mtok":2}`)
	warning := json.RawMessage(`{"model":"acme:unpriced","fallback":{"input_per_mtok":30,"output_per_mtok":60}}`)

	// Two priced rows across two steps, one carrying a fallback warning, plus
	// a cache-hit row that spends 0 and saves.
	rows := []store.AttemptCostArgs{
		{
			RunID: run.ID, StepID: "a", Attempt: 1, Resource: "mock:sim-1", Rate: rate,
			RateSource: "wildcard", CostNanoUSD: 2_000_000, Now: testNow,
		},
		{
			RunID: run.ID, StepID: "b", Attempt: 1, Resource: "acme:unpriced", Rate: rate,
			RateSource: "fallback", CostNanoUSD: 5_000_000, Warning: warning, Now: testNow,
		},
		{
			RunID: run.ID, StepID: "a", Attempt: 2, Resource: "mock:sim-1", Rate: rate,
			RateSource: "wildcard", CacheHit: true, SavedNanoUSD: 1_500_000, Now: testNow,
		},
	}
	for _, args := range rows {
		if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			return store.ApplyAttemptCost(ctx, q, args)
		}); err != nil {
			t.Fatalf("ApplyAttemptCost(%s/%d): %v", args.StepID, args.Attempt, err)
		}
	}

	// The materialized aggregate equals the exact ledger sum.
	got, err := s.Runs().Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	sum, err := s.Ledger().SumByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("SumByRun: %v", err)
	}
	if got.SpentNanoUsd != 7_000_000 || got.SavedNanoUsd != 1_500_000 {
		t.Errorf("run aggregate = {spent %d, saved %d}, want {7000000, 1500000}", got.SpentNanoUsd, got.SavedNanoUsd)
	}
	if got.SpentNanoUsd != sum.SpentNanoUsd || got.SavedNanoUsd != sum.SavedNanoUsd {
		t.Errorf("run aggregate {%d,%d} != ledger sum {%d,%d}", got.SpentNanoUsd, got.SavedNanoUsd, sum.SpentNanoUsd, sum.SavedNanoUsd)
	}

	// Three rows total; entry defaulted to attempt.
	ledger, err := s.Ledger().ListByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(ledger) != 3 {
		t.Fatalf("ledger rows = %d, want 3", len(ledger))
	}
	for _, r := range ledger {
		if r.Entry != store.EntryAttempt {
			t.Errorf("row %s/%d entry = %q, want attempt", r.StepID, r.Attempt, r.Entry)
		}
	}

	// The fallback row's warning landed as a cost_unknown_model event.
	events, err := s.Events().List(ctx, run.ID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var warned int
	for _, e := range events {
		if e.Type == store.EventCostUnknownModel {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("cost_unknown_model events = %d, want 1", warned)
	}
}

func TestApplyAttemptCostRequiresTx(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))
	// Called on the pool (no WithTx marker) → ErrNoTx.
	err := store.ApplyAttemptCost(context.Background(), s, store.AttemptCostArgs{
		RunID: run.ID, StepID: "a", Attempt: 1, Resource: "mock:sim-1",
		Rate: json.RawMessage(`{"input_per_mtok":1,"output_per_mtok":2}`), Now: testNow,
	})
	if !errors.Is(err, store.ErrNoTx) {
		t.Errorf("ApplyAttemptCost off-transaction: %v, want ErrNoTx", err)
	}
}
