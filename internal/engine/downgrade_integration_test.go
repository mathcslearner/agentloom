//go:build integration

package engine_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// The 10.4 downgrade catalog prices mock:expensive an order of magnitude
// above mock:cheap, so a downgrade both fires (the expensive model pushes the
// run toward its budget) and is observable in the ledger (the served model is
// priced at its own rate). The estimate the middleware budgets around is
// output-dominated (1000 max_tokens), so:
//
//	expensive: 1000 output tok * $100/MTok = 100_000_000 nano = $0.10
//	cheap:     1000 output tok *   $2/MTok =   2_000_000 nano = $0.002
const (
	expensiveStepNano = 100_000_000
	cheapStepNano     = 2_000_000
)

const downgradeCatalog = `{
	"schema_version": 1,
	"models": [
		{"name": "mock:expensive", "effective_from": "2025-01-01", "input_per_mtok": 0.0, "output_per_mtok": 100.0},
		{"name": "mock:cheap", "effective_from": "2025-01-01", "input_per_mtok": 0.0, "output_per_mtok": 2.0}
	],
	"fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}
}`

// downgradeChain builds start(noop) → gen0 → … → gen(n-1), a sequential chain
// of mock llm steps whose primary model is "mock/expensive" with a single
// "mock/cheap" fallback at the given soft threshold (threshold < 0 means no
// threshold — projection trigger only).
func downgradeChain(t *testing.T, n int, budgetUSD, threshold float64) string {
	t.Helper()
	fb := `{"model":"mock/cheap"}`
	if threshold >= 0 {
		fb = fmt.Sprintf(`{"model":"mock/cheap","at_budget_fraction":%g}`, threshold)
	}
	var steps, edges strings.Builder
	steps.WriteString(`{"id":"start","type":"noop"}`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&steps, `,{"id":"gen%d","type":"llm","config":{"model":"mock/expensive","prompt":"budget step prompt","max_tokens":1000,"model_fallbacks":[`+fb+`]}}`, i)
		from := "start"
		if i > 0 {
			from = fmt.Sprintf("gen%d", i-1)
		}
		fmt.Fprintf(&edges, `%s{"from":"%s","to":"gen%d"}`, commaIf(i > 0), from, i)
	}
	return fmt.Sprintf(`{
		"schema_version": 1,
		"name": "downgrade-chain",
		"budget_usd": %g,
		"on_budget_exceeded": "park",
		"steps": [%s],
		"edges": [%s]
	}`, budgetUSD, steps.String(), edges.String())
}

// ledgerResources returns each step's ledger resource (the model that actually
// served the attempt), keyed by step id, for the latest attempt of each step.
func ledgerResources(t *testing.T, s *store.Store, runID uuid.UUID) map[string]string {
	t.Helper()
	rows, err := s.Ledger().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.StepID] = r.Resource // rows are ordered by step, attempt; last wins
	}
	return out
}

// stepOutputModel reads the model recorded in a succeeded llm step's output.
func stepOutputModel(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) string {
	t.Helper()
	step, err := s.Steps().Get(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("Get %s: %v", stepID, err)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(step.Output, &out); err != nil {
		t.Fatalf("unmarshal %s output: %v", stepID, err)
	}
	return out.Model
}

// downgradeEvents returns the model_downgraded event payloads in the run feed.
func downgradeEvents(t *testing.T, s *store.Store, runID uuid.UUID) []store.ModelDowngradedEvent {
	t.Helper()
	events, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var out []store.ModelDowngradedEvent
	for _, e := range events {
		if e.Type != store.EventModelDowngraded {
			continue
		}
		var ev store.ModelDowngradedEvent
		if err := json.Unmarshal(e.Payload, &ev); err != nil {
			t.Fatalf("unmarshal model_downgraded: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// TestDowngradeAtThresholdPricesActualModel is 10.4's headline: an
// expensive-model chain crosses its soft threshold mid-run and downgrades to
// the cheap model; the ledger prices each attempt at the model that actually
// served it, and a model_downgraded event carries the from/to models and the
// threshold trigger.
func TestDowngradeAtThresholdPricesActualModel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// budget = $0.25. gen0 expensive (spent 0, frac 0 → run expensive $0.10).
	// gen1: spent 0.10, frac 0.4 < 0.5, projection 0.20 < 0.25 fits → expensive.
	// gen2: spent 0.20, frac 0.8 >= 0.5 → downgrade to cheap. gen3/gen4 cheap.
	s, h, _ := newDowngradeFixture(t)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, downgradeChain(t, 5, 0.25, 0.5)), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	res0 := ledgerResources(t, s, runID)
	wantRes := map[string]string{
		"gen0": "mock:expensive", "gen1": "mock:expensive",
		"gen2": "mock:cheap", "gen3": "mock:cheap", "gen4": "mock:cheap",
	}
	for step, want := range wantRes {
		if res0[step] != want {
			t.Errorf("%s ledger resource = %q, want %q", step, res0[step], want)
		}
		if m := stepOutputModel(t, s, runID, step); m != strings.TrimPrefix(want, "mock:") {
			t.Errorf("%s output model = %q, want %q", step, m, strings.TrimPrefix(want, "mock:"))
		}
	}

	// Exactly the three cheap steps carry a downgrade event; each is a
	// threshold trigger from expensive to cheap.
	evs := downgradeEvents(t, s, runID)
	if len(evs) != 3 {
		t.Fatalf("model_downgraded events = %d, want 3 (gen2/3/4)", len(evs))
	}
	for _, ev := range evs {
		if ev.FromModel != "mock/expensive" || ev.ToModel != "mock/cheap" {
			t.Errorf("downgrade %s→%s, want mock/expensive→mock/cheap", ev.FromModel, ev.ToModel)
		}
		if ev.FromResource != "mock:expensive" || ev.ToResource != "mock:cheap" {
			t.Errorf("downgrade resources %s→%s", ev.FromResource, ev.ToResource)
		}
		if ev.Trigger != store.DowngradeTriggerThreshold {
			t.Errorf("trigger = %q, want %q", ev.Trigger, store.DowngradeTriggerThreshold)
		}
	}

	// Final spend equals the exact ledger sum: 2 expensive + 3 cheap.
	final, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	want := int64(2*expensiveStepNano + 3*cheapStepNano)
	if final.SpentNanoUsd != want {
		t.Errorf("final spent = %d, want %d", final.SpentNanoUsd, want)
	}
	sum, err := s.Ledger().SumByRun(ctx, runID)
	if err != nil {
		t.Fatalf("SumByRun: %v", err)
	}
	if final.SpentNanoUsd != sum.SpentNanoUsd {
		t.Errorf("run aggregate %d != ledger sum %d", final.SpentNanoUsd, sum.SpentNanoUsd)
	}
}

// TestDowngradeProjectionTrigger: a budget too tight for even the first
// expensive step forces an immediate downgrade to the cheap model (the hard
// projection trigger, no threshold crossed), and the run completes on cheap.
func TestDowngradeProjectionTrigger(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// budget = $0.05 < the $0.10 expensive estimate, so gen0 cannot run
	// expensive; cheap ($0.002) fits. No threshold on the fallback.
	s, h, _ := newDowngradeFixture(t)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, downgradeChain(t, 2, 0.05, -1)), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	for _, step := range []string{"gen0", "gen1"} {
		if r := ledgerResources(t, s, runID)[step]; r != "mock:cheap" {
			t.Errorf("%s ledger resource = %q, want mock:cheap", step, r)
		}
	}
	evs := downgradeEvents(t, s, runID)
	if len(evs) != 2 {
		t.Fatalf("model_downgraded events = %d, want 2", len(evs))
	}
	for _, ev := range evs {
		if ev.Trigger != store.DowngradeTriggerProjection {
			t.Errorf("trigger = %q, want %q", ev.Trigger, store.DowngradeTriggerProjection)
		}
		if ev.Limit != store.BudgetLimitRun {
			t.Errorf("limit = %q, want run", ev.Limit)
		}
	}
}

// TestDowngradeExhaustedChainParks: when even the cheapest fallback would
// exceed the budget, the chain is exhausted and the run falls through to the
// configured budget action (park); raising the budget and unparking resumes it.
func TestDowngradeExhaustedChainParks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// budget = $0.001 < the $0.002 cheap estimate: neither model fits, so gen0
	// parks (no downgrade recorded — we never route to a model we'd park on).
	s, h, e := newDowngradeFixture(t)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, downgradeChain(t, 2, 0.001, 0.5)), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID

	parked := waitRunStatus(t, s, runID, store.RunStatusParked)
	if parked.ParkReason == nil || *parked.ParkReason != store.ParkReasonBudgetExceeded {
		t.Errorf("park reason = %v, want budget_exceeded", parked.ParkReason)
	}
	if evs := downgradeEvents(t, s, runID); len(evs) != 0 {
		t.Errorf("model_downgraded events = %d, want 0 (nothing fit)", len(evs))
	}
	requireAttemptOutcomes(t, s, runID, "gen0", []string{store.AttemptOutcomeBudgetExceeded})

	// Raise the budget generously and unpark: gen0 now fits at the expensive
	// model (frac low → no downgrade) and the run completes.
	if _, err := e.SetBudget(ctx, runID, 1_000_000_000); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if _, err := e.Unpark(ctx, runID); err != nil {
		t.Fatalf("Unpark: %v", err)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	if r := ledgerResources(t, s, runID)["gen0"]; r != "mock:expensive" {
		t.Errorf("gen0 ledger resource after unpark = %q, want mock:expensive", r)
	}
}

// newDowngradeFixture wires a store, single-worker queue harness, and an engine
// whose pricing catalog distinguishes mock:expensive from mock:cheap, over a
// fixed-usage provider that echoes the served model.
func newDowngradeFixture(t *testing.T) (*store.Store, *queuetest.Harness, *engine.Engine) {
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
	d := startDispatcher(t, s, h.Queue())
	e, err := engine.New(s, reg, "downgrade-worker",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithPricing(cat))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("downgrade-worker", e.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return s, h, e
}
