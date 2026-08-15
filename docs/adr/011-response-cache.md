# ADR-011: Response cache — key design & invalidation

- **Status:** Accepted
- **Date:** 2026-08-14
- **Ticket:** ROADMAP.md ticket 9.4

## Context

Agent workflows re-run the same expensive, deterministic-enough calls
constantly: the same `temperature=0` summarization on the same rendered
prompt, the same pure `json_transform`, the same corpus query. Every one of
those is a paid provider round-trip or a redundant computation the fleet has
already done. M9's second mandated feature is a response cache that turns the
repeat into a lookup — implemented, like the rate limiter (ADR-010), as
executor middleware so every current and future step type inherits it.

A response cache for AI outputs is not an HTTP cache, and the naive version is
actively dangerous. The forces in tension:

- **Identical inputs do not imply identical outputs.** An LLM at
  `temperature > 0` is non-deterministic by construction; serving a cached
  completion for it silently changes the workflow's semantics. Caching must be
  a deliberate, capability-gated decision, never an assumption that "same
  request ⇒ same response."
- **A cache key must invalidate on every axis that can change the output.**
  Not just the rendered prompt: the model, the sampling params, the provider,
  the plugin version (a behavior change), and the definition format version
  all bear on the result. Miss one and the cache serves a stale or wrong
  answer that looks authoritative. ADR-009 already made plugin versions
  behavioral *for exactly this consumer* — "bump the version whenever a change
  should invalidate previously cached outputs."
- **A key must be stable across cosmetic differences.** Two rendered requests
  that differ only in JSON key order or number spelling (`1` vs `1.0`) are the
  same request; if they key differently the hit rate collapses to noise.
- **Side effects must never be replayed from cache.** A `tool` step that
  charges a card or sends an email is not a function to memoize — ADR-009's
  `side_effectful` flag is the hard gate.
- **The cache is disposable, the run state is not.** Postgres remains the
  source of truth (project invariant). A cache entry is derived data whose
  loss costs money, never correctness — which decides both the store (Redis,
  not Postgres) and the failure stance (a cache error is a bypass, never a run
  failure).

This ADR decides the key format, the eligibility/default policy, the
invalidation strategy, and the storage. Ticket 9.4 delivers the definition
contract (`dag`'s `cache` step field) and the key builder + policy
(`internal/cache`); tickets 9.5 (store & middleware) and 9.6 (invalidation &
ops surface) implement against it, and append their "(as built)" subsections
here as they land.

## Decision

### The cache key

A cache key is the hex SHA-256 over an ordered list of components, one per
axis that can change the output. `internal/cache.Key(KeyInput)` builds it from:

1. **`KeySchemaVersion`** — the key builder's own format version (currently
   `1`). A change to *how* keys are built (a new component, a different
   canonicalization) bumps this integer, which strands every prior entry
   behind an unreachable prefix rather than risking a silent collision with an
   entry built the old way.
2. **The workflow definition `schema_version`** — a format change (ADR-003)
   that could reinterpret a stored config invalidates every prior key.
3. **Scope discriminator and run id** — `global` (empty run id) or `run:<id>`
   (see *Scope*).
4. **The executor plugin identity** — kind, name, version: `llm`, `tool`,
   `retrieve` at its semver. The executor turns config into a request and a
   response into stored output; a change to that transformation bumps its
   version and invalidates.
5. **The concrete external plugin identity** — kind, name, version: the model
   *provider* for an llm step, the *tool* for a tool step, the *retriever* for
   a retrieve step. This is also the Redis namespace (below).
6. **The per-step-type request body** — the resolved, rendered content that
   determines the output:
   - **llm** — resolved model, system prompt, sampling params (`temperature`,
     `max_tokens`), the rendered messages, and the tool schemas offered to the
     model. `temperature` distinguishes *nil* (the provider default, a
     different non-deterministic request) from an explicit `0`. The tool
     schemas component is currently vacuous for `llm` steps (no config field
     feeds provider tools yet) but the slot exists so M13/M14 agent steps
     need no key-format bump. Because the resolved model is a key component,
     M10.4's budget-driven downgrade to a cheaper model automatically yields a
     **different cache key** — a downgraded claim never reads (or writes) the
     primary model's cache entry (asserted in 10.4). **11.3 amendment:** an
     llm step's `output_format` block (structured-output policy) is a trailing
     key component appended **only when present**, so a plain-text step keys
     exactly as it did pre-11.3 (existing entries stay valid) while a format
     re-keys the entry. It must be a component because the same request yields
     a different output under a different format (native structured vs.
     repaired vs. raw text), and a `repair_only` format changes the output
     without changing the provider request. No `KeySchemaVersion` bump — the
     append-when-present rule keeps every existing key stable.
   - **tool** — the rendered tool arguments.
   - **retrieve** — the rendered query and `top_k`.

Two properties the builder guarantees:

- **Canonicalization.** Every JSON-bearing component (messages, tool schemas,
  tool input) is round-tripped through Go's decoder before hashing, so object
  keys sort and number spelling normalizes (`1`, `1.0`, `1e0` → `1`).
  Semantically equal requests hash equal. The round-trip's documented limits —
  integers above 2^53 lose precision, duplicate object keys collapse — are
  accepted; a rendered request that trips them is pathological, and the cost is
  a cache miss, never a wrong hit.
- **Unambiguous framing.** Components are **length-prefixed** (an 8-byte
  big-endian length before each) rather than delimiter-separated. No
  component's bytes can be mistaken for a boundary, so `("ab","")` and
  `("a","b")` key differently — strictly stronger than the `0x00`-separator
  scheme the 6.5 idempotency fingerprint uses, chosen here because a cache-key
  collision serves a wrong output, a higher stakes than a fingerprint mismatch.

An unusable input — missing plugin identity, run scope with no run id,
malformed request JSON — returns an error, never a silent weak key; the 9.5
middleware treats that as "do not cache this step."

### Redis key layout & namespacing

Entries are stored under `<prefix>:v<KeySchemaVersion>:<kind>:<name>:<hash>`,
where `<prefix>` is fleet configuration (9.5) and `<kind>:<name>` is the
**concrete external plugin** (component 5): `cache:v1:tool:json_transform:…`,
`cache:v1:model_provider:anthropic:…`, `cache:v1:retriever:pg_fulltext:…`.

This is deliberately the bust granularity 9.6 needs: an operator busts every
entry for one provider/tool/retriever by its prefix, everything under a
`KeySchemaVersion` by the `v<n>` segment, or the whole cache by the configured
prefix — all with a single non-blocking `SCAN` pattern.

### The default policy

`internal/cache.Decide` encodes a two-layer rule. Layer one is a **hard
eligibility gate** off ADR-009's capability flags: a plugin is cacheable iff
`Cacheable == true` **and** `SideEffectful == false`. No step-level policy can
open this gate — a `cache` block on a side-effectful step is a silent bypass,
not an error (submit-time validation cannot see a *tool's* flags, so the
worker owns enforcement; and test-double executors synthesize
`cacheable:false`, so they are ineligible by construction).

Layer two, within eligible plugins, is **determinism-driven** and overridable:

| Step / plugin | Capability | Determinism signal | Default | With `cache.mode` |
|---|---|---|---|---|
| `llm`, `temperature == 0` | cacheable, cost-bearing | deterministic | **read-write** | overridable |
| `llm`, `temperature != 0` or nil | cacheable, cost-bearing | non-deterministic | **bypass** | `read_write` opts in |
| `tool` → pure (e.g. `json_transform`) | tool cacheable | deterministic | **read-write** | overridable |
| `tool` → side-effectful (e.g. `http_request`) | tool side_effectful | — | **bypass (hard)** | any mode is a bypass |
| `retrieve` (`pg_fulltext`) | cacheable | non-deterministic¹ | **bypass** | `read_write` opts in |
| `sleep`, `join`, `branch`, `fail_n_times` | not cacheable | — | **bypass (hard)** | any mode is a bypass |
| `counter`, `effectful_echo` | side_effectful | — | **bypass (hard)** | any mode is a bypass |

¹ A retrieve query is a pure read *of a mutable corpus*: ADR-009 flags it
cacheable (deterministic given the corpus), but because `Ingest` mutates the
corpus underneath it, caching a retrieval is a staleness trade the author opts
into with a TTL, not a default.

The determinism signal is executor-supplied because it is plugin-specific: the
llm executor passes `temperature != nil && *temperature == 0`, a pure tool
passes `true`, a retrieve passes `false`. **The tool decision consults the
invoked tool's manifest, not the tool *executor's*** — the tool executor is
`side_effectful` (worst case across tools), but `json_transform` the tool is
cacheable, and 9.5 feeds `Decide` the tool-level flags.

### Step-level override — `cache: {mode, ttl, scope}`

`cache` is an optional per-step envelope field (ADR-003 reserved the name),
uniform across step types like `retry` and `timeout`, taking literals only (no
templating). It overrides the default *within eligibility*:

- **`mode`** (required when a `cache` block is present) — `off` (never
  read/write), `read_write` (read a hit, write a miss), `read_only` (serve a
  hit, never write — a canary/migration mode for validating a cache against
  live traffic without populating it).
- **`ttl`** — a Go duration, bounded by `MaxCacheTTL` (30 days) at validation;
  absent means the fleet default TTL (9.5 config) applies.
- **`scope`** — `global` (default) or `run`.

### Scope

`global` shares an entry across every run of every definition: the key is a
pure function of the request. `run` mixes the run id into the key, so a
retried or resumed step of the same run reuses its own result but no other run
does — for steps whose inputs are run-identical yet must not leak across runs.
A `definition`-wide scope is deliberately deferred: it adds a third key
variant for a use case (share within one workflow's runs but not across
workflows) no current milestone needs, and it can be added as an additive
scope value without a `KeySchemaVersion` bump.

### Invalidation

Three mechanisms, no more:

1. **TTL** — every written entry carries one (step override or fleet default,
   ≤ `MaxCacheTTL`). Redis-native, so idle entries self-evict.
2. **Version bump** — the plugin version and prompt-template version live *in*
   the key (components 4/5), and `KeySchemaVersion` gates the whole namespace.
   A behavioral change to a plugin bumps its version (ADR-009's rule) and the
   old entries become unreachable and TTL out.
3. **Admin bust-by-prefix** — 9.6's `SCAN`-batched, non-blocking, audited
   delete by the Redis key prefix (provider/tool/retriever, version, or all).

There is no explicit dependency tracking (e.g. "bust every retrieval when the
corpus changes"): the corpus is not versioned, so a retrieve opts into caching
with a short TTL and accepts bounded staleness, or an operator busts
`retriever:*` after a re-ingest.

**Interplay with output validation (M11, ADR-013):** an output that fails its
validation chain is **never written to the cache** — the engine's validate
stage sits between execute and cacheWrite, and a fail verdict skips the write.
And because the validation chain is **not** a cache-key component (a definition
can tighten a validator without changing the request), a cache **hit** is
**re-validated** under the current chain: a stored-but-now-invalid output is
re-executed, not served. A passing hit still short-circuits the provider call.

### Storage — write-through Redis, with a size cap

Entries live in **Redis, not Postgres**, written through on a miss (the value
is the step output plus a usage snapshot for M10's counterfactual accounting,
9.5). Redis because a cache is disposable derived data: losing it costs a
re-computation, never correctness, so it does not belong in the source of
truth; Redis TTLs are native; and the read sits on the hot claim path ahead of
the rate limiter, where a Postgres round-trip would be a regression. Only
workers read the cache — the API's Redis independence (ADR-002) is untouched.

Values over a configured size cap (default 1 MiB, mirroring the tool HTTP
response cap) are **skipped, not stored**: no chunking, no Postgres spill. A
response too large to cache cheaply is left to re-compute, and the skip is
recorded as a bypass metric (9.5). The cap protects Redis memory from a
pathological giant output evicting thousands of useful small ones.

### Where the key components come from (9.5)

The 9.5 middleware sits ahead of the ADR-010 rate limiter in the executor
chain: on a read hit it returns the cached output *without* acquiring a rate
limit token or calling the provider. It projects a resolved request onto
`cache.KeyInput` — the executor resolves its provider/tool/retriever (the same
`Resolve`/registry lookup `ResourceClaim` already does), reads the plugin
manifests for versions, and supplies the definition `schema_version` and run
id from the engine (the executor's `StepContext` carries neither today). A
cache error at any point is fail-safe: the step executes normally, uncached.

### Metrics & ops surface (9.5 / 9.6)

`internal/obs` gains a `cache` subsystem — hit / miss / bypass / store
counters by plugin, and a stats endpoint (9.6) whose numbers an integration
test reconciles against the Prometheus counters. Admin bust is an
`admin`-scoped API action, audit-logged with the actor key id (ADR-007).

### Cache store & middleware (as built, 9.5)

Ticket 9.5 implemented the runtime against the 9.4 key builder and policy:

- **`run_steps.cache_policy` (migration 0015).** The step's authored `cache`
  block is materialized as a nullable JSONB column at instantiation, exactly
  like `retry_policy` (0003) and `timeout` (0004): the middleware reads the
  effective policy off the claimed row and never reparses the definition
  snapshot, and a worker upgrade cannot change an in-flight run's cache
  behavior. NULL means the step authored no block (engine default decides).

- **The store — `internal/cache/redisstore`.** A thin byte-oriented Redis
  key/value layer over `cache.RedisKey`: it owns the prefix and namespacing,
  the value size cap (returns `cache.ErrValueTooLarge`, the sentinel living in
  the `cache` leaf so the engine matches it without importing the store), and
  the TTL on write. It does not know the shape of an entry — the engine
  marshals its `{output, usage}` value and hands opaque bytes, keeping `cache`
  a leaf and the store free of any exec/engine dependency (the pgfts
  precedent). A Redis error or a corrupt value is a miss, never a failure.

- **The executor hook — `exec.CacheBinder`.** Mirroring `ResourceClaimer`:
  the llm, tool, and retrieve executors project the resolved request onto a
  `CacheBinding` (the two plugin identities, the concrete plugin's capability
  flags, the determinism signal, the request body). A binder error means "skip
  caching, let Execute classify" — the routing judgment lives in one place.
  The **tool binding reports the invoked tool's flags, not the tool
  executor's** (the ADR-011 rule made real): `json_transform` is
  cacheable/deterministic, `http_request` is neither. The retrieve binding's
  determinism is always false (corpus-mutable, opt-in only).

- **The middleware — `engine/cache.go`.** `cacheRead` runs in `execute()`
  **ahead of the rate limiter**: a hit completes the step from the stored
  result with no limiter acquire and no provider call (asserted end-to-end —
  a granting recording limiter observes zero acquires on the hit); a miss
  returns a write binding threaded to `cacheWrite` after a successful
  execution. `SchemaVersion` is `dag.CurrentSchemaVersion` (only v1 exists, so
  a per-attempt DB read would provably return the constant; a future schema v2
  must thread the run's real version). Every error path is fail-safe: no
  binder, a declined policy, an unbuildable key, a corrupt policy, a corrupt
  entry, or a Redis error all resolve to "execute uncached."

- **`cache_hit` rides in the attempt's `usage` JSONB, no second migration.**
  0012 deliberately left `usage` an open object; `exec.Usage` gained
  `CacheHit bool`. On a hit the engine completes with the cached output and a
  usage marked `cache_hit=true` carrying the **counterfactual** token counts
  the original miss recorded (for M10's "saved" metric) — the flag lives
  *inside* usage precisely because it marks that usage as not-spent. A non-llm
  hit records `cache_hit=true` with zero tokens. The API's `usage` pass-through
  and the OpenAPI `usage` schema (open, no `additionalProperties: false`)
  needed no structural change.

- **Write-before-commit.** The write-through happens after a successful
  executor return but before the completion transaction. This is safe by
  construction: the entry is a pure function of the resolved request, so even a
  later-fenced completion wrote a *correct* entry. The shutdown-abandon path
  returns earlier and never writes.

- **Config & wiring.** `config.CacheConfig`
  (`AGENTLOOM_CACHE_{ENABLED,KEY_PREFIX,DEFAULT_TTL,MAX_VALUE_BYTES}`, enabled
  by default, default TTL 24h bounded by the 30-day ceiling, 1 MiB value cap).
  `cmd/worker` builds the redisstore over the same coordination Redis the queue
  uses and wires `engine.WithResponseCache`; `cmd/api` is untouched (the API
  never reads the cache — ADR-002's Redis independence holds).

- **Metrics.** A new `cache` subsystem (ADR-008 amended): `hits`/`misses`/
  `bypass`/`stores` counters labeled by `plugin` (`<kind>:<name>`, the concrete
  provider/tool/retriever, bounded by the compiled-in catalog) plus an
  unlabeled `fail_opens` counter (read/write Redis errors). 9.6's stats
  endpoint reconciles against these.

### Invalidation & ops surface (as built, 9.6)

Ticket 9.6 added the operator-facing half of the invalidation strategy — the
admin bust, the stats endpoint, and the `ctl cache` commands — on the 9.5
store and the `cache.RedisKey` namespacing, with **no migration, no engine
change, and no new config var**.

- **Durable stats live in Redis, in the store.** The engine's `engine_cache_*`
  Prometheus counters are per-worker and per-process, so the API (which never
  runs a step) cannot read them. The store therefore keeps its own per-plugin
  counters: `redisstore.Get` increments `hits` or `misses`, `Set` increments
  `stores`, in a hash `<prefix>:stats:<kind>:<name>` (one pipelined
  `HINCRBY` + `PEXPIRE` re-arming a 30-day TTL so idle plugins self-clean).
  The increments are **best-effort** — a failed counter update is swallowed and
  never touches the read, the write, or the step. They mirror the Prometheus
  counters on the normal path (one store op ⇒ one engine count), which is what
  lets the stats endpoint reconcile against them; the one documented
  divergence is a *corrupt stored value*, which the store counts as a hit
  (a value was present) but the engine counts as a fail-open (it couldn't
  decode it) — operator-tampering/version-skew territory. `bypass` and
  `fail_opens` stay Prometheus-only (they never reach the store).

- **Bust is SCAN + UNLINK by namespace.** `redisstore.Bust(BustMatch)` builds a
  Redis glob from `cache.BustPattern` — `<prefix>:v*:*` (all), `…:<kind>:*`
  (one kind), or `…:<kind>:<name>:*` (one concrete plugin) — and `SCAN`s in
  batches (`COUNT 512`), `UNLINK`ing each batch so both the scan and the delete
  are non-blocking. It returns the deleted count. Semantics are point-in-time:
  an entry a live worker writes after the scan passed its slot survives (a
  bust is a best-effort reclaim, not a barrier). The pattern matches `v*` so a
  bust also sweeps entries stranded behind an earlier `KeySchemaVersion`, and
  the `stats:` namespace is excluded by construction (it does not begin with
  `v`), so counters survive a bust. **A single run's entries cannot be busted**
  — a run-scoped entry mixes the run id into the *hash*, not the key — so its
  only bound is the TTL; this is documented, not a gap.

- **The API surface.** Two new **admin-scoped** routes (route→scope and
  route→class tables extended, OpenAPI still lints 100/100):
  `POST /v1/cache/bust` (namespace selector `{plugin_kind?, plugin_name?}`;
  a name without a kind, or a non-cacheable kind, is a 400) returns
  `{deleted}` and emits a structured **audit log line** carrying the actor
  `key_id` (ADR-007), the namespace, and the count; `GET /v1/cache/stats`
  returns per-plugin `{hits, misses, stores, hit_rate}`. Both answer **503
  `cache_unavailable`** when the cache is not wired.

- **The API's Redis use, revisited.** ADR-002 said the API holds no Redis
  client for dispatch; 6.4 already carved out an opt-in, fail-soft client for
  rate-limit buckets. 9.6 reuses that one client for the cache ops surface
  (`cmd/api` builds a `redisstore` over it when `AGENTLOOM_CACHE_ENABLED`).
  The API still never reads a cached *result* or dispatches — this is an ops
  surface only, fail-soft, and Postgres stays the API's sole hard dependency.

- **`ctl cache bust|stats`.** `bust --kind/--name` (no flags = bust all) prints
  the count; `stats` renders the per-plugin table with hit rates. The
  ops-runbook (`docs/ops-runbook.md`) documents when to reach for each
  invalidation mechanism (prefer a plugin version bump for behavioral changes;
  bust for corpus re-ingest or emergencies; TTL for everything else).

## Consequences

- **Repeat deterministic work becomes a lookup.** The headline win: identical
  `temperature=0` llm steps, pure tools, and opted-in retrievals collapse to a
  Redis GET, skipping the provider and the rate limiter entirely.
- **The key is only as correct as the version discipline.** Components 4/5
  make caching safe *if* plugin authors honor ADR-009's bump rule. A behavior
  change shipped without a version bump serves stale outputs until TTL — the
  cost of keying on version equality rather than hashing plugin code. The flag
  table and version strings are conformance-tested (ADR-009); the bump remains
  a human judgment.
- **Non-determinism is off by default, which lowers the hit rate but never
  lies.** A `temperature>0` step is uncached unless the author explicitly
  accepts that a cached completion may not match a fresh one. This is the
  intended trade: correctness over hit rate, opt-in for the rest.
- **Cache errors never fail a run, but a Redis outage means zero hits.** The
  fail-open stance keeps the fleet correct when Redis is down, at the cost of
  every call going to the provider (and the rate limiter) meanwhile — the same
  posture ADR-010 takes for limits.
- **The size cap and TTL are dev-scale defaults.** 1 MiB and the fleet default
  TTL are guesses tuned for the demo; a production deploy will revisit them,
  and the skip/evict behavior is observable so they can be tuned from data.

## Alternatives considered

- **Cache in Postgres.** Rejected: a cache is disposable derived data, so it
  does not belong in the source of truth; Postgres has no native TTL (a sweep
  job would be needed); and a read on the hot claim path wants Redis latency,
  not a table scan. Redis loss is recoverable by re-computation — exactly the
  "any Redis data loss must be recoverable via the reconciler" invariant,
  since a cache miss *is* the recovery.
- **Key on the authored config, not the resolved request.** Rejected: two
  configs can resolve to the same request (a defaulted `max_tokens`, a model
  alias) and should share an entry; and the resolved request is what actually
  determines the output. Keying on config would both miss legitimate hits and
  risk keying on a field that does not reach the provider.
- **Assume determinism and cache everything cacheable.** Rejected outright:
  this is the dangerous naive cache. `temperature>0` is non-deterministic, and
  serving a memoized completion for it silently corrupts workflow semantics.
  Determinism-gated default + explicit opt-in is the whole point of the design.
- **`0x00`-delimited key components (the 6.5 fingerprint scheme).** Rejected in
  favor of length-prefix framing: a delimiter is ambiguous if a component's
  bytes can contain it, and a cache-key collision serves a wrong output — worth
  the few extra bytes to make boundaries unforgeable.
- **Content-addressed invalidation (hash the plugin code / corpus into the
  key).** Rejected as over-engineering for v1: plugin versions already carry
  the behavioral-change signal, and the corpus is unversioned. Revisit if
  version discipline proves too coarse in practice.
- **A `definition` cache scope.** Deferred, not rejected: no current milestone
  needs "share within a workflow's runs but not across workflows," and it is an
  additive scope value later without a key-format bump.
