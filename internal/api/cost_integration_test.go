//go:build integration

package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/tools"
)

// Ticket 10.2's cost API contract test: the canonical fanout fixture runs to
// completion on a priced fleet, and GET /v1/runs/{id}/cost returns accurate
// per-step and per-resource (per-model) breakdowns while GET /v1/runs/{id}
// carries the matching cost summary. fanout.json has two mock/sim-1 llm steps
// (priced by the embedded mock:* wildcard) and one json_transform tool step
// (unpriced → free, no ledger row).
func TestRunCostBreakdown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	rootKey := mintTestKey(t)
	handler, err := api.New(s, time.Now, nil, rootKey, api.RateLimitOptions{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{
		Interval: 10 * time.Millisecond, Batch: 16,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	dctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); d.Run(dctx) }()

	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	toolReg, err := tools.NewBuiltins(tools.HTTPOptions{})
	if err != nil {
		t.Fatalf("tools.NewBuiltins: %v", err)
	}
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	cat, err := cost.Default()
	if err != nil {
		t.Fatalf("cost.Default: %v", err)
	}
	for _, name := range []string{"worker-a", "worker-b"} {
		eng, err := engine.New(s, exec.Builtins(providers, toolReg, retrievers), name,
			engine.WithDispatchNudge(d.Nudge), engine.WithPricing(cat))
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		h.Spawn(name, eng.Handle, queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1})
	}

	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, rootKey, submitBody(t, fanoutJSON(t), `{"topic": "cost tracking"}`), &sub); status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}

	// Poll to completion.
	deadline := time.Now().Add(15 * time.Second)
	var run api.RunResponse
	for {
		if getJSON(t, srv, rootKey, "/v1/runs/"+sub.RunID, &run) != http.StatusOK {
			t.Fatal("GET run failed mid-watch")
		}
		if run.Run.Status != store.RunStatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never finished:\n%+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Run.Status != store.RunStatusSucceeded {
		t.Fatalf("run status = %q, want succeeded", run.Run.Status)
	}

	// The run view carries a non-zero cost summary.
	if run.Run.Cost.SpentNanoUSD <= 0 {
		t.Errorf("run view cost.spent_nano_usd = %d, want positive", run.Run.Cost.SpentNanoUSD)
	}
	if run.Run.Cost.SpentUSD == "" || run.Run.Cost.SpentUSD == "0" {
		t.Errorf("run view cost.spent_usd = %q, want a non-zero USD string", run.Run.Cost.SpentUSD)
	}

	// The cost endpoint's full breakdown.
	var costResp api.RunCostResponse
	if status := getJSON(t, srv, rootKey, "/v1/runs/"+sub.RunID+"/cost", &costResp); status != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s/cost = %d, want 200", sub.RunID, status)
	}
	// Summary equals the run-view summary (both the materialized aggregate).
	if costResp.Summary.SpentNanoUSD != run.Run.Cost.SpentNanoUSD {
		t.Errorf("cost summary spent = %d, run view spent = %d", costResp.Summary.SpentNanoUSD, run.Run.Cost.SpentNanoUSD)
	}
	// Two llm attempts ledger, the unpriced json_transform tool does not.
	if len(costResp.Entries) != 2 {
		t.Fatalf("cost entries = %d, want 2 (two priced llm steps; json_transform is free)", len(costResp.Entries))
	}

	// Per-step breakdown: exactly brainstorm and synthesize, each with spend.
	byStep := map[string]api.CostByStepView{}
	for _, s := range costResp.ByStep {
		byStep[s.StepID] = s
	}
	for _, id := range []string{"brainstorm", "synthesize"} {
		v, ok := byStep[id]
		if !ok {
			t.Errorf("by_step missing %q", id)
			continue
		}
		if v.SpentNanoUSD <= 0 {
			t.Errorf("by_step %q spent = %d, want positive", id, v.SpentNanoUSD)
		}
	}
	if _, leaked := byStep["fetch_news"]; leaked {
		t.Error("by_step contains fetch_news — the unpriced json_transform tool should not ledger")
	}

	// Per-model breakdown: one mock:sim-1 resource row aggregating both calls.
	var mockRow *api.CostByResourceView
	for i := range costResp.ByResource {
		if costResp.ByResource[i].Resource == "mock:sim-1" {
			mockRow = &costResp.ByResource[i]
		}
	}
	if mockRow == nil {
		t.Fatalf("by_resource missing mock:sim-1; got %+v", costResp.ByResource)
	}
	if mockRow.Entries != 2 {
		t.Errorf("mock:sim-1 entries = %d, want 2", mockRow.Entries)
	}
	if mockRow.InputTokens <= 0 || mockRow.OutputTokens <= 0 {
		t.Errorf("mock:sim-1 tokens = (%d in, %d out), want positive", mockRow.InputTokens, mockRow.OutputTokens)
	}
	if mockRow.SpentNanoUSD != costResp.Summary.SpentNanoUSD {
		t.Errorf("mock:sim-1 spend %d != summary spend %d (it is the only priced resource)", mockRow.SpentNanoUSD, costResp.Summary.SpentNanoUSD)
	}

	// A run with no cost-bearing work still answers with a zero summary.
	var missing api.RunCostResponse
	other := mustSubmitEmptyRun(t, srv, rootKey)
	if status := getJSON(t, srv, rootKey, "/v1/runs/"+other+"/cost", &missing); status != http.StatusOK {
		t.Fatalf("GET cost for a fresh run = %d, want 200", status)
	}
	if missing.Summary.SpentNanoUSD != 0 || len(missing.Entries) != 0 {
		t.Errorf("fresh-run cost = %+v, want zero summary and no entries", missing.Summary)
	}

	// Unknown run → 404.
	if status := getJSON(t, srv, rootKey, "/v1/runs/"+uuidStr()+"/cost", nil); status != http.StatusNotFound {
		t.Errorf("GET cost for unknown run = %d, want 404", status)
	}
}

// mustSubmitEmptyRun submits a single-noop definition and returns its id — a
// run with no cost-bearing steps.
func mustSubmitEmptyRun(t *testing.T, srv *httptest.Server, bearer string) string {
	t.Helper()
	def := []byte(`{"schema_version":1,"name":"noop-only","steps":[{"id":"only","type":"noop"}],"edges":[]}`)
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, bearer, submitBody(t, def, ""), &sub); status != http.StatusCreated {
		t.Fatalf("POST empty run = %d, want 201", status)
	}
	return sub.RunID
}

// uuidStr returns a fresh random UUID string for the not-found probe.
func uuidStr() string {
	return "00000000-0000-4000-8000-000000000000"
}
