// Package pgfts is the reference retrieval backend (ticket 8.8, ADR-009):
// a retrieval.Retriever over Postgres full-text search (tsvector /
// ts_rank). It is the reference because it needs zero new infrastructure —
// the corpus lives in one table in the same Postgres the engine already
// depends on, so a worker can retrieve without pgvector or any external
// vector store (both documented as follow-on plugins in the backlog).
//
// Unlike the retrieval SPI package it wraps, pgfts imports internal/store
// (it needs a datastore); it is the worked example the "writing a
// retriever plugin" walkthrough (docs/plugins.md) points at — implement
// retrieval.Retriever, declare a manifest, register in cmd/worker.
package pgfts

import (
	"context"
	"errors"

	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/store"
)

// name is the retriever's registered name and the value a `retrieve`
// step's config.retriever selects. The canonical fixtures already use it.
const name = "pg_fulltext"

// version is the retriever's plugin version (ADR-009), 1.0.0 — the real
// backend replacing 8.8's dev stub. It feeds M9's cache keys.
const version = "1.0.0"

// Retriever is the Postgres full-text retrieval backend. It holds a store
// handle; Query and Ingest are safe for concurrent use (the store's pool
// is), satisfying the SPI's concurrency contract.
type Retriever struct {
	st *store.Store
}

// New builds the pg_fulltext retriever over st. The store is the same one
// the engine uses — retrieval adds no new datastore.
func New(st *store.Store) *Retriever {
	return &Retriever{st: st}
}

// Manifest implements retrieval.Retriever (ADR-009): kind retriever, name
// pg_fulltext, cacheable (a query is a pure read of the corpus; ranking is
// deterministic given the corpus), not side-effectful (Query never
// mutates; Ingest is off the step path) and not cost-bearing (no metered
// external call). No config schema — the retrieve step's config shape is
// uniform and lives on the executor.
func (r *Retriever) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindRetriever,
		Name:         name,
		Version:      version,
		Description:  "Postgres full-text (tsvector) retrieval over the ingested corpus.",
		Capabilities: plugin.Capabilities{Cacheable: true},
	}
}

// Ingest upserts each document into the corpus (keyed by ID), so
// re-ingesting the same corpus is idempotent. An empty slice is a no-op.
// Each doc is upserted in its own statement on the pool; a partial failure
// surfaces the error with whatever succeeded already committed — corpus
// loading is not a transaction boundary (callers re-run ingest freely).
func (r *Retriever) Ingest(ctx context.Context, docs []retrieval.Doc) error {
	for _, d := range docs {
		if d.ID == "" {
			return retrieval.Permanentf(name, nil, "document has empty id")
		}
		if d.Content == "" {
			return retrieval.Permanentf(name, nil, "document %q has empty content", d.ID)
		}
		if err := r.st.RetrievalDocs().Upsert(ctx, d.ID, d.Content, d.Metadata); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return retrieval.Transientf(name, err, "ingesting document %q", d.ID)
		}
	}
	return nil
}

// Query returns the top-k documents matching q, ranked by ts_rank
// descending. A non-positive k is treated as k=1 (the executor supplies a
// sensible default before calling, so this is only a floor). A query that
// matches nothing returns an empty slice and no error. A datastore error
// is transient; ctx cancellation/deadline passes through unwrapped so the
// engine judges timeout vs. cancelled (ADR-006 rows 3/8).
func (r *Retriever) Query(ctx context.Context, q string, k int) ([]retrieval.ScoredDoc, error) {
	if k <= 0 {
		k = 1
	}
	rows, err := r.st.RetrievalDocs().Query(ctx, q, int32(k))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, retrieval.Transientf(name, err, "querying corpus")
	}
	out := make([]retrieval.ScoredDoc, 0, len(rows))
	for _, row := range rows {
		out = append(out, retrieval.ScoredDoc{
			Doc: retrieval.Doc{
				ID:       row.ID,
				Content:  row.Content,
				Metadata: row.Metadata,
			},
			Score: row.Score,
		})
	}
	return out, nil
}

// Ensure *Retriever satisfies the SPI at compile time.
var _ retrieval.Retriever = (*Retriever)(nil)
