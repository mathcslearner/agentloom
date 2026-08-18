//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/event"
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
	warning := &event.CostUnknownModel{
		Model:    "acme:unpriced",
		Fallback: cost.Rate{InputPerMTok: 30, OutputPerMTok: 60},
	}

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
			_, aerr := store.ApplyAttemptCost(ctx, q, args)
			return aerr
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

	// Ticket 10.5: one cost_updated event per applied row, in seq order, with
	// run totals that never regress and whose final value equals the aggregate.
	var updates []store.CostUpdatedEvent
	for _, e := range events {
		if e.Type != store.EventCostUpdated {
			continue
		}
		var u store.CostUpdatedEvent
		if uerr := json.Unmarshal(e.Payload, &u); uerr != nil {
			t.Fatalf("decoding cost_updated payload: %v", uerr)
		}
		updates = append(updates, u)
	}
	if len(updates) != 3 {
		t.Fatalf("cost_updated events = %d, want 3", len(updates))
	}
	var prevSpent, prevSaved int64
	for i, u := range updates {
		if u.RunSpentNanoUSD < prevSpent || u.RunSavedNanoUSD < prevSaved {
			t.Errorf("cost_updated[%d] totals regressed: {spent %d, saved %d} after {spent %d, saved %d}",
				i, u.RunSpentNanoUSD, u.RunSavedNanoUSD, prevSpent, prevSaved)
		}
		prevSpent, prevSaved = u.RunSpentNanoUSD, u.RunSavedNanoUSD
	}
	if last := updates[len(updates)-1]; last.RunSpentNanoUSD != got.SpentNanoUsd || last.RunSavedNanoUSD != got.SavedNanoUsd {
		t.Errorf("final cost_updated totals {%d,%d} != run aggregate {%d,%d}",
			last.RunSpentNanoUSD, last.RunSavedNanoUSD, got.SpentNanoUsd, got.SavedNanoUsd)
	}
}

func TestApplyAttemptCostRequiresTx(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))
	// Called on the pool (no WithTx marker) → ErrNoTx.
	_, err := store.ApplyAttemptCost(context.Background(), s, store.AttemptCostArgs{
		RunID: run.ID, StepID: "a", Attempt: 1, Resource: "mock:sim-1",
		Rate: json.RawMessage(`{"input_per_mtok":1,"output_per_mtok":2}`), Now: testNow,
	})
	if !errors.Is(err, store.ErrNoTx) {
		t.Errorf("ApplyAttemptCost off-transaction: %v, want ErrNoTx", err)
	}
}
