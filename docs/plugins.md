# Plugin SPI

agentloom's capabilities — executors, tools, retrievers, model providers,
and (from M11) validators — are **plugins**: ordinary Go code, compiled
into the binaries, that self-describes through a manifest and registers at
boot. The design is [ADR-009](adr/009-plugin-spi.md); this page is the
practical guide, with a worked "writing a retriever plugin" walkthrough.

The five plugin kinds and their owning packages:

| Kind | Package | Interface |
|---|---|---|
| `executor` | `internal/exec` | `Executor` — runs one step type |
| `tool` | `internal/tools` | `Tool` — one `tool`-step capability |
| `retriever` | `internal/retrieval` | `Retriever` — a `retrieve`-step search backend |
| `model_provider` | `internal/llm` | `Provider` — an LLM chat backend |
| `validator` | `internal/validate` (M11) | — |

Every kind follows the same shape: a leaf interface package that imports
only `internal/plugin` (the manifest vocabulary) and `internal/dag` (the
error-class vocabulary), a **typed registry facade** over the generic
`plugin.Registry` (type-safe lookup + the one shared registration
discipline: an invalid or duplicate registration fails boot), and
self-description via a `plugin.Manifest` (identity, semver version,
capability flags, and — where the config is per-plugin — a JSON Schema
generated from the plugin's own Go struct). The API serves every
registered manifest verbatim on `GET /v1/plugins`.

## Capability flags

Three booleans on every manifest, read by later milestones **without
executing the plugin** ([ADR-009](adr/009-plugin-spi.md#capability-flags)):

- `side_effectful` — performs externally observable effects; its
  executions go through the side-effect journal / idempotency keys (5.5)
  and its outputs are never served from cache.
- `cacheable` — output is a function of (config, input) *and* caching is
  semantically sound (M9's response cache).
- `cost_bearing` — executing it spends money (M10's cost ledger).

Flags describe the plugin's **contract**, and the semver `version` feeds
M9's cache keys: bump it whenever a change should invalidate previously
cached outputs.

## Writing a retriever plugin (walkthrough)

A retriever answers a `retrieve` step's query with the top-k most relevant
documents. The reference implementation, `internal/retrieval/pgfts`, is the
worked example; a new backend (pgvector, an external vector store) is the
same three steps.

**1. Implement `retrieval.Retriever`.** Three methods — a manifest, an
ingest path, and a query path:

```go
package myretriever

import (
	"context"

	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/retrieval"
)

type Retriever struct { /* your datastore handle */ }

func (r *Retriever) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindRetriever,
		Name:         "my_retriever", // ^[a-z][a-z0-9_]*$, unique per kind
		Version:      "1.0.0",        // semver; feeds M9 cache keys
		Description:  "One-line description served on GET /v1/plugins.",
		Capabilities: plugin.Capabilities{Cacheable: true},
		// No ConfigSchema: the retrieve step's config shape (retriever,
		// query, top_k) is uniform and its schema lives on the executor.
	}
}

func (r *Retriever) Ingest(ctx context.Context, docs []retrieval.Doc) error {
	// Upsert each doc keyed by doc.ID so re-ingesting is idempotent.
	// Ingest is NOT on the step path — it is corpus-loading code.
	return nil
}

func (r *Retriever) Query(ctx context.Context, q string, k int) ([]retrieval.ScoredDoc, error) {
	// Return up to k documents, most relevant first. A no-match query is an
	// empty slice and no error. Classify failures for the retry engine:
	//   - datastore hiccup     → retrieval.Transientf(name, err, "...")
	//   - deterministic corpus → retrieval.Permanentf(name, err, "...")
	//   - ctx cancel/deadline  → return the ctx error UNWRAPPED (the engine
	//                            judges timeout vs. cancelled)
	return nil, nil
}
```

The output the `retrieve` executor writes for downstream steps is fixed by
the executor, not the retriever: `{retriever, query, top_k, results}`,
where each result is `{id, content, score, metadata}`. A downstream `llm`
step reads it with templating — `${{ steps.<id>.output.results }}` splices
the ranked array into a prompt (see `examples/definitions/rag_lite.json`).

**2. Register it in the deployables.** Both `cmd/worker` (which executes
retrieve steps) and `cmd/api` (which lists the catalog) build a
`retrieval.Registry`:

```go
retrievers, err := retrieval.NewRegistry(pgfts.New(st), myretriever.New(st))
```

`cmd/worker` hands the registry to `exec.CoreBuiltins(providers, tools,
retrievers)`; `cmd/api` folds `retrievers.Manifests()` into the
`GET /v1/plugins` listing. A registration that returns an invalid manifest,
the wrong kind, or a duplicate name fails boot — the intended behavior.

**3. Select it from a workflow.** A `retrieve` step names the retriever:

```json
{ "id": "search", "type": "retrieve",
  "config": { "retriever": "my_retriever", "query": "${{ run.params.q }}", "top_k": 5 } }
```

A step naming a retriever the fleet did not register dead-letters
`permanent` at claim time — the same path an unregistered executor takes.

That is the whole SPI: implement the interface, register in the
deployables, select from a workflow. The `pg_fulltext` backend in
`internal/retrieval/pgfts` is ~120 lines over Postgres full-text search and
is the reference to read alongside this guide.
