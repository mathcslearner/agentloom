//go:build integration

package pgfts_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 8.8's store-level acceptance: the reference retriever ingests a
// seeded corpus and ranks it with Postgres full-text search — more query
// term matches rank higher, non-matching docs are excluded, top-k bounds
// the page, re-ingest upserts, and metadata rides through verbatim.

// seedCorpus is a small, deterministic corpus. "cats" appears twice in
// docs[0] and once in docs[1] so ts_rank orders them; docs[2] never
// mentions cats.
var seedCorpus = []retrieval.Doc{
	{ID: "d1", Content: "Cats are wonderful pets and cats love to nap in the sun.", Metadata: json.RawMessage(`{"source":"felines-101"}`)},
	{ID: "d2", Content: "A single cat sat quietly on the mat.", Metadata: json.RawMessage(`{"source":"nursery-rhymes"}`)},
	{ID: "d3", Content: "Dogs are loyal companions who enjoy long walks.", Metadata: json.RawMessage(`{"source":"canines-101"}`)},
}

func newRetriever(t *testing.T) (*pgfts.Retriever, *store.Store) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t))
	return pgfts.New(s), s
}

func TestPGFTSIngestAndRank(t *testing.T) {
	t.Parallel()
	r, _ := newRetriever(t)
	ctx := t.Context()

	if err := r.Ingest(ctx, seedCorpus); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	results, err := r.Query(ctx, "cats", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// d3 (no cats) excluded; d1 (two mentions) ranks above d2 (one).
	if len(results) != 2 {
		t.Fatalf("results = %d (%+v), want 2 matches", len(results), results)
	}
	if results[0].ID != "d1" || results[1].ID != "d2" {
		t.Errorf("ranked order = [%s %s], want [d1 d2] (more matches ranks higher)", results[0].ID, results[1].ID)
	}
	if !(results[0].Score >= results[1].Score) {
		t.Errorf("scores not descending: %v then %v", results[0].Score, results[1].Score)
	}
	if results[0].Score <= 0 {
		t.Errorf("top score = %v, want a positive ts_rank", results[0].Score)
	}
	// Metadata rides through for downstream citation (JSONB normalizes
	// whitespace, so compare the decoded object, not the bytes).
	if got := metaSource(t, results[0].Metadata); got != "felines-101" {
		t.Errorf("d1 metadata source = %q, want felines-101", got)
	}
	// Content is returned so an llm step can ground its answer.
	if results[0].Content == "" {
		t.Error("d1 content is empty, want the ingested text")
	}
}

func TestPGFTSTopKBounds(t *testing.T) {
	t.Parallel()
	r, _ := newRetriever(t)
	ctx := t.Context()
	if err := r.Ingest(ctx, seedCorpus); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	results, err := r.Query(ctx, "cats", 1)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) != 1 || results[0].ID != "d1" {
		t.Errorf("top-1 results = %+v, want just d1 (the best match)", results)
	}
}

func TestPGFTSNoMatchAndEmptyQuery(t *testing.T) {
	t.Parallel()
	r, _ := newRetriever(t)
	ctx := t.Context()
	if err := r.Ingest(ctx, seedCorpus); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	for _, q := range []string{"elephant", "", "   "} {
		results, err := r.Query(ctx, q, 10)
		if err != nil {
			t.Fatalf("Query(%q): %v", q, err)
		}
		if len(results) != 0 {
			t.Errorf("Query(%q) = %d results, want 0", q, len(results))
		}
	}
}

func TestPGFTSReIngestUpserts(t *testing.T) {
	t.Parallel()
	r, s := newRetriever(t)
	ctx := t.Context()
	if err := r.Ingest(ctx, seedCorpus); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Re-ingest d2 with new content and metadata under the same id.
	updated := retrieval.Doc{ID: "d2", Content: "A clever fox jumped over the lazy hound.", Metadata: json.RawMessage(`{"source":"revised"}`)}
	if err := r.Ingest(ctx, []retrieval.Doc{updated}); err != nil {
		t.Fatalf("re-Ingest: %v", err)
	}

	// The corpus size is unchanged — upsert, not insert.
	n, err := s.RetrievalDocs().Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != int64(len(seedCorpus)) {
		t.Errorf("corpus size = %d, want %d (re-ingest upserts)", n, len(seedCorpus))
	}

	// d2 no longer matches "cats" (its content was replaced) but does match "fox".
	cats, err := r.Query(ctx, "cats", 10)
	if err != nil {
		t.Fatalf("Query cats: %v", err)
	}
	for _, d := range cats {
		if d.ID == "d2" {
			t.Error("d2 still matches 'cats' after its content was replaced")
		}
	}
	fox, err := r.Query(ctx, "fox", 10)
	if err != nil {
		t.Fatalf("Query fox: %v", err)
	}
	if len(fox) != 1 || fox[0].ID != "d2" || metaSource(t, fox[0].Metadata) != "revised" {
		t.Errorf("fox results = %+v, want the updated d2", fox)
	}
}

// metaSource decodes the "source" field of a doc's metadata object.
func metaSource(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshaling metadata %s: %v", raw, err)
	}
	return m.Source
}

func TestPGFTSIngestRejectsEmptyFields(t *testing.T) {
	t.Parallel()
	r, _ := newRetriever(t)
	ctx := t.Context()

	for _, d := range []retrieval.Doc{
		{ID: "", Content: "has content"},
		{ID: "x", Content: ""},
	} {
		err := r.Ingest(ctx, []retrieval.Doc{d})
		var re *retrieval.Error
		if !errors.As(err, &re) || re.Class != dag.ClassPermanent {
			t.Errorf("Ingest(%+v) err = %v, want a permanent *retrieval.Error", d, err)
		}
	}
}
