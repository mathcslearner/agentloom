//go:build integration

package engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 10.2's headline: the cost ledger. Cost-bearing attempts are priced
// against the ADR-012 catalog and their rows written in the same transaction
// as the success CAS, so a run's materialized spent_nano_usd equals the exact
// integer sum of its ledger rows — proven here under concurrent completions,
// against the offline mock provider priced by the embedded mock:* wildcard.

// mockRate is the embedded catalog's mock:* wildcard rate (1.0/2.0 per MTok).
var mockRate = cost.Rate{InputPerMTok: 1.0, OutputPerMTok: 2.0}

// fanoutLLMDef builds a definition where one noop entry fans out to n
// independent mock llm leaves — n cost-bearing completions that race.
func fanoutLLMDef(t *testing.T, n int) *dag.Definition {
	t.Helper()
	var steps, edges strings.Builder
	steps.WriteString(`{"id":"start","type":"noop"}`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&steps, `,{"id":"gen%d","type":"llm","config":{"model":"mock/sim-1","prompt":"draft number %d please expand at length"}}`, i, i)
		fmt.Fprintf(&edges, `%s{"from":"start","to":"gen%d"}`, commaIf(i > 0), i)
	}
	return mustDecode(t, `{
		"schema_version": 1,
		"name": "cost-fanout",
		"steps": [`+steps.String()+`],
		"edges": [`+edges.String()+`]
	}`)
}

func commaIf(b bool) string {
	if b {
		return ","
	}
	return ""
}

// TestCostLedgerExactSumUnderConcurrency is the 10.2 property: a wide fan-out
// of mock llm steps, completed concurrently by a fleet, ends with the run
// aggregate exactly equal to the independent sum of its ledger rows — and to
// the cost hand-computed from each attempt's persisted usage at the mock rate.
func TestCostLedgerExactSumUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const n = 8

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(mockProviders(t)), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	for _, id := range []string{"w-a", "w-b", "w-c", "w-d"} {
		w, werr := engine.New(s, reg, id,
			engine.WithDispatchNudge(d.Nudge),
			engine.WithPricing(testCatalogE2E(t)))
		if werr != nil {
			t.Fatalf("engine.New: %v", werr)
		}
		h.Spawn(id, w.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	}

	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: fanoutLLMDef(t, n), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	sum, err := s.Ledger().SumByRun(ctx, runID)
	if err != nil {
		t.Fatalf("SumByRun: %v", err)
	}
	// The exactness property: materialized aggregate == independent ledger sum.
	if run.SpentNanoUsd != sum.SpentNanoUsd {
		t.Errorf("run.spent_nano_usd = %d, ledger sum = %d — must be exactly equal", run.SpentNanoUsd, sum.SpentNanoUsd)
	}
	// And that sum equals the cost hand-computed from persisted usage.
	entries, err := s.Ledger().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("ledger has %d rows, want %d (one per llm leaf)", len(entries), n)
	}
	var want int64
	for i := 0; i < n; i++ {
		u := mustUsage(t, latestAttempt(t, s, runID, fmt.Sprintf("gen%d", i)).Usage)
		want += cost.Cost(u.InputTokens, u.OutputTokens, mockRate)
	}
	if run.SpentNanoUsd != want {
		t.Errorf("run.spent_nano_usd = %d, hand-computed = %d", run.SpentNanoUsd, want)
	}
	if run.SpentNanoUsd <= 0 {
		t.Errorf("run.spent_nano_usd = %d, want positive spend", run.SpentNanoUsd)
	}
	// Each row is a productive attempt entry, real spend, exact rate source.
	for _, e := range entries {
		if e.Entry != store.EntryAttempt {
			t.Errorf("row %s entry = %q, want attempt", e.StepID, e.Entry)
		}
		if e.CacheHit {
			t.Errorf("row %s marked cache_hit, want a real miss", e.StepID)
		}
		if e.RateSource != "wildcard" {
			t.Errorf("row %s rate_source = %q, want wildcard (mock:*)", e.StepID, e.RateSource)
		}
	}
}

// testCatalogE2E is the embedded default catalog for integration engines.
func testCatalogE2E(t *testing.T) *cost.Catalog {
	t.Helper()
	c, err := cost.Default()
	if err != nil {
		t.Fatalf("cost.Default: %v", err)
	}
	return c
}

// newCacheFixtureWithPricing is newCacheFixture plus a pricing catalog on the
// engine, so cache hits ledger their counterfactual "saved" figure (10.2).
func newCacheFixtureWithPricing(t *testing.T) *cacheFixture {
	t.Helper()
	return newCacheFixture(t, engine.WithPricing(testCatalogE2E(t)))
}

// TestCostLedgerCacheHitSaved is 10.2's cache-hit criterion: two identical
// temperature=0 runs; the second is a cache hit that ledgers $0 spend with the
// counterfactual saved figure equal to the first run's actual spend.
func TestCostLedgerCacheHitSaved(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCacheFixtureWithPricing(t)

	run1 := f.runToSuccess(t, cacheDefTemp0(t))
	run2 := f.runToSuccess(t, cacheDefTemp0(t))
	f.h.WaitQuiescent(ctx)

	e1 := onlyLedgerRow(t, f.s, run1)
	e2 := onlyLedgerRow(t, f.s, run2)

	if e1.CacheHit {
		t.Error("first run (miss) row marked cache_hit, want real spend")
	}
	if e1.CostNanoUsd <= 0 {
		t.Errorf("miss cost = %d, want positive", e1.CostNanoUsd)
	}
	if !e2.CacheHit {
		t.Error("second run (hit) row not marked cache_hit")
	}
	if e2.CostNanoUsd != 0 {
		t.Errorf("hit cost = %d, want 0", e2.CostNanoUsd)
	}
	if e2.SavedNanoUsd != e1.CostNanoUsd {
		t.Errorf("hit saved = %d, want %d (the miss's actual spend)", e2.SavedNanoUsd, e1.CostNanoUsd)
	}
	// The run aggregates: the miss run spent, the hit run saved.
	r1, _ := f.s.Runs().Get(ctx, run1)
	r2, _ := f.s.Runs().Get(ctx, run2)
	if r1.SpentNanoUsd != e1.CostNanoUsd || r1.SavedNanoUsd != 0 {
		t.Errorf("miss run aggregate = {spent %d, saved %d}, want {%d, 0}", r1.SpentNanoUsd, r1.SavedNanoUsd, e1.CostNanoUsd)
	}
	if r2.SpentNanoUsd != 0 || r2.SavedNanoUsd != e1.CostNanoUsd {
		t.Errorf("hit run aggregate = {spent %d, saved %d}, want {0, %d}", r2.SpentNanoUsd, r2.SavedNanoUsd, e1.CostNanoUsd)
	}
}

// TestCostLedgerUnknownModelFallback: a model with no catalog entry is priced
// at the fallback with rate_source fallback, and a cost_unknown_model event is
// appended in the completion transaction.
func TestCostLedgerUnknownModelFallback(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// A catalog that does NOT price mock (only a fallback) — so mock/sim-1
	// resolves to the fallback rate.
	cat := mustParseCatalog(t, `{
		"schema_version": 1,
		"models": [],
		"tools": [],
		"fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}
	}`)

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(mockProviders(t)))
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	w, err := engine.New(s, reg, "cost-worker",
		engine.WithDispatchNudge(d.Nudge), engine.WithPricing(cat))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("cost-worker", w.Handle, queuetest.LeaseConfig(400*time.Millisecond))

	def := mustDecode(t, `{
		"schema_version": 1,
		"name": "unknown-model",
		"steps": [{"id":"gen","type":"llm","config":{"model":"mock/sim-1","prompt":"hello"}}],
		"edges": []
	}`)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	row := onlyLedgerRow(t, s, runID)
	if row.RateSource != "fallback" {
		t.Errorf("rate_source = %q, want fallback", row.RateSource)
	}
	u := mustUsage(t, row.Usage)
	if want := cost.Cost(u.InputTokens, u.OutputTokens, cost.Rate{InputPerMTok: 30, OutputPerMTok: 60}); row.CostNanoUsd != want {
		t.Errorf("cost = %d, want %d (fallback rate)", row.CostNanoUsd, want)
	}
	// The warning event landed in the run's event feed.
	if !hasEventType(t, s, runID, store.EventCostUnknownModel) {
		t.Errorf("no %q event in the run feed", store.EventCostUnknownModel)
	}
}

// onlyLedgerRow reads a run's single cost_ledger row (fails if not exactly 1).
func onlyLedgerRow(t *testing.T, s *store.Store, runID uuid.UUID) gen.CostLedger {
	t.Helper()
	rows, err := s.Ledger().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger has %d rows, want exactly 1", len(rows))
	}
	return rows[0]
}

// hasEventType reports whether the run's event feed carries an event of typ.
func hasEventType(t *testing.T, s *store.Store, runID uuid.UUID, typ string) bool {
	t.Helper()
	events, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// mustParseCatalog parses a catalog doc or fails the test.
func mustParseCatalog(t *testing.T, doc string) *cost.Catalog {
	t.Helper()
	c, err := cost.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("cost.Parse: %v", err)
	}
	return c
}
