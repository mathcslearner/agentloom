// Package retrieval defines agentloom's retrieval SPI (ticket 8.8,
// ADR-009): the Retriever interface a `retrieve` step queries, the typed
// Registry the exec package wraps the retrieve executor around, and the
// document types they exchange. The reference backend — Postgres
// full-text search — lives in the internal/retrieval/pgfts subpackage,
// deliberately split out so this package stays a leaf.
//
// A retriever is a single, named, self-describing search capability: it
// ingests documents (Ingest) and answers a text query with the top-k
// ranked matches (Query). The `retrieve` step names one retriever and a
// query; the executor (internal/exec/retrieveexec.go) resolves it here,
// runs one Query, and writes the ranked results to the step output for
// downstream templating and CEL.
//
// Like internal/tools and internal/llm this is a leaf package: it imports
// internal/plugin (the manifest vocabulary) and internal/dag (the ADR-006
// error-class vocabulary) and stdlib, never internal/exec, internal/store,
// or the engine. Concrete backends that need a datastore (pgfts) import
// store themselves, keeping this SPI free of any persistence dependency —
// the clean separation the "writing a retriever plugin" walkthrough builds
// on (docs/plugins.md).
package retrieval

import (
	"context"
	"encoding/json"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// Doc is one document a retriever ingests: a caller-chosen stable ID (the
// upsert key — re-ingesting the same ID updates in place), the text
// searched and returned, and optional opaque metadata carried through to
// query results verbatim (source URL, title, tags — whatever the caller
// wants a downstream llm step to cite). ID and Content are required;
// Metadata may be nil (persists and returns as the empty object).
type Doc struct {
	ID       string          `json:"id"`
	Content  string          `json:"content"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// ScoredDoc is a Doc with the retriever's relevance score for a query —
// the shape the `retrieve` step writes into its output's `results` array.
// Higher scores are more relevant; the scale is retriever-specific (pgfts
// reports ts_rank), so scores are meaningful for ordering within one
// result set, not for comparison across retrievers.
type ScoredDoc struct {
	Doc
	Score float64 `json:"score"`
}

// Retriever is one named search capability a `retrieve` step queries
// (ADR-009's retriever kind). Implementations must be safe for concurrent
// Query calls — one Retriever instance serves every step that names it
// across the worker's consumer goroutines — and must return promptly once
// ctx is canceled (the lease heartbeat stops with the handler).
type Retriever interface {
	// Manifest is the retriever's self-description (ADR-009): kind
	// retriever, a unique name (^[a-z][a-z0-9_]*$), a semver version, and
	// capability flags. Retrievers declare no per-retriever config schema
	// (nil ConfigSchema): the `retrieve` step's config shape — retriever,
	// query, top_k — is uniform across retrievers and its schema lives on
	// the retrieve executor, so there is nothing retriever-specific to
	// serve. The registry validates the identity at registration.
	Manifest() plugin.Manifest

	// Ingest adds or updates documents in the retriever's corpus, keyed by
	// Doc.ID (re-ingesting an existing ID updates it). It is not invoked by
	// the `retrieve` step — steps only Query — but by corpus-loading code
	// (tests, seeding, a future ingest API); an empty slice is a no-op.
	// Errors classify per ADR-006 (a *retrieval.Error), though Ingest is
	// off the step execution path so classification is advisory.
	Ingest(ctx context.Context, docs []Doc) error

	// Query returns the k documents most relevant to the query text, most
	// relevant first. k <= 0 is the caller's contract to honor a default
	// (the executor supplies one before calling, so backends may treat a
	// non-positive k as "no limit" or a small default of their own). A
	// query that matches nothing returns an empty slice and no error.
	//
	// Errors classify per ADR-006: return a *retrieval.Error to declare
	// transient (datastore hiccup) or permanent (malformed corpus state),
	// and pass ctx cancellation/deadline through unwrapped so the engine
	// keeps the timeout/cancelled judgment (ADR-006 rows 3/8). An
	// unclassified error defaults to transient at the engine.
	Query(ctx context.Context, query string, k int) ([]ScoredDoc, error)
}
