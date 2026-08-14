package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/retrieval"
)

// retrieveVersion is the production retrieve executor's plugin version
// (ADR-009). It replaces the dev stub's 0.1.0-stub in place; the bump to
// 1.0.0 is the behavior change M9's response cache keys on (a real ranked
// result must never read a cache entry keyed on the stub's empty list).
// The capability flag is unchanged from the stub (cacheable) — a query is
// a pure read of the corpus.
const retrieveVersion = "1.0.0"

// defaultTopK is the number of results a retrieve step returns when its
// config omits top_k (0 = absent, since the field is an int). Small by
// design: retrieval feeds an llm step's context window, and a handful of
// the best matches is the RAG-lite default.
const defaultTopK = 5

// maxTopK caps a retrieve step's requested result count, so a large or
// corrupt top_k cannot ask the datastore for an unbounded page.
const maxTopK = 100

// RetrieveExecutor runs retrieve steps (ticket 8.8): it decodes the
// (already 8.2-rendered) RetrieveConfig, looks the named retriever up in
// the retriever registry, runs exactly one Query, and writes the ranked
// results to the step output for downstream templating/CEL. It never
// retries — the M5 engine owns retry, informed by the ADR-006 class the
// retriever declares.
type RetrieveExecutor struct {
	retrievers *retrieval.Registry
}

// NewRetrieveExecutor builds the retrieve executor over a retriever
// registry. A nil registry is valid — a worker that runs no retrieve steps
// needs none — and any retrieve step then fails permanent at lookup with a
// diagnosable "no retrievers configured" error, mirroring the llm and tool
// executors' unconfigured paths.
func NewRetrieveExecutor(reg *retrieval.Registry) RetrieveExecutor {
	return RetrieveExecutor{retrievers: reg}
}

// Type implements Executor.
func (RetrieveExecutor) Type() string { return string(dag.StepRetrieve) }

// PluginManifest implements SelfDescribing (ADR-009). cacheable: a query
// is a pure read of the corpus and its ranking is deterministic given the
// corpus; not side-effectful (Query never mutates) and not cost-bearing
// (no metered external call — the reference backend is local Postgres).
func (RetrieveExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepRetrieve, retrieveVersion,
		"A retrieval query through the retriever SPI.",
		plugin.Capabilities{Cacheable: true})
}

// retrieveOutput is the documented shape a retrieve step writes: the
// resolved retriever and query, the effective top_k, and the ranked
// results. `results` is always present (an empty array on no match, never
// null) so a downstream `${{ steps.<id>.output.results }}` never misses.
type retrieveOutput struct {
	Retriever string                `json:"retriever"`
	Query     string                `json:"query"`
	TopK      int                   `json:"top_k"`
	Results   []retrieval.ScoredDoc `json:"results"`
}

// Execute runs one retrieve attempt. Config, lookup, and top_k failures
// are deterministic (permanent); the retriever's own errors carry an
// ADR-006 class honored through ClassifiedError; context
// cancellation/deadline pass through unwrapped so the engine judges
// timeout vs. cancelled.
func (e RetrieveExecutor) Execute(ctx context.Context, sc StepContext) (Output, error) {
	cfg, err := configAs[*dag.RetrieveConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if cfg == nil || cfg.Retriever == "" {
		// Config was validated at submit time (1.3 requires retriever); a
		// miss here means corrupt stored state or version skew — permanent.
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "retriever")}
	}
	if cfg.Query == "" {
		// query is required at submit time too, but a template could render
		// to empty at runtime; that is deterministic, so permanent.
		return Output{}, Permanentf("retrieve step %q: query rendered empty", cfg.Retriever)
	}
	if cfg.TopK < 0 {
		return Output{}, Permanentf("retrieve step %q: top_k must not be negative, got %d", cfg.Retriever, cfg.TopK)
	}

	if e.retrievers == nil {
		// A worker with no retriever registry can never run this step —
		// deterministic, so permanent, not a retry.
		return Output{}, Permanentf("no retrievers configured for retriever %q", cfg.Retriever)
	}

	topK := cfg.TopK
	if topK == 0 {
		topK = defaultTopK
	}
	if topK > maxTopK {
		topK = maxTopK
	}

	r, err := e.retrievers.Get(cfg.Retriever)
	if err != nil {
		// Unknown retriever is deterministic — no retry conjures one.
		return Output{}, Permanentf("resolving retriever %q: %w", cfg.Retriever, err)
	}

	results, err := r.Query(ctx, cfg.Query, topK)
	if err != nil {
		return Output{}, classifyRetrievalError(err)
	}
	if results == nil {
		results = []retrieval.ScoredDoc{}
	}
	data, err := json.Marshal(retrieveOutput{
		Retriever: cfg.Retriever,
		Query:     cfg.Query,
		TopK:      topK,
		Results:   results,
	})
	if err != nil {
		return Output{}, fmt.Errorf("marshaling retrieve output: %w", err)
	}
	return Output{Data: data}, nil
}

// classifyRetrievalError maps a retriever failure onto the executor's
// classified contract. A *retrieval.Error carries its ADR-006 class; a
// context error passes through unwrapped so the engine judges
// timeout/cancelled (ADR-006 rows 3/8); anything else falls to the
// engine's default-transient.
func classifyRetrievalError(err error) error {
	var re *retrieval.Error
	if errors.As(err, &re) {
		return &ClassifiedError{Class: re.Class, Err: err}
	}
	// Context cancellation/deadline (or any other pass-through) keeps its
	// identity so errors.Is against context.Canceled/DeadlineExceeded holds
	// in the engine.
	return err
}
