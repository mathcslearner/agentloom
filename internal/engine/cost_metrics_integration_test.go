//go:build integration

package engine_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 10.5: the cost metrics and cost_updated events. Spend, savings, and
// tokens are recorded post-commit from the ledgered charge; budget parks and
// model downgrades increment their own counters at the claim-time decision;
// and every cost-bearing attempt appends a cost_updated event whose running
// run total is monotonic and equals the aggregate at completion.

// usdEqual asserts a float USD counter equals an integer nano-USD amount,
// within a tiny epsilon for the nano→USD division.
func usdEqual(t *testing.T, got float64, wantNano int64, what string) {
	t.Helper()
	want := float64(wantNano) / 1e9
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("%s = %.12f USD, want %.12f (%d nano)", what, got, want, wantNano)
	}
}

// TestCostMetricsSpentSavedTokens is 10.5's headline: a miss then an identical
// hit. The miss records spend and its billed tokens by resource; the hit
// records the counterfactual saved figure. The metrics match the ledger, and
// the run's cost_updated events carry monotonic totals that finish at the
// aggregate — the M18.4 meter-consistency check, pre-paid.
func TestCostMetricsSpentSavedTokens(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCacheFixtureWithPricing(t)

	run1 := f.runToSuccess(t, cacheDefTemp0(t))
	run2 := f.runToSuccess(t, cacheDefTemp0(t))
	f.h.WaitQuiescent(ctx)

	const resource = "mock:sim-1"
	miss := onlyLedgerRow(t, f.s, run1)
	hit := onlyLedgerRow(t, f.s, run2)

	// Spend and saved counters mirror the ledger exactly.
	usdEqual(t, counterValue(t, f.mreg, "engine_cost_spent_usd_total", map[string]string{"resource": resource}),
		miss.CostNanoUsd, "engine_cost_spent_usd_total")
	usdEqual(t, counterValue(t, f.mreg, "engine_cost_saved_usd_total", map[string]string{"resource": resource}),
		hit.SavedNanoUsd, "engine_cost_saved_usd_total")

	// Token counters record the miss's billed tokens; the hit consumes none.
	mu := mustUsage(t, miss.Usage)
	if got := counterValue(t, f.mreg, "engine_cost_input_tokens_total", map[string]string{"resource": resource}); got != float64(mu.InputTokens) {
		t.Errorf("engine_cost_input_tokens_total = %v, want %d", got, mu.InputTokens)
	}
	if got := counterValue(t, f.mreg, "engine_cost_output_tokens_total", map[string]string{"resource": resource}); got != float64(mu.OutputTokens) {
		t.Errorf("engine_cost_output_tokens_total = %v, want %d", got, mu.OutputTokens)
	}

	// The miss run's cost_updated event carries the run's total, matching the
	// aggregate; the hit run's carries the saved total.
	assertCostUpdatedMatchesAggregate(t, f.s, run1)
	assertCostUpdatedMatchesAggregate(t, f.s, run2)
}

// assertCostUpdatedMatchesAggregate verifies a run's cost_updated events are
// monotonic in run totals and their final values equal the run aggregate — the
// property the M18 live meter relies on (it renders the running total straight
// from the event stream).
func assertCostUpdatedMatchesAggregate(t *testing.T, s *store.Store, runID uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	events, err := s.Events().List(ctx, runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
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
	if len(updates) == 0 {
		t.Fatalf("run %s has no cost_updated events", runID)
	}
	var prevSpent, prevSaved int64
	for i, u := range updates {
		if u.RunSpentNanoUSD < prevSpent || u.RunSavedNanoUSD < prevSaved {
			t.Errorf("cost_updated[%d] totals regressed: {spent %d, saved %d} after {spent %d, saved %d}",
				i, u.RunSpentNanoUSD, u.RunSavedNanoUSD, prevSpent, prevSaved)
		}
		prevSpent, prevSaved = u.RunSpentNanoUSD, u.RunSavedNanoUSD
	}
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if last := updates[len(updates)-1]; last.RunSpentNanoUSD != run.SpentNanoUsd || last.RunSavedNanoUSD != run.SavedNanoUsd {
		t.Errorf("final cost_updated totals {%d,%d} != run aggregate {%d,%d}",
			last.RunSpentNanoUSD, last.RunSavedNanoUSD, run.SpentNanoUsd, run.SavedNanoUsd)
	}
}

// TestCostMetricsBudgetParkCounter proves a budget park increments the
// budget_exceeded counter with limit=run, action=park.
func TestCostMetricsBudgetParkCounter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, _, reg := newMeteredBudgetFixture(t, &fixedUsageProvider{in: 0, out: 1000})

	res, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: mustDecode(t, budgetChainDef(t, 5, 0.005, "park")), Now: testNow,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRunStatus(t, s, runID, store.RunStatusParked)
	h.WaitQuiescent(ctx)

	if got := counterValue(t, reg, "engine_cost_budget_exceeded_total", map[string]string{"limit": store.BudgetLimitRun, "action": "park"}); got != 1 {
		t.Errorf("engine_cost_budget_exceeded_total{limit=run,action=park} = %v, want 1", got)
	}
}

// TestCostMetricsDowngradeCounter proves a threshold downgrade increments the
// downgrades counter with the budget_threshold trigger.
func TestCostMetricsDowngradeCounter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, _, reg := newMeteredDowngradeFixture(t)

	// A 5-step expensive chain under a $0.30 budget with a soft threshold at
	// 0.5: once run spend reaches half the budget, later steps route to cheap.
	res, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: mustDecode(t, downgradeChain(t, 5, 0.30, 0.5)), Now: testNow,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	got := counterValue(t, reg, "engine_cost_downgrades_total", map[string]string{"trigger": store.DowngradeTriggerThreshold})
	if got < 1 {
		t.Errorf("engine_cost_downgrades_total{trigger=budget_threshold} = %v, want ≥ 1", got)
	}
	// A downgrade counter must match the model_downgraded event count.
	if evs := downgradeEvents(t, s, runID); float64(len(evs)) != got {
		t.Errorf("downgrade counter %v != model_downgraded events %d", got, len(evs))
	}
}

// newMeteredBudgetFixture is newBudgetFixture with a Prometheus-backed
// WorkerMetrics, returning the registry for counter assertions.
func newMeteredBudgetFixture(t *testing.T, p *fixedUsageProvider) (*store.Store, *queuetest.Harness, *engine.Engine, *prometheus.Registry) {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(fixedUsageRegistry(t, p)), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	mreg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(mreg)
	d := startDispatcher(t, s, h.Queue())
	e, err := engine.New(s, reg, "budget-worker",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithMetrics(wm),
		engine.WithPricing(testCatalogE2E(t)))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("budget-worker", e.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return s, h, e, mreg
}

// newMeteredDowngradeFixture is newDowngradeFixture with a Prometheus-backed
// WorkerMetrics, returning the registry for counter assertions.
func newMeteredDowngradeFixture(t *testing.T) (*store.Store, *queuetest.Harness, *engine.Engine, *prometheus.Registry) {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	p := &fixedUsageProvider{in: 0, out: 1000}
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(fixedUsageRegistry(t, p)), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	cat, err := cost.Parse([]byte(downgradeCatalog))
	if err != nil {
		t.Fatalf("cost.Parse: %v", err)
	}
	mreg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(mreg)
	d := startDispatcher(t, s, h.Queue())
	e, err := engine.New(s, reg, "downgrade-worker",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithMetrics(wm),
		engine.WithPricing(cat))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("downgrade-worker", e.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return s, h, e, mreg
}
