package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// CostRepo reads the cost ledger (ticket 10.2, ADR-012). Writes go through
// ApplyAttemptCost, a transition-style helper run inside the attempt-
// completion transaction — like the guarded transitions, the ledger insert
// and the run-aggregate bump are one atomic unit, so the materialized
// runs.spent_nano_usd always equals the exact integer sum of the run's
// cost_ledger rows. These read methods serve the cost API (GET
// /v1/runs/{id}/cost) and the exact-sum property test.
type CostRepo interface {
	// ListByRun returns a run's ledger rows ordered by step, attempt, entry.
	ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.CostLedger, error)
	// SumByRun independently sums a run's ledger — the property test compares
	// it against the materialized runs aggregate.
	SumByRun(ctx context.Context, runID uuid.UUID) (gen.SumCostLedgerByRunRow, error)
	// SumByStep is a step's cumulative attributed spend (all attempts) — the
	// claim-time budget check reads it to enforce a step-level max_usd cap
	// (ticket 10.3).
	SumByStep(ctx context.Context, runID uuid.UUID, stepID string) (int64, error)
	// AggregateByStep is the per-step spend/savings breakdown.
	AggregateByStep(ctx context.Context, runID uuid.UUID) ([]gen.AggregateCostByStepRow, error)
	// AggregateByResource is the per-model / per-tool spend/savings breakdown
	// (the resource name is the model or "tool:<name>").
	AggregateByResource(ctx context.Context, runID uuid.UUID) ([]gen.AggregateCostByResourceRow, error)
}

type costRepo struct{ q *gen.Queries }

func (r costRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.CostLedger, error) {
	rows, err := r.q.ListCostLedgerByRun(ctx, runID)
	return rows, wrapErr("list cost ledger", err)
}

func (r costRepo) SumByRun(ctx context.Context, runID uuid.UUID) (gen.SumCostLedgerByRunRow, error) {
	row, err := r.q.SumCostLedgerByRun(ctx, runID)
	return row, wrapErr("sum cost ledger", err)
}

func (r costRepo) SumByStep(ctx context.Context, runID uuid.UUID, stepID string) (int64, error) {
	n, err := r.q.SumCostLedgerByStep(ctx, gen.SumCostLedgerByStepParams{RunID: runID, StepID: stepID})
	return n, wrapErr("sum cost ledger by step", err)
}

func (r costRepo) AggregateByStep(ctx context.Context, runID uuid.UUID) ([]gen.AggregateCostByStepRow, error) {
	rows, err := r.q.AggregateCostByStep(ctx, runID)
	return rows, wrapErr("aggregate cost by step", err)
}

func (r costRepo) AggregateByResource(ctx context.Context, runID uuid.UUID) ([]gen.AggregateCostByResourceRow, error) {
	rows, err := r.q.AggregateCostByResource(ctx, runID)
	return rows, wrapErr("aggregate cost by resource", err)
}

// AttemptCostArgs are the inputs to ApplyAttemptCost: one cost-bearing
// attempt's priced charge, already resolved by the engine's pricing layer
// (internal/cost lives in the engine, not the store). Money is integer
// nano-USD (ADR-012).
type AttemptCostArgs struct {
	RunID   uuid.UUID
	StepID  string
	Attempt int32
	// Entry discriminates the charge kind: EntryAttempt for the productive
	// provider/tool call (the only kind in 10.2); M11/M12 add overhead
	// entries on the same attempt. Empty defaults to EntryAttempt.
	Entry string
	// Resource is the ADR-010/ADR-012 resource name the charge bills to
	// ("mock:sim-1", "tool:paid_search"). Required.
	Resource string
	// Usage is the attempt's token-accounting snapshot (exec.Usage JSON);
	// nil for a priced-tool row (tools have no token dimension).
	Usage json.RawMessage
	// Rate is the resolved price snapshot stored for auditability; required.
	Rate json.RawMessage
	// RateSource is the resolution provenance ("exact"|"wildcard"|"fallback").
	RateSource string
	// CacheHit marks a $0 cache-served row (ADR-011): CostNanoUSD is 0 and
	// SavedNanoUSD carries the counterfactual figure.
	CacheHit bool
	// Overhead flags a judge/summarization charge (ADR-012 rule 4); always
	// false in 10.2.
	Overhead bool
	// CostNanoUSD is the attributed spend; SavedNanoUSD the cache savings.
	CostNanoUSD  int64
	SavedNanoUSD int64
	// Warning, when non-nil, is the cost_unknown_model event payload appended
	// in the same transaction (a fallback-priced attempt, ADR-012); nil means
	// no event. The engine builds it via event.CostUnknownModelFrom.
	Warning *event.CostUnknownModel
	// Now is the injected current time (the row's created_at). Required.
	Now time.Time
}

// EntryAttempt is the cost_ledger.entry value for a productive provider or
// tool call. ADR-012 rule 4 reserves further values ('judge', 'compaction')
// for M11/M12 overhead rows; 11.5 writes judge overhead via JudgeEntry.
const EntryAttempt = "attempt"

// JudgeEntry is the cost_ledger.entry value for one llm_judge validator's
// overhead charge on a step's attempt (ticket 11.5, ADR-012 rule 4). The
// chain index disambiguates two judges on the same attempt so the
// (run, step, attempt, entry) primary key never collides — the same-attempt
// slot migration 0016 reserved for M11. On an errored judge (a billed
// malformed answer under on_error:fail) there is exactly one, keyed "judge:e".
func JudgeEntry(chainIndex int) string {
	return fmt.Sprintf("judge:%d", chainIndex)
}

// JudgeErrorEntry is the entry for a judge that billed a call but then errored
// (a malformed answer under on_error:fail) — a single such row per attempt,
// so it needs no index.
const JudgeErrorEntry = "judge:e"

// CompactionEntry is the cost_ledger.entry value for one summarize compaction
// call's overhead charge on a step's attempt (ticket 12.5, ADR-012 rule 4). The
// pipeline index disambiguates two summarizations on the same attempt so the
// (run, step, attempt, entry) primary key never collides — the same-attempt
// slot migration 0016 reserved for M12.
func CompactionEntry(index int) string {
	return fmt.Sprintf("compaction:%d", index)
}

// CostApplied is the outcome of ApplyAttemptCost: the run's totals after the
// charge (also carried on the cost_updated event) and whether a row was
// written. A nil-charge call (nothing to ledger) never reaches here — the
// engine's priceAttempt returns nil and the caller skips ApplyAttemptCost.
type CostApplied struct {
	RunSpentNanoUSD int64
	RunSavedNanoUSD int64
	BudgetNanoUSD   *int64
}

// ApplyAttemptCost writes one attempt's cost row and folds it into the run
// aggregate, atomically (ticket 10.2, ADR-012). It must be called inside the
// success completion transaction, after the fenced SucceedStep CAS and while
// the run lock that CAS took is held — so a fenced (zombie) completion, which
// never lands the CAS, never reaches here, and the aggregate bump serializes
// with every other run mutation. When Warning is set (a fallback-priced
// unknown model), the cost_unknown_model event is appended under the same
// transaction's monotonic seq.
func ApplyAttemptCost(ctx context.Context, q Querier, args AttemptCostArgs) (CostApplied, error) {
	const op = "apply attempt cost"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return CostApplied{}, err
	}
	if args.Resource == "" {
		return CostApplied{}, fmt.Errorf("store: %s: empty Resource", op)
	}
	if len(args.Rate) == 0 {
		return CostApplied{}, fmt.Errorf("store: %s: empty Rate snapshot", op)
	}
	entry := args.Entry
	if entry == "" {
		entry = EntryAttempt
	}
	if err := gq.InsertCostLedgerEntry(ctx, gen.InsertCostLedgerEntryParams{
		RunID:        args.RunID,
		StepID:       args.StepID,
		Attempt:      args.Attempt,
		Entry:        entry,
		Resource:     args.Resource,
		Usage:        args.Usage,
		Rate:         args.Rate,
		RateSource:   args.RateSource,
		CacheHit:     args.CacheHit,
		Overhead:     args.Overhead,
		CostNanoUsd:  args.CostNanoUSD,
		SavedNanoUsd: args.SavedNanoUSD,
		CreatedAt:    args.Now,
	}); err != nil {
		return CostApplied{}, wrapErr(op+": insert ledger row", err)
	}
	total, err := gq.AddRunCost(ctx, gen.AddRunCostParams{
		RunID: args.RunID, DSpent: args.CostNanoUSD, DSaved: args.SavedNanoUSD,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CostApplied{}, fmt.Errorf("store: %s: bump run cost: run %s: %w", op, args.RunID, ErrNotFound)
	}
	if err != nil {
		return CostApplied{}, wrapErr(op+": bump run cost", err)
	}
	applied := CostApplied{
		RunSpentNanoUSD: total.SpentNanoUsd,
		RunSavedNanoUSD: total.SavedNanoUsd,
		BudgetNanoUSD:   total.BudgetNanoUsd,
	}
	// cost_updated (ticket 10.5): appended under the same run lock and monotonic
	// seq as the bump above, so the totals across a run's events never regress.
	updated := event.CostUpdated{
		StepID:          args.StepID,
		Attempt:         args.Attempt,
		Entry:           entry,
		Resource:        args.Resource,
		CacheHit:        args.CacheHit,
		Overhead:        args.Overhead,
		CostNanoUSD:     args.CostNanoUSD,
		SavedNanoUSD:    args.SavedNanoUSD,
		RunSpentNanoUSD: applied.RunSpentNanoUSD,
		RunSavedNanoUSD: applied.RunSavedNanoUSD,
		BudgetNanoUSD:   applied.BudgetNanoUSD,
	}
	if err := appendEvent(ctx, gq, op, args.RunID, updated); err != nil {
		return CostApplied{}, err
	}
	// The unknown-model warning follows the cost_updated event (higher seq), so
	// a consumer sees the total move before the advisory that priced it.
	if args.Warning != nil {
		if err := appendEvent(ctx, gq, op, args.RunID, *args.Warning); err != nil {
			return CostApplied{}, err
		}
	}
	return applied, nil
}
