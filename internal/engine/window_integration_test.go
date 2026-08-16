//go:build integration

package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
)

// cxSpawnPriced wires a context-capable engine (mock provider, blackboard,
// retrievers) plus the pricing catalog, so the M12.6 provider-window guardrail
// is live: the catalog supplies model context windows (mock:small = 1024,
// mock:* = 1,000,000). providers lets a test substitute a window-enforcing mock.
func cxSpawnPriced(t *testing.T, s *store.Store, h *queuetest.Harness, d *engine.Dispatcher, id string, providers *llm.Registry) {
	t.Helper()
	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	cat, err := cost.Default()
	if err != nil {
		t.Fatalf("cost.Default: %v", err)
	}
	reg := exec.Builtins(providers, nil, retrievers)
	w, err := engine.New(s, reg, id,
		engine.WithDispatchNudge(d.Nudge),
		engine.WithBlackboard(board),
		engine.WithRetrievers(retrievers),
		engine.WithPricing(cat),
		engine.WithRetryScheduler(h.Delayed()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn(id, w.Handle, queuetest.LeaseConfig(400*time.Millisecond))
}

// TestContextWindowAutoCompacts is the ticket 12.6 headline: context_window.json
// declares a `context` block with NO explicit budget_tokens on the small-window
// model mock/small (catalog context_window 1024). The engine defaults the budget
// from the window (window − max_tokens − headroom), compacts the assembly to fit,
// and the request stays within the window — auto-compaction from the window alone.
func TestContextWindowAutoCompacts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "context_window.json"))
	if err != nil {
		t.Fatalf("reading context_window.json: %v", err)
	}
	s, h, runID := setup(t, string(defJSON))
	d := startDispatcher(t, s, h.Queue())
	cxSpawnPriced(t, s, h, d, "worker-a", mockProviders(t))
	cxSpawnPriced(t, s, h, d, "worker-b", mockProviders(t))

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	ev := contextEvent(t, s, runID, "summary")
	// The budget was defaulted from the model context window (no explicit budget).
	if ev.BudgetSource != "window" {
		t.Errorf("budget_source = %q, want window", ev.BudgetSource)
	}
	if ev.ContextWindow != 1024 {
		t.Errorf("context_window = %d, want 1024", ev.ContextWindow)
	}
	// The default budget = window − max_tokens(700) − headroom(64) = 260.
	if ev.BudgetTokens != 1024-700-64 {
		t.Errorf("budget_tokens = %d, want %d (window − max_tokens − headroom)", ev.BudgetTokens, 1024-700-64)
	}
	// Compaction ran (the raw assembly exceeded the defaulted budget).
	if ev.Revisions < 1 {
		t.Errorf("revisions = %d, want >= 1 (auto-compaction from the window default)", ev.Revisions)
	}
	if ev.PreflightTokens > ev.BudgetTokens {
		t.Errorf("preflight %d over budget %d — compaction did not fit", ev.PreflightTokens, ev.BudgetTokens)
	}
	// The hard window guarantee: assembled + max_tokens <= context_window.
	if ev.PreflightTokens+700 > ev.ContextWindow {
		t.Errorf("preflight %d + max_tokens 700 = %d exceeds window %d", ev.PreflightTokens, ev.PreflightTokens+700, ev.ContextWindow)
	}
	// The raw (pre-compaction) assembly was over budget — compaction was real.
	if ev.RawPreflightTokens <= ev.PreflightTokens {
		t.Errorf("raw preflight %d not greater than compacted %d — nothing compacted", ev.RawPreflightTokens, ev.PreflightTokens)
	}
}

// oversizeContextlessDef is a single llm step on the small-window model with a
// large completion bound and no context block — nothing to compact, so the
// hard window guard must fail it before any provider call.
const oversizeContextlessDef = `{
	"schema_version": 1,
	"name": "context-window-oversize",
	"steps": [
		{"id": "big", "type": "llm",
		 "config": {"model": "mock/small", "prompt": "Write an essay.", "max_tokens": 2000}}
	],
	"edges": []
}`

// TestContextWindowExceededDeadLetters: an llm step whose completion bound alone
// exceeds the model context window, with no context block to compact, fails
// permanently before any provider call (ADR-014's hard guarantee) — the "with
// compaction disabled → typed error, no provider call" criterion.
func TestContextWindowExceededDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setup(t, oversizeContextlessDef)
	d := startDispatcher(t, s, h.Queue())
	cxSpawnPriced(t, s, h, d, "worker-a", mockProviders(t))

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{"big": store.StepStatusDeadLettered})
	a := latestAttempt(t, s, runID, "big")
	if a.Outcome == nil || *a.Outcome != "permanent" {
		t.Errorf("outcome = %v, want permanent", a.Outcome)
	}
	if len(a.Usage) != 0 {
		t.Errorf("attempt recorded usage %s, want none (no provider call)", a.Usage)
	}
	if !strings.Contains(string(a.Error), "context_window_exceeded") {
		t.Errorf("error = %s, want a context_window_exceeded failure", a.Error)
	}
	// No context_assembled event: the step has no context block, so the failure
	// is the hard guard, not a compaction over-budget.
	if n := countEvents(t, s, runID, store.EventContextAssembled); n != 0 {
		t.Errorf("context_assembled events = %d, want 0", n)
	}
}

// TestNoProviderContextOverflowByConstruction proves the engine guard fires
// before the provider: the mock is configured to reject any request over the
// same 1024-token window the catalog declares for mock:small. An oversize step
// dead-letters with the ENGINE's typed context_window_exceeded (not the mock's
// provider error), and an in-window compaction fixture completes — so the mock's
// overflow path is never reached (zero provider context-overflow by construction).
func TestNoProviderContextOverflowByConstruction(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	windowedMock := func() *llm.Registry {
		reg, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{ContextWindow: 1024}})
		if err != nil {
			t.Fatalf("NewRegistryFromKeys: %v", err)
		}
		return reg
	}

	// The oversize step: the engine guard must catch it before the windowed mock.
	s, h, runID := setup(t, oversizeContextlessDef)
	d := startDispatcher(t, s, h.Queue())
	cxSpawnPriced(t, s, h, d, "worker-a", windowedMock())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	a := latestAttempt(t, s, runID, "big")
	// The engine's error format ("context_window_exceeded: resource=... window=..."),
	// not the mock provider's ("... exceeds context window ..."): the guard, not
	// the provider, produced the failure.
	if !strings.Contains(string(a.Error), "context_window_exceeded: resource=") {
		t.Errorf("error = %s, want the engine's pre-call guard error (not the provider's overflow)", a.Error)
	}
	if len(a.Usage) != 0 {
		t.Errorf("attempt recorded usage %s, want none (guard fired before the call)", a.Usage)
	}

	// An in-window compaction fixture completes cleanly against the windowed mock:
	// every summary request is compacted below the window, so the mock's overflow
	// path is never taken.
	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "context_window.json"))
	if err != nil {
		t.Fatalf("reading context_window.json: %v", err)
	}
	s2, h2, run2 := setup(t, string(defJSON))
	d2 := startDispatcher(t, s2, h2.Queue())
	cxSpawnPriced(t, s2, h2, d2, "worker-c", windowedMock())
	waitRun(t, s2, run2, store.RunStatusSucceeded)
	h2.WaitQuiescent(ctx)
	// The summary attempt carries usage — the compacted request reached the mock.
	sa := latestAttempt(t, s2, run2, "summary")
	if len(sa.Usage) == 0 {
		t.Errorf("summary attempt recorded no usage, want a completed (compacted) provider call")
	}

	var u exec.Usage
	if err := json.Unmarshal(sa.Usage, &u); err != nil {
		t.Fatalf("unmarshaling summary usage: %v", err)
	}
	if u.InputTokens+u.OutputTokens <= 0 {
		t.Errorf("summary usage = %+v, want a real completion", u)
	}
}

// TestContextWindowUnknownSkipsGuard: a model whose resource has a rate but no
// catalog context window is unguarded (the ADR-010 "unlimited by omission"
// stance) — a would-be-oversize step runs to completion, and its
// context_assembled event records no window and no window-defaulted budget.
func TestContextWindowUnknownSkipsGuard(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// An override catalog: mock:nowindow has a rate but no context_window, so
	// resolveWindow misses and the guard is inert.
	const overrideCat = `{
		"schema_version": 1,
		"models": [
			{"name": "mock:nowindow", "effective_from": "2025-01-01", "input_per_mtok": 1.0, "output_per_mtok": 2.0}
		]
	}`
	cat, err := cost.Parse([]byte(overrideCat))
	if err != nil {
		t.Fatalf("cost.Parse: %v", err)
	}
	base, _ := cost.Default()
	merged := cost.Merge(base, cat)

	const def = `{
		"schema_version": 1,
		"name": "context-window-unknown",
		"steps": [
			{"id": "big", "type": "llm",
			 "config": {"model": "mock/nowindow", "prompt": "Write an essay.", "max_tokens": 2000}}
		],
		"edges": []
	}`
	s, h, runID := setup(t, def)
	d := startDispatcher(t, s, h.Queue())

	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	reg := exec.Builtins(mockProviders(t), nil, nil)
	w, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithBlackboard(board),
		engine.WithPricing(merged),
		engine.WithRetryScheduler(h.Delayed()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", w.Handle, queuetest.LeaseConfig(400*time.Millisecond))

	// max_tokens 2000 exceeds mock:small's 1024, but mock:nowindow has no window,
	// so the guard is inert and the step completes.
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	requireStepStatuses(t, s, runID, map[string]string{"big": store.StepStatusSucceeded})
}
