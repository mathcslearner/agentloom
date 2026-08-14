package store

import (
	"context"
	"encoding/json"

	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// RetrievalDocRepo reads and writes retrieval_docs rows (ticket 8.8): the
// corpus the pg_fulltext retriever (internal/retrieval/pgfts) ingests and
// ranks. This is a plain corpus store — no CAS transitions, no events —
// so it exposes ordinary CRUD, unlike the run/step repositories whose
// mutations all route through transitions.go.
type RetrievalDocRepo interface {
	// Upsert adds or updates one document keyed by id (re-ingesting the
	// same id replaces its content/metadata). Nil metadata is stored as the
	// empty object so the NOT NULL column is satisfied.
	Upsert(ctx context.Context, id, content string, metadata json.RawMessage) error
	// Query returns the documents matching the full-text query, ranked most
	// relevant first, capped at limit. An empty or no-match query returns no
	// rows and no error.
	Query(ctx context.Context, query string, limit int32) ([]gen.QueryRetrievalDocsRow, error)
	// Count returns the corpus size (a test/diagnostic aid).
	Count(ctx context.Context) (int64, error)
}

type retrievalDocRepo struct{ q *gen.Queries }

func (r retrievalDocRepo) Upsert(ctx context.Context, id, content string, metadata json.RawMessage) error {
	if metadata == nil {
		metadata = emptyJSON
	}
	err := r.q.UpsertRetrievalDoc(ctx, gen.UpsertRetrievalDocParams{ID: id, Content: content, Metadata: metadata})
	return wrapErr("upsert retrieval doc", err)
}

func (r retrievalDocRepo) Query(ctx context.Context, query string, limit int32) ([]gen.QueryRetrievalDocsRow, error) {
	rows, err := r.q.QueryRetrievalDocs(ctx, gen.QueryRetrievalDocsParams{Query: query, RowLimit: limit})
	return rows, wrapErr("query retrieval docs", err)
}

func (r retrievalDocRepo) Count(ctx context.Context) (int64, error) {
	n, err := r.q.CountRetrievalDocs(ctx)
	return n, wrapErr("count retrieval docs", err)
}
