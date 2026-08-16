//go:build integration

package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/cache/redisstore"
	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// cxSpawnSummarize spawns a worker wired for summarization compaction (ticket
// 12.5): the blackboard, retrievers, the mock summarizer, the pricing catalog
// (so summary overhead ledgers), and an optional response cache (so a repeated
// summarization of the same span is a cache hit).
func cxSpawnSummarize(t *testing.T, s *store.Store, h *queuetest.Harness, d *engine.Dispatcher, id string, cacheStore *redisstore.Store) {
	t.Helper()
	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	providers := mockProviders(t)
	reg := exec.Builtins(providers, nil, retrievers)
	opts := []engine.Option{
		engine.WithDispatchNudge(d.Nudge),
		engine.WithBlackboard(board),
		engine.WithRetrievers(retrievers),
		engine.WithSummarizer(contextmgr.NewLLMSummarizer(providers)),
		engine.WithPricing(testCatalogE2E(t)),
		engine.WithRetryScheduler(h.Delayed()),
	}
	if cacheStore != nil {
		opts = append(opts, engine.WithResponseCache(cacheStore, time.Hour))
	}
	w, err := engine.New(s, reg, id, opts...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn(id, w.Handle, queuetest.LeaseConfig(600*time.Millisecond))
}

// TestContextSummarization is the ticket 12.5 headline (the M12 exit-criterion
// shape): the canonical sixteen-turn conversation with four rollup steps runs
// offline on the mock, every rollup's assembled request stays under its context
// budget (zero provider-overflow by construction), the summarize strategy writes
// chained summaries to the blackboard (parent_version links the chain), and each
// summarization is metered as compaction overhead on its serving step.
func TestContextSummarization(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON := readSummarizationFixture(t)
	s, h, runID := setup(t, defJSON)
	d := startDispatcher(t, s, h.Queue())
	cxSpawnSummarize(t, s, h, d, "worker-a", nil)
	cxSpawnSummarize(t, s, h, d, "worker-b", nil)

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// Chained summaries: the blackboard key context_summary has one version per
	// rollup, each linking to its parent (the chain), and each summary is
	// genuinely smaller than the span it replaced.
	hist, err := s.Blackboard().History(ctx, runID, "context_summary")
	if err != nil {
		t.Fatalf("blackboard history: %v", err)
	}
	if len(hist) != 4 {
		t.Fatalf("context_summary versions = %d, want 4 (one per rollup)", len(hist))
	}
	for i, row := range hist {
		var sv contextmgr.SummaryValue
		if err := json.Unmarshal(row.Value, &sv); err != nil {
			t.Fatalf("version %d value: %v", i+1, err)
		}
		if sv.Text == "" || sv.Resource != "mock:cheap" {
			t.Errorf("version %d SummaryValue = %+v", i+1, sv)
		}
		wantParent := 0
		if i > 0 {
			wantParent = i // parent is the previous version number
		}
		if sv.ParentVersion != wantParent {
			t.Errorf("version %d parent_version = %d, want %d", i+1, sv.ParentVersion, wantParent)
		}
		if len(sv.SpanNames) == 0 || sv.SummaryTokens >= sv.SpanTokens {
			t.Errorf("version %d span=%v summary=%d span_tokens=%d (summary should be smaller)", i+1, sv.SpanNames, sv.SummaryTokens, sv.SpanTokens)
		}
	}

	// Every rollup's assembled request fit its budget, recorded at least one
	// summarization, and reports a synthetic summary source plus a summarized
	// disposition on the folded turns.
	for _, r := range []string{"rollup1", "rollup2", "rollup3", "rollup4"} {
		ev := contextEvent(t, s, runID, r)
		if ev.PreflightTokens > ev.BudgetTokens {
			t.Errorf("%s preflight %d over budget %d", r, ev.PreflightTokens, ev.BudgetTokens)
		}
		if ev.Summaries < 1 {
			t.Errorf("%s summaries = %d, want >= 1", r, ev.Summaries)
		}
		sawSummarized, sawSummary := false, false
		for _, src := range ev.Sources {
			if src.Status == "summarized" {
				sawSummarized = true
			}
			if src.Kind == "summary" {
				sawSummary = true
			}
		}
		if !sawSummarized || !sawSummary {
			t.Errorf("%s dispositions missing summarized/summary source: %+v", r, ev.Sources)
		}
		foundSummarize := false
		for _, rev := range contextRevisions(t, s, runID, r) {
			if rev.Strategy == "summarize" && rev.Changed {
				foundSummarize = true
			}
		}
		if !foundSummarize {
			t.Errorf("%s has no changed summarize revision", r)
		}
	}

	// Summarizer overhead is ledgered on the serving step, flagged overhead,
	// under a compaction:<i> entry, priced at the mock:cheap resource.
	overhead := 0
	for _, row := range ledgerRows(t, s, runID) {
		if strings.HasPrefix(row.Entry, "compaction:") {
			overhead++
			if !row.Overhead || row.Resource != "mock:cheap" || row.CostNanoUsd <= 0 {
				t.Errorf("compaction ledger row malformed: %+v", row)
			}
		}
	}
	if overhead != 4 {
		t.Errorf("compaction overhead rows = %d, want 4 (one per rollup)", overhead)
	}
}

// TestContextSummarizationCacheHit: two runs of the same conversation share one
// response cache, so the second run's summarizer calls (identical spans, global
// scope) are served from cache — its compaction overhead becomes $0 saved rows,
// no second provider call billed (ADR-011 + ADR-012 rule 4).
func TestContextSummarizationCacheHit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON := readSummarizationFixture(t)
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	cacheStore := newCacheStore(t, h.Client())
	d := startDispatcher(t, s, h.Queue())
	cxSpawnSummarize(t, s, h, d, "worker", cacheStore)

	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	submit := func() uuid.UUID {
		res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		d.Nudge()
		waitRun(t, s, res.Run.ID, store.RunStatusSucceeded)
		return res.Run.ID
	}

	run1 := submit()
	h.WaitQuiescent(ctx)
	// Run 1 populated the cache with real (billed) summaries.
	for _, row := range ledgerRows(t, s, run1) {
		if strings.HasPrefix(row.Entry, "compaction:") && row.CacheHit {
			t.Errorf("run 1 compaction row unexpectedly a cache hit: %+v", row)
		}
	}

	run2 := submit()
	h.WaitQuiescent(ctx)
	// Run 2 summarizes the identical spans (global cache scope) → cache hits:
	// $0 saved rows, no spend.
	hits := 0
	for _, row := range ledgerRows(t, s, run2) {
		if !strings.HasPrefix(row.Entry, "compaction:") {
			continue
		}
		hits++
		if !row.CacheHit || row.CostNanoUsd != 0 || row.SavedNanoUsd <= 0 {
			t.Errorf("run 2 compaction row should be a $0 saved cache hit: %+v", row)
		}
	}
	if hits != 4 {
		t.Errorf("run 2 compaction rows = %d, want 4 cache hits", hits)
	}
}

// TestContextSummarizerFallsBack: an unroutable summarizer model makes the
// summarize strategy fall back to the next deterministic strategy — the step
// still completes under budget, a warning is recorded on the summarize revision,
// and no summary is written to the blackboard.
func TestContextSummarizerFallsBack(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setup(t, summarizerFallbackDef)
	d := startDispatcher(t, s, h.Queue())
	cxSpawnSummarize(t, s, h, d, "worker", nil)

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// No summary was written (the summarizer failed).
	if _, err := s.Blackboard().Head(ctx, runID, "context_summary"); err == nil {
		t.Error("context_summary written despite summarizer failure")
	}
	// The summarize revision fell back (unchanged, with an error), and the
	// deterministic fallback strategy then ran and fit the budget.
	revs := contextRevisions(t, s, runID, "summary")
	if len(revs) < 2 {
		t.Fatalf("revisions = %d, want >= 2 (summarize fallback + deterministic)", len(revs))
	}
	if revs[0].Strategy != "summarize" || revs[0].Changed || revs[0].Error == "" {
		t.Errorf("first revision should be an unchanged summarize fallback with an error: %+v", revs[0])
	}
	ev := contextEvent(t, s, runID, "summary")
	if ev.PreflightTokens > ev.BudgetTokens {
		t.Errorf("preflight %d over budget %d after fallback", ev.PreflightTokens, ev.BudgetTokens)
	}
	// No summarizer billed → no compaction overhead rows.
	for _, row := range ledgerRows(t, s, runID) {
		if strings.HasPrefix(row.Entry, "compaction:") {
			t.Errorf("unexpected compaction overhead row after fallback: %+v", row)
		}
	}
}

// summarizerFallbackDef declares a summarize strategy on an unroutable model
// (mock/does-not-route resolves, but "bogus/model" does not), so the summarizer
// fails and the pipeline falls back to sliding_window.
const summarizerFallbackDef = `{
	"schema_version": 1,
	"name": "summarizer-fallback",
	"steps": [
		{"id": "t1", "type": "llm",
		 "config": {"model": "mock/sim-1", "prompt": "Turn one about the quarterly planning decisions and the north-star metric we picked for the coming half and why activation matters here.", "max_tokens": 128},
		 "blackboard": {"write": [{"key": "turn_1", "from": "/text", "tags": ["turn"]}]}},
		{"id": "t2", "type": "llm",
		 "config": {"model": "mock/sim-1", "prompt": "Turn two about the ingestion pipeline reliability work and the at-least-once queue with idempotency keys we prioritized before features.", "max_tokens": 128},
		 "blackboard": {"write": [{"key": "turn_2", "from": "/text", "tags": ["turn"]}]}},
		{"id": "summary", "type": "llm",
		 "config": {"model": "mock/sim-1", "prompt": "Summarize the decisions.", "max_tokens": 128},
		 "context": {
			"sources": [
				{"kind": "blackboard", "name": "turn_1", "key": "turn_1"},
				{"kind": "blackboard", "name": "turn_2", "key": "turn_2"}
			],
			"budget_tokens": 60,
			"compaction": [
				{"strategy": "summarize", "model": "bogus/unroutable", "key": "context_summary", "max_tokens": 32},
				{"strategy": "sliding_window", "n": 1}
			]
		 }}
	],
	"edges": [{"from": "t1", "to": "t2"}, {"from": "t2", "to": "summary"}]
}`

func readSummarizationFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "context_summarization.json"))
	if err != nil {
		t.Fatalf("reading context_summarization.json: %v", err)
	}
	return string(b)
}

func ledgerRows(t *testing.T, s *store.Store, runID uuid.UUID) []gen.CostLedger {
	t.Helper()
	rows, err := s.Ledger().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	return rows
}
