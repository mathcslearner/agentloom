//go:build integration

package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 8.8's headline e2e: the canonical rag_lite.json runs through the
// PRODUCTION retrieve and llm executors — a pg_fulltext retrieve step over
// a seeded corpus feeds its ranked results into a mock-backed llm step via
// 8.2 templating, and the answer text carries the retrieved document ids
// (the citation proof). Fully offline: local Postgres full-text search +
// the deterministic mock provider, zero network.

// ragCorpus is a tiny seeded corpus for the retrieval e2e; "turtles"
// appears in exactly one document so the retrieve step returns it and the
// mock answer echoes its id.
var ragCorpus = []retrieval.Doc{
	{ID: "sea-turtles", Content: "Sea turtles are ancient reptiles that migrate thousands of miles across oceans.", Metadata: json.RawMessage(`{"source":"marine-life"}`)},
	{ID: "tortoises", Content: "Tortoises are land-dwelling turtles known for their remarkable longevity.", Metadata: json.RawMessage(`{"source":"reptiles"}`)},
	{ID: "penguins", Content: "Penguins are flightless birds that thrive in cold southern climates.", Metadata: json.RawMessage(`{"source":"birds"}`)},
}

// ragRegistry builds the executor registry for the retrieval e2e: the
// production retrieve executor over a pg_fulltext retriever on s, and the
// production llm executor over the offline mock provider.
func ragRegistry(t *testing.T, s *store.Store) *exec.Registry {
	t.Helper()
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(
		exec.NewRetrieveExecutor(retrievers),
		exec.NewLLMExecutor(mockProviders(t)),
	)
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	return reg
}

// TestRAGLitePipelineCompletes is the ticket 8.8 headline: rag_lite.json
// runs to success, the retrieve step records its ranked results in the
// documented output shape, and the downstream llm step's answer text
// embeds the retrieved document ids — retrieve → llm data flow end-to-end.
func TestRAGLitePipelineCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "rag_lite.json"))
	if err != nil {
		t.Fatalf("reading rag_lite.json: %v", err)
	}
	// The query ANDs its significant terms (websearch_to_tsquery), so the
	// question uses just "turtles" — which stems to a term both turtle docs
	// contain and the penguin doc does not.
	s, h, runID := setupWithParams(t, string(defJSON), json.RawMessage(`{"question": "turtles"}`))

	// Seed the corpus before the run starts (Ingest is off the step path).
	if err := pgfts.New(s).Ingest(ctx, ragCorpus); err != nil {
		t.Fatalf("seeding corpus: %v", err)
	}

	d := startDispatcher(t, s, h.Queue())
	spawn := func(id string) {
		w, werr := engine.New(s, ragRegistry(t, s), id, engine.WithDispatchNudge(d.Nudge))
		if werr != nil {
			t.Fatalf("engine.New: %v", werr)
		}
		h.Spawn(id, w.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	}
	spawn("worker-a")
	spawn("worker-b")

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"search": store.StepStatusSucceeded,
		"answer": store.StepStatusSucceeded,
	})

	// The retrieve step output follows the documented shape and ranks the
	// two turtle documents above (and to the exclusion of) the penguin one.
	var search struct {
		Retriever string `json:"retriever"`
		Query     string `json:"query"`
		TopK      int    `json:"top_k"`
		Results   []struct {
			ID      string  `json:"id"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	unmarshalStepOutput(t, s, runID, "search", &search)
	if search.Retriever != "pg_fulltext" || search.TopK != 3 {
		t.Errorf("search output header = %+v, want pg_fulltext / top_k 3", search)
	}
	if len(search.Results) != 2 {
		t.Fatalf("search returned %d results (%+v), want the 2 turtle docs", len(search.Results), search.Results)
	}
	gotIDs := map[string]bool{}
	for _, r := range search.Results {
		gotIDs[r.ID] = true
	}
	if !gotIDs["sea-turtles"] || !gotIDs["tortoises"] || gotIDs["penguins"] {
		t.Errorf("result ids = %v, want the two turtle docs and not penguins", gotIDs)
	}

	// The llm answer embeds the retrieved ids (the mock echoes the prompt,
	// into which templating spliced the results) — the "consumed by an llm
	// step, with citations" acceptance.
	var answer struct {
		Text string `json:"text"`
	}
	unmarshalStepOutput(t, s, runID, "answer", &answer)
	for _, id := range []string{"sea-turtles", "tortoises"} {
		if !strings.Contains(answer.Text, id) {
			t.Errorf("answer text does not cite %q: %s", id, answer.Text)
		}
	}
}

// TestRetrieveUnknownRetrieverDeadLetters pins the failure path: a retrieve
// step naming a retriever the fleet does not register fails permanent and
// dead-letters (source permanent) without any partial success.
func TestRetrieveUnknownRetrieverDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const def = `{
		"schema_version": 1,
		"name": "retrieve-unknown",
		"on_failure": "fail_fast",
		"steps": [
			{"id": "search", "type": "retrieve",
			 "config": {"retriever": "nonexistent", "query": "anything"}},
			{"id": "after", "type": "noop"}
		],
		"edges": [{"from": "search", "to": "after"}]
	}`

	s, h, runID := setup(t, def)
	if err := pgfts.New(s).Ingest(ctx, ragCorpus); err != nil {
		t.Fatalf("seeding corpus: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())

	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewRetrieveExecutor(retrievers), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{
		"search": store.StepStatusDeadLettered, "after": store.StepStatusPending,
	})
	dls, err := s.DeadLetters().ListByStep(ctx, runID, "search")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 || dls[0].Source != store.DeadLetterSourcePermanent {
		t.Fatalf("dead letters = %+v, want one with source permanent", dls)
	}
	requireAttemptOutcomes(t, s, runID, "search", []string{store.AttemptOutcomePermanent})
}
