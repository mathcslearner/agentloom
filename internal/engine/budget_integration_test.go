//go:build integration

package engine_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 10.3's headline: budget enforcement at claim time. A sequential chain
// of mock llm steps under a run budget parks exactly at the first step whose
// projected spend would exceed the cap; raising the budget and unparking
// resumes it to success. The mock returns a fixed usage per call so each
// step's actual cost equals ~2_004_000 nano-USD at the mock:* rate (1.0/2.0
// per MTok), and the estimate (chars/4 input + max_tokens output) is a tight
// upper bound — so the park point is deterministic.

// perStepNano is each mock llm step's actual cost: Cost(0 input, 1000 output)
// at the mock:* rate = 1000 * $2/MTok = 2_000_000 nano-USD.
const perStepNano = 2_000_000

// fixedUsageProvider is a mock-like provider whose every Chat call returns a
// fixed usage — so a step's actual and estimated cost are controllable — and
// counts its calls, so a budget-blocked step can be proven never to have
// called the provider.
type fixedUsageProvider struct {
	in, out int64
	calls   atomic.Int64
}

func (p *fixedUsageProvider) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	p.calls.Add(1)
	return llm.ChatResponse{
		Model: req.Model, StopReason: "end_turn",
		Blocks: []llm.Block{llm.TextBlock("ok")},
		Usage:  llm.Usage{InputTokens: p.in, OutputTokens: p.out},
	}, nil
}

func (*fixedUsageProvider) Manifest() plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindModelProvider, Name: "mock", Version: "1.0.0", Description: "fixed-usage mock"}
}

func fixedUsageRegistry(t *testing.T, p *fixedUsageProvider) *llm.Registry {
	t.Helper()
	reg, err := llm.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// budgetChainDef builds start(noop) → gen0 → … → gen(n-1), a sequential chain
// of mock llm steps each capped at max_tokens=1000, under a run budget.
func budgetChainDef(t *testing.T, n int, budgetUSD float64, onExceeded string) string {
	t.Helper()
	var steps, edges strings.Builder
	steps.WriteString(`{"id":"start","type":"noop"}`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&steps, `,{"id":"gen%d","type":"llm","config":{"model":"mock/sim-1","prompt":"budget step prompt","max_tokens":1000}}`, i)
		from := "start"
		if i > 0 {
			from = fmt.Sprintf("gen%d", i-1)
		}
		fmt.Fprintf(&edges, `%s{"from":"%s","to":"gen%d"}`, commaIf(i > 0), from, i)
	}
	return fmt.Sprintf(`{
		"schema_version": 1,
		"name": "budget-chain",
		"budget_usd": %g,
		"on_budget_exceeded": %q,
		"steps": [`+steps.String()+`],
		"edges": [`+edges.String()+`]
	}`, budgetUSD, onExceeded)
}

// TestBudgetParksWithinOneStepOfCap is 10.3's headline: a budgeted chain parks
// at the first step whose projection exceeds the cap, emits budget_exceeded
// with projection detail, and — after the budget is raised and the run is
// unparked — resumes and completes with every step's cost ledgered.
func TestBudgetParksWithinOneStepOfCap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// budget = $0.005 = 5_000_000 nano. Each step costs 2_000_000; the estimate
	// is a tight upper bound. gen0 (proj ~2M) and gen1 (spent 2M, proj ~4M)
	// proceed; gen2 (spent 4M, proj ~6M > 5M) parks.
	s, h, e := newBudgetFixture(t, &fixedUsageProvider{in: 0, out: 1000})
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, budgetChainDef(t, 5, 0.005, "park")), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID

	parked := waitRunStatus(t, s, runID, store.RunStatusParked)
	if parked.ParkReason == nil || *parked.ParkReason != store.ParkReasonBudgetExceeded {
		t.Errorf("park reason = %v, want budget_exceeded", parked.ParkReason)
	}
	// Spent stays within the cap, and one more step would have exceeded it —
	// the defined tolerance: parks within one step's estimate of the cap.
	if parked.SpentNanoUsd > *parked.BudgetNanoUsd {
		t.Errorf("spent %d exceeded budget %d at park", parked.SpentNanoUsd, *parked.BudgetNanoUsd)
	}
	if parked.SpentNanoUsd+perStepNano <= *parked.BudgetNanoUsd {
		t.Errorf("spent %d + one step %d did not exceed budget %d — parked too early",
			parked.SpentNanoUsd, perStepNano, *parked.BudgetNanoUsd)
	}
	// gen0/gen1 succeeded; gen2 was released with a budget_exceeded attempt;
	// gen3/gen4 never became ready.
	requireStepStatuses(t, s, runID, map[string]string{
		"start": store.StepStatusSucceeded,
		"gen0":  store.StepStatusSucceeded,
		"gen1":  store.StepStatusSucceeded,
		"gen2":  store.StepStatusReady,
		"gen3":  store.StepStatusPending,
		"gen4":  store.StepStatusPending,
	})
	requireAttemptOutcomes(t, s, runID, "gen2", []string{store.AttemptOutcomeBudgetExceeded})
	if !hasEventType(t, s, runID, store.EventBudgetExceeded) {
		t.Errorf("no %q event in the run feed", store.EventBudgetExceeded)
	}

	// Raise the budget and unpark: the run resumes and completes.
	if _, err := e.SetBudget(ctx, runID, 1_000_000_000); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if !hasEventType(t, s, runID, store.EventRunBudgetUpdated) {
		t.Errorf("no %q event after raise", store.EventRunBudgetUpdated)
	}
	if _, err := e.Unpark(ctx, runID); err != nil {
		t.Fatalf("Unpark: %v", err)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// gen2 ran on its second attempt; every step's cost is ledgered.
	requireAttemptOutcomes(t, s, runID, "gen2", []string{store.AttemptOutcomeBudgetExceeded, "succeeded"})
	final, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if want := int64(5 * perStepNano); final.SpentNanoUsd != want {
		t.Errorf("final spent = %d, want %d (5 steps)", final.SpentNanoUsd, want)
	}
	rows, err := s.Ledger().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("ledger has %d rows, want 5", len(rows))
	}
}

// TestBudgetFailPolicyDeadLetters: under on_budget_exceeded=fail the
// over-budget step dead-letters permanently and the run fails (fail_fast).
func TestBudgetFailPolicyDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, _ := newBudgetFixture(t, &fixedUsageProvider{in: 0, out: 1000})
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, budgetChainDef(t, 5, 0.005, "fail")), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireAttemptOutcomes(t, s, runID, "gen2", []string{store.AttemptOutcomePermanent})
	gen2, err := s.Steps().Get(ctx, runID, "gen2")
	if err != nil {
		t.Fatalf("Get gen2: %v", err)
	}
	if gen2.Status != store.StepStatusDeadLettered {
		t.Errorf("gen2 status = %q, want dead_lettered", gen2.Status)
	}
}

// TestBudgetStepMaxTokensNeverCalls: a step whose projected request exceeds
// its budget.max_tokens cap dead-letters permanently before the provider is
// ever called (the oversized-request guard).
func TestBudgetStepMaxTokensNeverCalls(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &fixedUsageProvider{in: 0, out: 1000}
	s, h, _ := newBudgetFixture(t, p)
	// The single llm step's projected request (input estimate + 1000 output)
	// far exceeds a max_tokens cap of 10, so it must never run.
	def := mustDecode(t, `{
		"schema_version": 1,
		"name": "oversized",
		"steps": [{"id":"big","type":"llm","config":{"model":"mock/sim-1","prompt":"budget step prompt","max_tokens":1000},"budget":{"max_tokens":10}}],
		"edges": []
	}`)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{"big": store.StepStatusDeadLettered})
	if n := p.calls.Load(); n != 0 {
		t.Errorf("provider was called %d times; the oversized request must never be sent", n)
	}
}

// newBudgetFixture wires a store, a single-worker queue harness, and an engine
// with pricing (embedded default catalog, mock:* wildcard) over the given
// fixed-usage provider, and returns the engine for control-plane ops.
func newBudgetFixture(t *testing.T, p *fixedUsageProvider) (*store.Store, *queuetest.Harness, *engine.Engine) {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(fixedUsageRegistry(t, p)), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	e, err := engine.New(s, reg, "budget-worker",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithPricing(testCatalogE2E(t)))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("budget-worker", e.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return s, h, e
}
