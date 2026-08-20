# ADR-009: Plugin SPI — kinds, registration, config schemas, capability flags

- **Status:** Accepted
- **Date:** 2026-08-14
- **Ticket:** ticket 8.1

## Context

M8 is where the engine meets AI: LLM steps, tool calls, retrieval, and —
in later milestones — validators all need concrete implementations behind
the engine's execution pipeline. Ticket 4.1 shipped a deliberately minimal
executor SPI (`internal/exec`): an `Executor` interface and an
instance-based registry keyed by step type, with a note that the full
plugin SPI would arrive in M8 and grow it rather than replace it.

Three consumers now force the SPI to become explicit architecture rather
than an internal convenience:

1. **The engine** looks plugins up by identity and, starting with M9's
   middleware chain, must make behavioral decisions it cannot infer from
   the implementation — may this output be cached? does this call spend
   money? does it fire external side effects that must be journaled?
2. **The API** must serve a machine-usable plugin listing
   (`GET /v1/plugins`) so operators and tooling can see what a deployment
   can execute, with each plugin's config schema in a form UI forms can
   consume (M17.4 builds the builder's config panels directly from these
   schemas — one contract, three consumers).
3. **Future plugin kinds** — tools (8.7), retrievers (8.8), model
   providers (8.3/8.4), validators (M11) — need one registration
   discipline instead of four ad-hoc registries invented independently.

The forces in tension: the SPI must be general enough that every later AI
feature registers through it, but M8 must not build speculative machinery
(out-of-process isolation, dynamic discovery, hot reload) that nothing on
the roadmap needs. And the ~15 existing call sites constructing
`exec.Registry` instances in tests must not be churned by the refactor.

## Decision

### Plugin kinds

We define a closed vocabulary of five plugin kinds:

| Kind | Interface (owning package) | Arrives |
|---|---|---|
| `executor` | `exec.Executor` (`internal/exec`) | M4 (refactored here) |
| `tool` | tool SPI (`internal/tools`) | 8.7 |
| `retriever` | retriever SPI (`internal/…`, 8.8) | 8.8 |
| `model_provider` | provider interface (`internal/llm`) | 8.3 (interface), 8.4 (registry + routing) |
| `validator` | validator SPI (`internal/validate`) | M11 |

A plugin's identity is the pair **(kind, name)**. Names are
`^[a-z][a-z0-9_]*$`, at most 64 characters — the same spelling as step
types, which is deliberate: an executor plugin's name **is** its step
type. Names are unique per kind, not globally (a `tool` named `retrieve`
would be confusing but is not a registry error).

### In-process compilation model

Plugins are ordinary Go packages compiled into the deployable binaries.
Registration happens at process boot into instance-based registries
(never package globals — the 4.1 decision stands); registries are
read-only after boot, so lookups take no locks. An invalid or duplicate
registration **fails startup** — a mis-wired plugin is a build/deploy
bug, and shadowing or skipping it silently would surface as baffling
runtime behavior instead.

Out-of-process plugins (subprocess, gRPC) and WASM sandboxing are
**explicitly deferred to the backlog**. Nothing on the v1 roadmap needs
untrusted third-party plugins; the SPI's shape (manifest + interface
value) does not preclude adding an out-of-process adapter later.

Consequence of in-process compilation: **the plugin set is a build-time
property**. Both deployables can therefore construct the same catalog
from the shared packages, which is how the API serves `GET /v1/plugins`
without any registration handshake (below).

### The manifest and the generic registry

`internal/plugin` is a new leaf package (imported by kind-owning
packages, importing none of them — no cycles) that owns the SPI's shared
vocabulary:

- `plugin.Kind` — the five-value enum above.
- `plugin.Capabilities` — the three capability flags (below).
- `plugin.Manifest` — the self-description every plugin carries: kind,
  name, semver version string, optional human description, capability
  flags, and an optional JSON Schema for its config
  (`json.RawMessage`; nil means "takes no config"). `Validate()`
  enforces the naming rule, the kind vocabulary, the version format, and
  that a present schema is a JSON object.
- `plugin.Registry` — the generic registry: `Register(Manifest, impl
  any)` (rejecting invalid manifests, nil impls, and duplicate
  (kind, name) with typed errors), `Lookup(kind, name)`, and sorted
  `Manifests()` / `ManifestsByKind(kind)` listings.

Kind-owning packages wrap it in **typed facades**: `exec.Registry` keeps
its 4.1 surface (`NewRegistry(execs ...Executor)`, `Get(stepType)
(Executor, error)` with the established `*UnknownTypeError`) but is now
backed by a `plugin.Registry` and additionally exposes `Manifests()`.
The facade pattern repeats for providers/tools/retrievers/validators as
they land: type-safe lookup at the call site, one shared registration
discipline and listing shape underneath.

### Self-description, and the test-double escape hatch

Production plugins describe themselves: an executor implements

```go
PluginManifest() plugin.Manifest
```

(the optional `exec.SelfDescribing` interface) and the registry validates
that the manifest's kind is `executor` and its name equals `Type()` — a
plugin may not claim an identity other than the one it executes under.

Bare `Executor`s without `PluginManifest` remain registrable. The many
test doubles across the engine, queue, and crash suites construct
registries with minimal stub executors, and forcing a manifest onto every
one of them buys nothing. For a non-describing executor the registry
**synthesizes a conservative manifest**: version `0.0.0`, and the flags
set to the assumptions that are safe when nothing is known —
`side_effectful: true` (never skip journaling for an unknown plugin),
`cacheable: false` (never serve a cached result for an unknown plugin),
`cost_bearing: false` (nothing to meter). A conformance test pins that
every executor in the shipped builtin sets self-describes, so synthesized
manifests can exist only in test code.

### Capability flags

Three boolean flags, chosen because they are exactly what M9 (cache,
rate-limit middleware) and M10 (cost ledger) must know without executing
the plugin:

- **`side_effectful`** — the plugin performs externally observable side
  effects. Its executions must go through the side-effect journal /
  idempotency keys (5.5), and its outputs must never be served from
  cache (replaying the output would skip the effect).
- **`cacheable`** — the plugin's output is a function of (config, input)
  *and* caching is semantically sound. This is deliberately stronger
  than "deterministic": `sleep` is pure but not cacheable (a cache hit
  would skip the wait, which is the step's whole meaning), and
  `fail_n_times` branches on the attempt number. LLM calls are
  non-deterministic yet *are* cacheable — M9's response cache exists
  precisely to reuse expensive non-deterministic outputs under a
  deliberate key design.
- **`cost_bearing`** — executing the plugin spends money (provider
  tokens, paid APIs). M10's ledger and budget enforcement meter these;
  M9's cache-hit accounting records counterfactual spend for them.

Flags describe the plugin's **contract, not today's implementation
shortcut**: through 8.6–8.8 the dev stubs (`llm`, then `tool`, then
`retrieve`) carried the flags of the real semantics that replaced them in
place, so middleware written against the flags behaved correctly across
each swap without a flag migration. With 8.8 every builtin ships at a real
`1.0.0` — no dev stub remains.

The builtin flag table (the conformance baseline):

| Plugin (executor) | Version | side_effectful | cacheable | cost_bearing |
|---|---|---|---|---|
| `noop` | 1.0.0 | – | ✓ | – |
| `echo` | 1.0.0 | – | ✓ | – |
| `sleep` | 1.0.0 | – | – | – |
| `fail_n_times` | 1.0.0 | – | – | – |
| `join` | 1.0.0 | – | – | – |
| `branch` | 1.0.0 | – | – | – |
| `counter` | 1.0.0 | ✓ | – | – |
| `effectful_echo` | 1.0.0 | ✓ | – | – |
| `llm` | 1.0.0 | – | ✓ | ✓ |
| `planner` | 1.0.0 | – | ✓ | ✓ |
| `tool` | 1.0.0 | ✓ | – | – |
| `retrieve` | 1.0.0 | – | ✓ | – |
| `human_approval` | 1.0.0 | ✓ | – | – |

(`human_approval` (ticket 15.2) is side_effectful — a pending approval is an
outward-facing request for a human decision, and a decision is never served
from cache — and not cost-bearing (its config carries no spend); its executor
does deterministic pre-flight then hands the engine an approval request to park
on. `planner` (ticket 13.3) is an llm-family executor — the `LLMExecutor`
re-targeted by embedding, keyed under its own name so a planner and an
`llm` step never share a response-cache entry — so it carries the llm flags
(cacheable + cost_bearing); its output is a `PlanOutput` the engine applies
to the running graph via `store.ExpandRun`. `join`/`branch` are pure but not
cacheable: they are control flow whose
meaning is readiness and edge firing — caching them is noise. The `tool`
*executor* is side-effectful because an unknown tool must be assumed to
act on the world; individual built-in tools carry their own per-tool
flags under kind `tool` — `http_request` side_effectful, `json_transform`
cacheable — see the 8.7 as-built section. The `retrieve` executor is
cacheable: a query is a pure read of the corpus and ranking is
deterministic given the corpus; the `pg_fulltext` retriever under kind
`retriever` carries the same flag — see the 8.8 as-built section.)

### Plugin version strings

Versions are semantic versions (`MAJOR.MINOR.PATCH` with optional
pre-release/build suffix, validated at registration). They exist for one
concrete consumer — **M9's cache keys**: a cached output is keyed on
(plugin name, plugin version, config, input) among other components, so
the bump rule is behavioral, not cosmetic: *bump the version whenever a
change should invalidate previously cached outputs*. Real builtins start
at `1.0.0`; the dev stubs carry `0.1.0-stub` so a stubbed plugin is
unmistakable in any listing, and their replacement by real executors is
itself the kind of behavior change that mandates the bump to `1.0.0` —
as `llm` did in 8.6 and `tool` did in 8.7 (the real executor's live
outputs must never read a cache entry keyed on the stub's echo).

### Config schemas

Per-plugin config schemas are **generated from the same Go structs the
decoder uses** (the project invariant since ADR-003) — never
hand-written. For executors, the config structs already live in the dag
package's step-config catalog; a new `dag.StepConfigSchema(StepType)`
reflects one step type's config struct into a standalone JSON Schema
document (invopop/jsonschema, same reflector settings as the definition
schema: strict `additionalProperties: false` matching
`DisallowUnknownFields`), and `exec.Registry` attaches it automatically
at registration, since executor name = step type. Non-executor kinds
supply schemas in their manifests using the same generator against
their own config structs.

The API serves each schema verbatim (`config_schema` in
`GET /v1/plugins`) as an embedded JSON Schema 2020-12 document — the
machine-usable form M17.4's UI forms will render directly.

### API exposure and the single-build assumption

`GET /v1/plugins` (read scope, read rate-limit class) lists the catalog
**compiled into the API binary**, sorted by (kind, name). There is no
worker→Postgres registration handshake: under the in-process model the
plugin set is a build-time property, and API and workers ship from the
same build (the compose stack and every planned deployment build both
images from one source tree).

One configuration wrinkle: the worker's registry is gated by
`AGENTLOOM_WORKER_TEST_EXECUTORS` (6.2 — the filesystem-writing test
executors are opt-in). The API mirrors this with
`AGENTLOOM_API_TEST_EXECUTORS` (default false), so a deployment sets
both knobs alike and the listing matches the fleet. We accept that a
heterogeneous fleet (mixed builds or mismatched knobs) could make the
listing inaccurate; dynamic fleet introspection is deferred until
something real needs it, and the ADR records the assumption so that
change has a trigger.

`ctl plugins list` renders the listing as a table (kind, name, version,
capability flags).

### Model-provider registry & routing (as built, 8.4)

The `model_provider` kind gets the same typed-facade treatment the
executor kind got in 8.1: `llm.Registry` wraps `plugin.Registry`
(kind `model_provider`, name = provider name), with `Register`
identity-checking the manifest's kind, `Get(name)`, `Manifests()`, and
`Names()`. Registration failures (nil provider, wrong kind, duplicate
name) fail boot, exactly like executors.

Providers are the one kind whose **listing is configuration-gated, not
just build-gated**. An executor is compiled in unconditionally; a
provider can only be constructed with an API key, and a worker running
no llm steps boots keyless (8.3). So `llm.NewRegistryFromKeys` — the one
constructor both deployables call — builds Anthropic iff its key is
present, OpenAI iff its key is present, and an empty registry is valid.
This directly satisfies 8.4's "either provider absent without breaking
startup": the *catalog* a deployment lists matches the *keys* it holds.
`internal/llm` stays a leaf (imports `dag`, `plugin`, stdlib): the
constructor takes a plain `ProviderKeys` struct, so `internal/config`
never enters llm's import graph.

**Routing.** `Registry.Resolve(explicitProvider, model)` is the entry
point the 8.6 llm executor calls; it returns the provider and the
canonical model the provider should receive, applying three rules in
priority order:

1. an explicit `provider` step-config field wins (unknown name →
   `*ProviderUnavailableError`);
2. a `"<provider>/<model>"` namespace prefix routes by the named
   provider and strips the prefix (this is the form the 8.5 mock provider
   uses, `mock/...` — now live, see the 8.5 section below);
3. a bare model matches the longest vendor prefix in a small static
   table (`claude`→anthropic; `gpt-`/`chatgpt-`/`o1`/`o3`/`o4`→openai).

Two typed errors are deliberately distinct: `*UnknownModelError` (no
rule matched — the ticket's required typed error) versus
`*ProviderUnavailableError` (a rule resolved to a provider that isn't
configured, i.e. its key is absent). The split makes a misconfiguration
diagnosable — "no provider knows this model" needs a different fix than
"you didn't configure this provider" — and both are deterministic, so
8.6 maps them to permanent failures.

### Mock/simulation provider (as built, 8.5)

`internal/llm/mock.go` adds a third provider behind the same interface: a
deterministic, offline `Mock` registered under name `mock` and addressed
through routing rule 2 (`model: "mock/<model>"`), so it needs **no**
vendor-prefix entry and no code change in `Resolve`. It is the workhorse
of the test and load suites (M19) — cheap, reproducible, and offline by
construction.

- **Manifest.** Kind `model_provider`, name `mock`, version `1.0.0`
  (deliberately not a `0.x-stub` version — the mock is a permanent
  fixture of the system, never replaced in place like the dev stubs).
  Capability flags are `{cacheable: true}` only: its output is a pure
  function of (config, request) so M9 caching is sound, but it is
  **not** `cost_bearing` (the whole point is a free provider) and not
  `side_effectful`. This is the first provider whose flags differ from
  the HTTP providers' `cacheable + cost_bearing`, and the API catalog
  test pins the difference.
- **Determinism contract.** A `Seed` drives one seeded PCG PRNG
  (`math/rand/v2`) guarded by a mutex; the whole draw sequence per call —
  call counter, injection lottery, latency sample, per-rule sequence
  cursor — advances under that lock, so a given seed and a given
  *sequential* call order produce a byte-identical transcript
  (responses, errors, and latency draws). The latency wait itself happens
  outside the lock so concurrent callers aren't serialized; determinism
  is defined for a sequential order and documented as such. Time is
  injectable through a `Sleep` seam (nil → a real ctx-aware timer, the
  only clock the mock touches), honoring the injectable-time invariant.
- **Scripting.** `MockRule`s match on prompt substring, regex, or the
  1-based global call ordinal (`OnCall`), first match wins; each rule
  carries a `Respond` sequence whose last entry is sticky (the queuetest
  `Script` shape). Each `MockOutcome` is either a success (text or full
  `Blocks`, with explicit or estimated `Usage`) or, when `Status` is
  non-zero, a scripted `*Error` classified through the *same*
  provider-agnostic `classifyStatus` the HTTP providers use — so injected
  429/500s map onto the ADR-006 taxonomy identically. `MockInjection`
  adds global per-call 429/500 failure rates; a `Hang` outcome blocks on
  ctx until cancelled (timeout/cancel test fuel), returning the context
  error unclassified exactly like the real providers.
- **Wiring.** `ProviderKeys` gains `Mock *MockConfig` (nil = absent,
  never a boot error — it carries no key, so it is scripted, not
  authenticated), constructed by the one shared `NewRegistryFromKeys`.
  `config.LLMConfig.MockEnabled` / `AGENTLOOM_LLM_MOCK_ENABLED` toggles
  it (binary default off; compose defaults on so the M8 exit-criterion
  workflow runs on the mock in CI without any key). The canonical
  `examples/definitions/mock_pipeline.json` — a converted linear M4 chain
  of two `llm` steps passing data via 8.2 templating — is the reused e2e
  fixture, executed end-to-end against the mock in the engine integration
  suite.
- **Load distributions (added 19.1).** For the load campaign the mock's
  latency/token simulation grew additively without touching the transcript
  determinism: `LatencySpec` gained a **lognormal** mode (`P50`/`P99` →
  μ/σ) alongside the existing fixed/uniform modes, so a fleet mock can
  simulate a realistic long-tailed provider latency; a new `TokenDist`
  (`{Input, Output TokenSpec}`) lets an outcome **draw** its reported
  `Usage` from a distribution instead of the char-count estimator (an
  explicit `Usage` still wins). Both draw from the same seeded PRNG under
  the same lock, so a seed + call order still yields a byte-identical
  transcript; both default nil/zero, so the 12.1 mock token-counter parity
  holds unless a script opts in. The offline-run wire contract
  (`ParseMockScript`) gained matching `latency`/`tokens`/`inject` blocks
  (global) and per-outcome `latency`/`tokens`/`usage` overrides, with
  durations as Go-duration strings — the same strict-decode leaf-parser
  discipline as the 14.5 script. The compose load overlay
  (`docker-compose.load.yml`) mounts `test/load/mock.json` (lognormal
  p50 120 ms / p99 900 ms, uniform token draws, always-revise critic rule)
  into every worker via `AGENTLOOM_LLM_MOCK_SCRIPT_FILE`.

### Tool SPI & built-in tools (as built, 8.7)

`internal/tools` is the fourth kind-owning leaf package (imports `plugin`
+ `dag` + stdlib + gojq + jsonschema/v6, never `exec`/engine), giving the
`tool` kind the same typed-facade treatment executors and providers got.

- **Interface.** `Tool` = `Manifest() plugin.Manifest` +
  `Invoke(ctx, Invocation) (json.RawMessage, error)`. The ticket's literal
  `Invoke(ctx, args)` grew to an `Invocation` struct so `http_request` can
  read the step's stable idempotency key (5.5) and a logger without a
  future signature churn — the `StepContext` precedent. Every tool
  **must** declare its args JSON Schema in `Manifest().ConfigSchema` (nil
  is a registration error; a no-arg tool declares `{"type":"object"}`),
  generated from the tool's Go arg struct with the same invopop reflector
  settings `dag.StepConfigSchema` uses — the ADR-003 "structs are the
  source of truth" invariant, extended to tool args.

- **The registry validates args, generically.** Unlike executor config
  (validated only by strict struct decode), tool args are validated
  against the declared schema at dispatch by a real 2020-12 validator
  (`santhosh-tekuri/jsonschema/v6`, compiled once per tool at
  registration — an uncompilable schema fails boot). `Registry.ValidateArgs`
  is the framework gate the `tool` executor calls **before** `Invoke`, so
  the acceptance "bad args → permanent failure, no call" is a framework
  guarantee every tool (including future third-party ones) inherits, not a
  per-tool discipline. A violation is a typed `*ArgsValidationError`
  (permanent); an unknown tool is `*UnknownToolError`.

- **`exec.ToolExecutor`** replaces `StubToolExecutor` in place (version
  bumped `0.1.0-stub` → `1.0.0`, the M9 cache-bust trigger; flags
  unchanged — `side_effectful`, since an unknown tool must be assumed to
  act on the world). It decodes the already-8.2-rendered `ToolConfig`,
  looks the tool up, validates args, invokes once (no retry — the M5
  engine owns retry), and persists the tool's result **verbatim** (no
  envelope — the tool name already lives in the step config, and
  downstream templating reads `${{ steps.x.output.<field> }}` directly).
  A `*tools.Error`'s class is honored via `exec.ClassifiedError`; context
  errors pass through unwrapped (engine judges timeout/cancelled).

- **Built-ins.** `http_request` (side_effectful) makes one outbound call
  guarded by a host **allowlist** (the SSRF guard: an empty allowlist
  denies every host — the safe default — and a blocked host is the typed
  `*HostNotAllowedError`, permanent, provably before any connection;
  `CheckRedirect` re-validates every redirect hop, closing the
  redirect-to-forbidden-host bypass), bounded by a timeout and a
  response-size cap, and stamped with an automatic `Idempotency-Key`
  header on non-GET calls from the 5.5 key (the header wins over any
  user-supplied one — key stability is the guarantee). `json_transform`
  (cacheable — the first pure built-in tool) evaluates a gojq program
  under `ctx` (a pathological program is bounded by the step timeout);
  one emitted value returns that value, zero-or-many returns an array.

- **Wiring.** `tools.NewBuiltins(HTTPOptions)` is the one constructor both
  deployables call. `cmd/worker` builds it from `config.ToolsConfig`
  (`AGENTLOOM_TOOLS_HTTP_ALLOWLIST` empty-default deny-all, `_TIMEOUT`,
  `_MAX_RESPONSE_BYTES`) and hands it to the exec registry; `cmd/api`
  folds the tools' (config-independent) manifests into `GET /v1/plugins`
  beside the providers'. `exec.Builtins`/`CoreBuiltins` grew a
  `*tools.Registry` parameter (the 8.6 `*llm.Registry` precedent; nil is
  valid — a tool step then fails permanent at lookup). The canonical
  `fanout.json`'s one executed tool step moved from an `http_request`
  (which the empty compose allowlist would block) to an offline
  `json_transform` over the templated topic, keeping the M8 exit-criterion
  workflow fully offline on compose/CI; the non-executed corpus fixtures
  keep realistic `http_request` steps.

### Retrieval SPI & reference backend (as built, 8.8)

`internal/retrieval` is the fifth kind-owning leaf package (imports
`plugin` + `dag` + stdlib, never `exec`/engine/`store`), giving the
`retriever` kind the same typed-facade treatment. The reference backend
lives in a **subpackage**, `internal/retrieval/pgfts`, deliberately split
out so the SPI stays a leaf: only `pgfts` imports `store`. That split is
also the point — it is the worked example the "writing a retriever plugin"
walkthrough (`docs/plugins.md`) points at.

- **Interface.** `Retriever` = `Manifest() plugin.Manifest` +
  `Ingest(ctx, []Doc) error` + `Query(ctx, q string, k int) ([]ScoredDoc,
  error)`. `Doc` is `{id, content, metadata}` (id the upsert key, so
  re-ingesting a corpus is idempotent); `ScoredDoc` embeds `Doc` and adds a
  `score` — the shape written into step output. `Ingest` is **not** on the
  step execution path (steps only `Query`); it is corpus-loading code
  (tests, seeding, a future ingest API — deliberately not an endpoint in
  v1).

- **No per-retriever config schema.** Unlike tools, retrievers declare a
  nil `ConfigSchema`: the `retrieve` step's config shape — `retriever`,
  `query`, `top_k` — is uniform across every retriever, and its schema
  lives on the `retrieve` executor (generated from `dag.RetrieveConfig`
  like every executor's). So `retrieval.Registry` is the plain typed facade
  (register/get/manifests) without the tool registry's schema-compilation
  step.

- **`exec.RetrieveExecutor`** replaces the last dev stub
  (`StubRetrieveExecutor`) in place (version `0.1.0-stub` → `1.0.0`, the M9
  cache-bust trigger; flag unchanged — `cacheable`). It decodes the
  already-8.2-rendered `RetrieveConfig`, defaults `top_k` to 5 when absent
  and caps it at 100, resolves the named retriever (unknown → permanent),
  runs one `Query`, and writes the documented output shape:
  `{retriever, query, top_k, results}` where `results` is an array of
  `{id, content, score, metadata}` — **always present** (an empty array on
  no match, never null) so a downstream `${{ steps.<id>.output.results }}`
  never misses. A `*retrieval.Error`'s class is honored via
  `exec.ClassifiedError`; context errors pass through unwrapped; an empty
  rendered query and a negative `top_k` are deterministic ⇒ permanent.

- **Reference backend.** `pgfts` runs over Postgres full-text search —
  **zero new infrastructure**, the reason it is the reference: the corpus
  is one table (`retrieval_docs`, migration 0013) in the same Postgres the
  engine already depends on. Ranking uses a **functional GIN index** over
  `to_tsvector('english', content)` (no materialized tsvector column, so
  the sqlc row type stays scalar), and `Query` ranks by `ts_rank`
  descending with `websearch_to_tsquery` — which ANDs query terms and never
  errors on arbitrary input, so an empty/no-match query returns an empty
  slice, not a failure. `pg_fulltext` is `cacheable`-only (a query is a
  pure read; ranking is deterministic given the corpus), not
  side-effectful, not cost-bearing. Fixed `'english'` and a single flat
  corpus in v1; per-corpus language, namespaces, and the alternate backends
  (pgvector, external vector stores) are documented backlog plugins on this
  same SPI.

- **Wiring.** `retrieval.NewRegistry(pgfts.New(st))` is built in both
  deployables — always (no key, no toggle: it needs only the shared
  Postgres). `cmd/worker` hands it to the exec registry; `cmd/api` folds
  its manifest into `GET /v1/plugins`. `exec.Builtins`/`CoreBuiltins` grew a
  third `*retrieval.Registry` parameter (the 8.6/8.7 precedent; nil valid —
  a retrieve step then fails permanent at lookup). The new canonical
  `rag_lite.json` fixture is the RAG-lite proof: a `pg_fulltext` retrieve
  step feeds its ranked results into a mock-backed `llm` step via
  `${{ steps.search.output.results }}`, executed end-to-end against a seeded
  corpus in the engine integration suite; `fanout.json`'s retrieve step is
  now the real executor against an empty corpus (offline-green).

### Validator SPI (as built, 11.1)

The fifth and final kind, `validator` (ADR-013), lands its SPI in
`internal/validate` — the `internal/tools` structure exactly: a leaf
package importing `internal/plugin` (manifest vocabulary) and `internal/dag`
(ADR-006 error classes), never `internal/exec` or the engine. `Validator`
is `Manifest() + Validate(ctx, Input) (Verdict, error)`; `validate.Registry`
is the typed facade over `plugin.Registry` (kind validator) that **compiles
each validator's config JSON Schema once at registration** (nil schema
rejected — a no-config validator declares the empty object via
`EmptyConfigSchema`) and exposes `ValidateConfig`, the pre-flight gate the
engine's validate stage calls before running a validator. Typed errors
mirror `tools`: `*validate.Error{Validator, Class}` (transient/permanent —
a *transport* failure of the validation stage, distinct from a fail
verdict), `*UnknownValidatorError`, `*ConfigValidationError`; secret hygiene
is structural (no error field holds an output).

Capability flags: a validator is never `side_effectful` (a mutating
validator would break re-validation on retry and cache hit); the
deterministic validators (11.2) are `cacheable`, and the `llm_judge` (11.5)
is `cost_bearing` — which the engine reads to order the chain cheap-first
and to attribute judge cost as overhead (ADR-012 rule 4). **11.1 ships the
SPI with no built-in validators** (`validate.NewBuiltins()` is empty), so
the builtin flag table (§ "Capability flags") gains no validator rows yet;
11.2 registers `json_schema`, `regex`, `contains`, `cel`, `numeric_range`,
and 11.5 adds `llm_judge`. `cmd/worker` wires the (empty) registry via
`engine.WithValidators`; `cmd/api` folds its manifests into `GET /v1/plugins`
so the kind is listed. The engine's validate stage, verdict persistence, and
`validation_failed` routing are documented in ADR-013.

### Validator built-ins & flag table (as built, 11.2)

11.2 fills `NewBuiltins()` with the five deterministic validators, each a pure
function of output + config (so `cacheable` and nothing else); 11.5 adds
`llm_judge`, the first `cost_bearing` validator (it calls a provider). The
engine reads `cost_bearing` to order the chain cheap-first (the judge runs only
on an output the free validators accepted) and to attribute the judge's call as
overhead (ADR-012 rule 4). `NewBuiltins` gained a `*llm.Registry` parameter in
11.5 — the judge routes its model through the same registry the llm executor
uses; a nil registry registers the judge (for the listing) but fails every
config's routing pre-flight. The validator flag table:

| Plugin (validator) | Version | side_effectful | cacheable | cost_bearing |
|---|---|---|---|---|
| `json_schema` | 1.0.0 | – | ✓ | – |
| `regex` | 1.0.0 | – | ✓ | – |
| `contains` | 1.0.0 | – | ✓ | – |
| `cel` | 1.0.0 | – | ✓ | – |
| `numeric_range` | 1.0.0 | – | ✓ | – |
| `llm_judge` | 1.0.0 | – | ✓ | ✓ |

No validator is ever `side_effectful` (a mutating validator would break
re-validation on retry and on cache hit). Validators that compile an artifact
whose *content* the config JSON Schema cannot vet (an RE2 pattern, a CEL
predicate, a JSON Schema document, a `numeric_range`'s cross-field bounds)
implement the optional `ConfigCompiler` (`CompileConfig(config) error`): the
`Registry.ValidateConfig` pre-flight gate calls it after the schema check, so a
bad artifact is a permanent config error before any spend, and the successful
call warms the validator's compile cache (compiled once per distinct config per
process — the tools-args-schema precedent, but for the artifact rather than the
schema). Config shapes, the string-vs-JSON target rules, and the issue-code
vocabulary are in ADR-013's "Built-in deterministic validators" section.

## Consequences

Easier:

- Every later AI feature (8.3–8.8, M11) registers through one
  discipline: implement the kind's interface, return a manifest, add to
  the deployables' catalog construction. Registration bugs fail boot.
- M9/M10 middleware reads capability flags and versions off the manifest
  instead of maintaining per-type knowledge, and behaves correctly
  across the stub→real executor swaps without changes.
- The UI's config panels (M17.4) consume the same generated schemas the
  API serves — no second source of truth for config shapes.
- The exec facade keeps its 4.1 surface, so the refactor touches no
  engine code and no existing test.

Harder / accepted costs:

- `GET /v1/plugins` reflects the API binary's catalog, not measured
  fleet capability; a mixed fleet can make it lie (accepted,
  single-build assumption documented above).
- The synthesized-manifest escape hatch means a production executor
  *could* ship without self-describing if the conformance test were
  deleted; the test is the guard.
- Capability flags are declared, not verified — a plugin can lie about
  `side_effectful`. In-process plugins are trusted code; verification
  only becomes meaningful with the deferred isolation work.
- One more package (`internal/plugin`) beyond the planned layout;
  recorded here as a layout addendum.

## Alternatives considered

- **Grow the whole SPI inside `internal/exec`** (the 4.1 comment's
  literal reading). Rejected: model providers and validators are not
  executors, and forcing `exec.Manifest` onto them misnames the concept;
  a leaf `internal/plugin` package gives the shared vocabulary a home
  with zero import-cycle risk.
- **Require every executor (including test doubles) to self-describe.**
  Rejected: it churns ~15 test call sites for no information gain; the
  synthesized conservative manifest plus the builtin conformance test
  achieves the same production guarantee.
- **Worker-registered plugin rows in Postgres, API serves the table.**
  Rejected for 8.1: it adds a migration, liveness/GC semantics for
  departed workers, and a merge policy for heterogeneous fleets — all to
  answer a question the in-process model answers at compile time. The
  deferred trigger is real fleet heterogeneity.
- **Capability flags as a free-form string set.** Rejected: the three
  consumers (cache, journal, ledger) are known, and a closed struct
  keeps them type-checked; a fourth flag is one field away.
- **Full semver parsing/ordering (Masterminds/semver dependency).**
  Rejected: nothing orders versions yet — M9 only needs equality in a
  cache key. A format regex suffices; a library can arrive when ordering
  does.
- **Serving schemas by reference (`/v1/plugins/{name}/schema`).**
  Rejected: the whole catalog with embedded schemas is a few tens of KB,
  served rarely, and one round trip is simpler for the UI.
