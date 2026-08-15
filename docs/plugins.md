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
| `validator` | `internal/validate` | `Validator` — judges a step output (ADR-013) |

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

## Writing a validator plugin (walkthrough)

A validator judges a step's output and returns a **verdict** (pass/fail with
structured issues) — the substrate for M11's semantic retries
([ADR-013](adr/013-output-validation-and-semantic-retries.md)). The shape
mirrors a tool: a manifest whose `ConfigSchema` is generated from the
validator's own Go config struct (compiled once at registration, enforced
before the validator runs), plus one `Validate` method.

**1. Implement `validate.Validator`.**

```go
package myvalidator

import (
	"context"

	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Config is the validator's config struct; its JSON Schema is generated
// from these tags and enforced by the framework before Validate runs.
type Config struct {
	Substring string `json:"substring"`
}

type Validator struct{}

func (Validator) Manifest() plugin.Manifest {
	schema, _ := validate.ConfigSchema(&Config{}) // generated, never hand-written
	return plugin.Manifest{
		Kind:         plugin.KindValidator,
		Name:         "my_validator", // ^[a-z][a-z0-9_]*$, unique per kind
		Version:      "1.0.0",
		Capabilities: plugin.Capabilities{Cacheable: true}, // never side_effectful
		ConfigSchema: schema,
	}
}

func (Validator) Validate(ctx context.Context, in validate.Input) (validate.Verdict, error) {
	// in.Value is the sub-tree the chain entry's `target` selected (the
	// whole output when absent; /text for llm-family steps). in.Config is
	// already schema-validated. Return:
	//   - a PASS verdict when the output is acceptable,
	//   - a FAIL verdict with issues (codes/paths/messages) otherwise —
	//     a fail verdict is a *successful* validation with a negative result,
	//   - a non-nil error ONLY when you cannot judge at all: a
	//     validate.Permanentf/Transientf (a transport failure of the
	//     validation stage), or the ctx error UNWRAPPED.
	return validate.PassVerdict(), nil
}
```

**2. Register it in the deployables.** Both `cmd/worker` (which runs the
validate stage) and `cmd/api` (which lists the catalog) build a
`validate.Registry`; extend `validate.NewBuiltins` or register directly:

```go
validators, err := validate.NewRegistry(myvalidator.Validator{})
```

`cmd/worker` hands it to `engine.WithValidators(validators)`; `cmd/api` folds
`validators.Manifests()` into `GET /v1/plugins`.

**3. Select it from a workflow.** A step's `validation` envelope names a
chain of validators, each optionally targeting a JSON-pointer sub-tree:

```json
{ "id": "gen", "type": "llm", "config": { "model": "mock/sim-1", "prompt": "..." },
  "validation": { "validators": [
    { "name": "my_validator", "config": { "substring": "Summary" } },
    { "name": "json_schema",  "config": { "schema": { "type": "object" } }, "target": "/text" }
  ] } }
```

A step naming a validator the fleet did not register, or a config that fails
its schema, dead-letters `permanent` at claim — **before** the executor runs,
so nothing is spent. A failing verdict records the `validation_failed`
outcome (the verdict persisted on the attempt, visible on
`GET /v1/runs/{id}`). If the step declares a semantic policy
(`validation.max_attempts >= 2`), the engine then re-attempts with a
feedback-augmented prompt (the **semantic-retry loop**, 11.4 — see below)
before dead-lettering; without one it dead-letters on the first bad verdict.

### Built-in validators (11.2)

The fleet ships five deterministic validators — pure, fast, `cacheable`-only:

| Name | Config | What it checks |
|---|---|---|
| `json_schema` | `{schema}` | the target (a JSON string is re-parsed as JSON) satisfies a JSON Schema; path-level issues |
| `regex` | `{pattern, negate?}` | the target's string form matches an RE2 pattern |
| `contains` | `{substring, case_insensitive?, negate?}` | the target's string form contains a substring |
| `cel` | `{expr, parse_json?}` | a CEL boolean predicate over `value` / `output` / `step_type` |
| `numeric_range` | `{min?, max?, exclusive_min?, exclusive_max?}` | the target (a numeric string is accepted) is a number within bounds |

The default `target` for an llm-family step is `/text` (the model's answer),
so `json_schema`/`regex`/`cel` judge the answer rather than the `{model,
stop_reason, text}` envelope.

**`ConfigCompiler` (optional).** A validator whose config compiles into an
artifact the config JSON Schema cannot fully vet — an unparseable regex, a CEL
expression that will not typecheck, a `numeric_range` with no bound —
implements `CompileConfig(config json.RawMessage) error`. The framework calls
it at claim (pre-flight, after the schema check), so a bad artifact is a
permanent config error before any spend, and a compile cache keyed on the
config bytes means the artifact is built once per distinct config, not per
attempt. A *runtime* failure over a particular output (a `cel` predicate
referencing a missing field) is instead a **fail verdict**, not a config
error — repairable by the semantic retry.

### Structured output & JSON repair (11.3)

An `llm` step can declare an `output_format` on its config to get
structured output with repair and validation built in
([ADR-013](adr/013-output-validation-and-semantic-retries.md)):

```jsonc
"config": {
  "model": "anthropic/claude-sonnet-5",
  "prompt": "Extract the fields as JSON: ${{ run.params.doc }}",
  "output_format": {
    "type": "json_schema",              // or "json" for any-JSON (parseability)
    "schema": { "type": "object", "properties": { "title": {"type":"string"} }, "required": ["title"] },
    "mode": "auto"                      // or "repair_only" to leave the request untouched
  }
}
```

What the engine does with it:

- **Native structured output** (`mode: auto`) — asks the provider for
  structured output through its native mechanism (Anthropic forced tool-use,
  OpenAI `response_format`); the mock answers `{"echo": <text>}` natively.
- **Deterministic JSON repair** — when the model answers in text anyway, a
  cheap pass strips code fences, extracts the first JSON value from prose,
  removes trailing commas, and quotes bare keys before declaring failure.
- **An implicit `json_schema` validator** — prepended to the step's chain,
  enforcing the declared shape over the (repaired) output, so an output that
  cannot be shaped into schema-conforming JSON dead-letters as
  `validation_failed` rather than flowing on malformed.

The persisted output gains a `json` field (the structured value) alongside
`text` (the canonical compact JSON), so downstream steps read
`${{ steps.<id>.output.json.<field> }}`. Each attempt records repair
provenance — `attempts[].repair` = `{status: native|raw|repaired|unrepairable,
steps?, raw_text?}` — in `GET /v1/runs/{id}`. See
[`examples/definitions/structured_extract.json`](../examples/definitions/structured_extract.json)
for the canonical workflow.

### Semantic retries (11.4)

When a step's output fails its validation chain, a **semantic policy** lets the
engine rebuild the prompt from the critique and re-attempt — instead of
dead-lettering on the first bad verdict. It is authored alongside the chain on
the step envelope:

```jsonc
"validation": {
  "validators": [ { "name": "contains", "config": {"substring": "APPROVED"}, "target": "/text" } ],
  "max_attempts": 3,                    // the semantic budget (1..10; 1 = no retry, the default)
  "feedback": {                         // llm-family only; requires max_attempts >= 2
    "template": "This is attempt ${{ feedback.attempt }} of ${{ feedback.max_attempts }}. Your previous output was rejected:\n${{ feedback.issues }}\n\nPrevious output:\n${{ feedback.prior_output }}\n\nRevise it.",
    "max_output_chars": 2000            // truncates the spliced prior output
  }
}
```

On a failing verdict the engine records the attempt `validation_failed`,
renders the feedback template (the default is used when `feedback` is omitted),
and re-attempts with the critique folded into the prompt — a **different**
request under a **semantic** budget disjoint from the transport retry budget
(`retry`). The augmented prompt re-keys the response cache by construction, so a
semantic retry is never served a stale hit; the attempts are cost-metered like
any other. Exhausting `max_attempts` dead-letters the step with the full
verdict history on the DLQ record. The template admits exactly four tokens —
`${{ feedback.prior_output }}`, `${{ feedback.issues }}`, `${{ feedback.attempt }}`,
`${{ feedback.max_attempts }}` — a narrower surface than the
[step-input expressions](expressions.md).

`GET /v1/runs/{id}` exposes both budgets per step (`transport_failures` and
`validation_failures`, disjoint) and each augmented prompt as
`attempts[].feedback` = `{semantic_attempt, max_attempts, prior_attempt, text}`.
See [`examples/definitions/semantic_retry.json`](../examples/definitions/semantic_retry.json)
for the canonical workflow.
