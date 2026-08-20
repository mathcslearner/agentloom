# ADR-010: Fleet-wide rate limiting & backpressure

- **Status:** Accepted
- **Date:** 2026-08-14
- **Ticket:** ticket 9.1

## Context

Every worker in the fleet calls the same external resources — one Anthropic
account, one OpenAI account, a handful of rate-limited tool endpoints. Those
providers meter two independent dimensions: **requests per minute** and
**tokens per minute**. A single worker respecting a limit is meaningless when
four others are hammering the same account; the only correct place to enforce
a shared limit is a shared ledger. ADR-007's Redis token bucket (ticket 6.3)
was built to be that ledger — deliberately tenant-agnostic, so 6.4 keys it per
API key and M9 keys it per provider resource. 6.3 also left two contract edges
(`ErrCostExceedsCapacity`, `RetryAfterNever`) "for M9's wait-vs-perm-fail
distinction". This is that milestone.

The forces in tension:

- **A rate-limit denial is not a failure.** A step that would exceed the
  provider's requests/min budget has done nothing wrong; retrying it as a
  `transient` failure (ADR-006) would burn its retry budget, dead-letter a
  healthy step after three throttles, and pollute the failure metrics an
  operator reads during an incident. Throttling must be orthogonal to the
  failure taxonomy.
- **A worker slot must never block on a limit.** The naive implementation —
  acquire, and if denied `sleep` until tokens exist — holds a worker goroutine
  (and its lease) hostage to another resource's contention. With a bounded
  worker pool, a few throttled LLM steps would starve every other run's
  throughput. Backpressure has to *release* the slot, not park it in a sleep.
- **Two dimensions must be enforced together, atomically.** A request that
  passes the requests/min check but fails the tokens/min check must consume
  *neither* token — otherwise a sustained token-limited workload leaks one
  request token on every denial, and the two ledgers drift apart. 6.4 accepted
  a single-token no-refund skew because its denials are rare abuse; M9's
  denials are steady-state under load, so the skew would be systematic.
- **The token cost is only an estimate before the call.** Token usage is
  known *after* the provider responds. The limiter must debit a pre-call
  estimate, or a fan-out of large-context steps all pass the check
  simultaneously and blow the real budget. The estimate error is then
  reconciled post-call (ticket 9.3).
- **Limits are protective, not correctness.** Postgres is the source of
  truth; a rate limit is a courtesy to the provider and a cost guardrail.
  When Redis is down, or a model has no configured limit, the fleet must keep
  running — the same fail-open stance 6.4 took for API rate limits.

This ADR decides the semantics; tickets 9.2 (limiter middleware) and 9.3
(token reconciliation) implement against it. 9.1 additionally delivers the
resource-limit configuration (loading + validation).

## Decision

### Named resources

A **resource** is a named external-capacity pool that many workers share. Its
name is `<provider>:<identifier>`:

- `anthropic:<model>`, `openai:<model>`, `mock:<model>` — named by the
  **resolved** provider, so a step's `model: "mock/sim-1"` (8.4's namespace
  form) and `model: "claude-sonnet-5"` (8.4's vendor-prefix routing) both
  resolve to a provider before the resource name is formed:
  `mock:sim-1`, `anthropic:claude-sonnet-5`. The limiter keys off what the
  provider actually meters, not off how the workflow author spelled the model.
- `tool:<name>` — a custom resource a rate-limited tool declares (e.g.
  `tool:http_request` for a shared third-party endpoint). Pure tools
  (`json_transform`) declare none.

Resource names are the metric label `resource` (ADR-008): they come from a
finite operator-authored config, so their cardinality is bounded and they are
a legal label (unlike `run_id`/`step_id`).

### Resolution & the unknown-resource policy

`Set.Lookup(name)` resolves in this order:

1. **Exact match** — `anthropic:claude-sonnet-5`.
2. **Provider wildcard** — `<provider>:*`, e.g. `anthropic:*`, matching every
   model of a provider with one limit. `*` is admissible in config *only* as a
   trailing `:*` segment.
3. **Not found → unlimited.** A resource with no matching config entry is
   **not rate limited** — the step skips the limiter entirely.

Not-found-means-unlimited is the deliberate unknown-resource policy. Limits
are protective opt-in: a deployment names the resources it wants governed and
leaves the rest unbounded. The alternative — fail-closed, treating an unknown
resource as zero capacity — would brick the fleet the first time a workflow
used a new model name before an operator added its limit, converting a config
omission into a total outage. This mirrors 6.4's fail-open philosophy: a rate
limit never becomes a correctness dependency.

### Dual buckets per resource

Each configured resource carries up to two independent limits, each a 6.3
token bucket:

- **Requests** — cost 1 per provider call. Key `<prefix>:<resource>:requests`.
- **Tokens** — cost = the pre-call token *estimate*. Key
  `<prefix>:<resource>:tokens`.

A resource may configure either dimension alone (e.g. a tool endpoint limited
by requests only); a nil dimension is unbounded. The config unit is
**per-minute** for human legibility; the bucket runs per-second
(`refill_per_sec = per_minute / 60`) with capacity = the configured `burst`,
defaulting to one minute of refill (`ceil(per_minute)`) when unset — so an
un-bursted bucket tolerates exactly one minute's steady rate as its ceiling.
The key prefix is configurable for test isolation, exactly as 6.4's
`AGENTLOOM_API_RATELIMIT_KEY_PREFIX` is.

### The atomic dual acquire

The two buckets are acquired **all-or-nothing in one atomic step**: both
succeed and both debit, or neither debits and the call is denied. This is the
two-key Lua script 6.3 deferred ("if the two sequential acquires measurably
matter, the honest fix is a second Lua script taking two keys"). M9 is where
it stops being deferrable: unlike 6.4 — where a denial is rare abuse and the
accepted no-refund skew is negligible — M9 denials are steady-state under
load, so a sequential `acquire(requests); acquire(tokens)` would leak a
request token on every token-bucket denial and the two ledgers would drift.
The script (landing in `internal/ratelimit` in 9.2, keeping the library
tenant-agnostic — the caller still supplies both `Bucket`s and costs) refills
both buckets against the one Redis clock, grants only if both have the tokens,
and on denial reports `retry_after = max(requests_retry_after,
tokens_retry_after)` — the time until *both* dimensions can admit the call.

### Backpressure: the `throttled` outcome

A denial does not fail the step. It records a new **administrative attempt
outcome `throttled`** — the second such outcome after `lost` (ADR-006), and
like `lost` deliberately *outside* the error-class taxonomy:

- It is **never counted against the retry budget.** `CountCountedFailures`
  counts `transient`/`timeout` only, so `throttled` needs no query change — a
  step may be throttled any number of times without consuming an attempt. This
  is the ADR-006 acceptance criterion "attempt counter unchanged by
  throttles" (9.2).
- It is **never in `retry_on`** and never judged by `classifyFailure`; the
  limiter decides it structurally, before the executor is ever invoked.
- The transition **reuses 5.2's retry machinery wholesale**: a claim-fenced
  CAS `running → retrying`, stamping `next_attempt_at = now + delay`, clearing
  the claim, and appending a `step_throttled` event (payload: resource, which
  bucket denied, `retry_after`) — but **not** bumping `steps_failed`. The
  worker ACKs after the commit and moves on; the slot is released immediately.
  Nothing anywhere in this design waits in-process for tokens.
- The delayed re-dispatch rides the ADR-005 delayed-delivery ZSET under a new
  reason **`throttle`** (added to ADR-005's vocabulary by 9.2, as `retry`,
  `unpark`, and `dlq_requeue` were added by their owning tickets). The
  envelope is built without `EnqueuedAt` so identical `(run, step)` re-queues
  encode to byte-identical ZSET members — ZADD's move-the-fire-time dedup, the
  same property the retry route relies on.
- The **commit-then-schedule crash gap is already healed** by the reconciler's
  overdue-`retrying` scan (5.2): that scan is keyed on status + `next_attempt_at`,
  not on the reason a step became `retrying`, so a throttled step whose delayed
  entry was lost to a crash is re-dispatched with no new reconciler duty. The
  claim CAS's `next_attempt_at ≤ now` guard is what keeps an early delivery
  from executing before the backoff elapses.

`throttled` joins the `step_attempts.outcome` CHECK constraint in 9.2's
migration (mirroring how 5.2 added the classed vocabulary); until then it is
reserved by this ADR and ADR-006's cross-update, unused in code.

### Requeue math

The delay before a throttled step's re-dispatch is:

```
delay = clamp(retry_after, floor, cap) + U[0, jitter_frac × clamped]
```

- `retry_after` is the limiter's own computed time-to-tokens (per-cost, the
  max across the two buckets) — not a blind exponential. It is the honest
  earliest moment the call could succeed.
- **`floor`** (default 500ms) guards against a hot requeue loop: a
  token-trickle denial can report a near-zero `retry_after`, and re-dispatching
  that immediately would spin the step through claim→deny→requeue with no
  progress. The floor forces a minimum breather.
- **`cap`** (default 5m) bounds the wait so a badly-misconfigured tiny limit
  does not strand a step for hours; the reconciler's overdue scan is the
  backstop.
- **`jitter_frac`** (default 20%) adds *partial, additive* jitter on top of
  the computed delay. This is deliberately **not** ADR-006's AWS full jitter
  (`U[0, computed]`): `retry_after` is a real refill deadline, so waking a
  fan-out of siblings uniformly across `[0, retry_after]` would send most of
  them *before* the tokens exist, guaranteeing a second denial. Additive
  jitter de-correlates the thundering herd of fanned-out siblings retrying
  against one bucket while still respecting the deadline.

The floor / cap / jitter fraction are 9.2 config knobs; the defaults and the
formula are decided here.

### Wait-vs-never: when a throttle is really a permanent failure

Two denials can never be lifted by waiting, and must **not** become an endless
throttle loop. They are permanent failures routed to the DLQ (ADR-006):

- **`ErrCostExceedsCapacity`** — the token estimate exceeds the token bucket's
  capacity. No amount of refill admits it; the step's context is simply larger
  than the configured burst. 6.3 already rejects this Go-side before touching
  Redis. → force-classified `permanent`, dead-lettered (source `permanent`).
- **`RetryAfterNever`** — a denial from a never-refilling bucket
  (`refill_per_sec == 0`, a fixed quota). The missing tokens never come back.
  → `permanent`, DLQ. (v1 config does not admit rate-zero buckets — see
  Configuration — so this is defensive; the sentinel exists because the shared
  6.3 library can produce it.)

Both are deterministic functions of the estimate and the config, so
re-execution is provably futile — the ADR-006 rows-4-7-15 pattern.

### Where the resource binding comes from (9.2)

A step binds to a resource, an estimate, and a cost through a new optional
executor hook, designed here and implemented in 9.2. Cost-bearing executors
(the llm executor; a rate-limited tool) implement:

```go
// ResourceClaim is what an executor tells the limiter middleware before
// its provider call: the named resource and the estimated token cost.
type ResourceClaimer interface {
    ResourceClaim(sc StepContext) (resource string, estTokens int64, err error)
}
```

An executor that does not implement it (noop, echo, sleep, pure tools) bypasses
the limiter entirely — the middleware is a no-op for steps that name no
resource. The estimate was a **rough** chars/4 + declared `max_tokens` heuristic
in 9.2; **12.6 replaced the input half with the real `internal/tokens` counter**
(`CountRequest` over the fully framed request), so the debit tracks real usage
(exact on the mock and OpenAI, calibrated on Anthropic). 9.3 still reconciles the
post-call `actual − estimate` on the token bucket so any residual bias cannot let
the fleet drift past the provider's real budget over time — the reconciliation
histogram simply tightens toward zero once the estimator is exact.

### Fairness stance (documented, deferred)

Global per-resource buckets plus fire-time-ordered re-dispatch mean a single
huge fan-out run can monopolize a resource: its hundreds of throttled steps
re-queue and reclaim tokens as fast as they refill, starving a small
concurrent run of the same model. The remedy, **if load tests demand it**
(M19), is a per-run throttle cap: count consecutive throttles per run against
the resource, and above a threshold **park** the run (a new `park_reason`,
`resource_starved`, on 5.6's park primitive — which holds no lease or worker
slot) until the pressure clears, giving other runs a fair share of the bucket.
This is deliberately *not* built in v1: it adds a per-run counter and a
park/unpark cycle whose thresholds only real contention can tune, and the
global bucket already enforces the provider's actual limit — the only property
that is a hard requirement. The fairness gap is a throughput-distribution
concern, not a correctness one, and is recorded here so the M19 load campaign
knows exactly what to measure and what to add.

### Not limited: the M11.5 `llm_judge` (documented gap)

The `llm_judge` validator (ADR-013, M11.5) calls a provider from the engine's
**validate stage**, which sits after the executor and is not routed through
the `ResourceClaimer` middleware. So a judge's provider call is **not**
back-pressured by the fleet limiter in 11.5: a workflow that judges every
output doubles its provider traffic against limits the limiter does not see.
This is an accepted limitation, not a correctness bug — the judge is priced
and budget-metered (as overhead), so cost governance applies; only the
rate-limit dimension is missing. Wiring the judge through a resource binding
(its own `resource` key, `<judge-provider>:<judge-model>`) is a follow-up the
M19 load campaign can pick up if judge traffic proves to need it.

### Configuration format & loading (this ticket, 9.1)

Resource limits are a JSON document, supplied to the worker either inline
(`AGENTLOOM_RESOURCES`) or as a file path (`AGENTLOOM_RESOURCES_FILE`) —
**exactly one** of the two, rejected at config load if both are set. Neither
set means no configured limits: every resource is unlimited. The config
package stays env-pure (it holds the two strings and the mutual-exclusion
check); the JSON is parsed and validated by the leaf package `internal/limits`
(stdlib only — the 9.2 middleware maps its output onto `ratelimit.Bucket`, so
`limits` imports neither `ratelimit` nor any engine package).

```json
{
  "resources": [
    {
      "name": "anthropic:claude-sonnet-5",
      "requests": {"per_minute": 60},
      "tokens":   {"per_minute": 200000, "burst": 400000}
    },
    {"name": "openai:*", "requests": {"per_minute": 120}}
  ]
}
```

Validation (strict decode — unknown fields rejected — with every error
reported at once, the config package's house style):

| Field | Bound |
|---|---|
| `name` | required; no whitespace; `*` only as a trailing `:*` segment; unique across the set |
| resource | at least one of `requests` / `tokens` present |
| `per_minute` | required per rate; strictly positive and finite (no fixed-quota buckets in v1 — a rate-zero limit is a `RetryAfterNever` brick, never a sane governance limit, exactly as 6.4 forbids rate-zero API buckets) |
| `burst` | optional; when present, ≥ 1; absent = one minute of refill |

### Metrics (9.2)

The middleware records throttle count and queue-wait time **by `resource`**
(the bounded label above); 9.3 adds an estimate-error histogram. The counters
feed M10's "saved" accounting indirectly (a throttle is deferred spend, not
avoided spend). Instrument names follow ADR-008's `engine_<subsystem>_...`
convention under a new `ratelimit` subsystem.

**As built (9.2):** the subsystem is `ratelimit`, with
`engine_ratelimit_throttled_total{resource, bucket}` (bucket ∈
`requests`/`tokens`/`both`), `engine_ratelimit_throttle_wait_seconds{resource}`
(the clamped+jittered re-dispatch delay — the queue-wait a throttle adds), and
`engine_ratelimit_fail_opens_total` (a limiter error after which the step
proceeded unlimited). `resource` joins ADR-008's label allowlist. The two-key
atomic acquire ships as `ratelimit.AcquireDual` (a second Lua script beside the
single-key `Acquire`); the resource→bucket mapping and the unknown-resource
skip live in the leaf adapter `internal/ratelimit/resource`, which the engine
consults through the `ResourceLimiter` seam. The `throttled` outcome joins the
`step_attempts.outcome` CHECK in migration 0014, and the store transition
`ThrottleStep` reuses 5.2's `RetryRunStep` CAS verbatim (no new SQL). The
requeue-math knobs (`floor`, `cap`, `jitter_frac`) are worker config
(`AGENTLOOM_RESOURCES_THROTTLE_*`); the key prefix is
`AGENTLOOM_RESOURCES_KEY_PREFIX`.

### Token-cost reconciliation (as built, 9.3)

The limiter debits a pre-call token *estimate* (chars/4 + declared
`max_tokens`); the true usage is known only after the provider responds. A
biased estimator — one that is systematically low — would let the fleet admit
more real tokens than the provider's budget on every call, and the error
compounds under sustained load. 9.3 closes the gap: after a granted,
token-metered call returns with its real usage, the middleware corrects the
token bucket by `delta = actual − estimate`.

- **The correction is a third Lua script, `ratelimit.Adjust`**, beside the
  single-key `Acquire` and the two-key `AcquireDual`. It refills the bucket to
  the Redis clock exactly as the acquire scripts do (same `{tokens, ts}` hash,
  absent-key-=-full, backwards-clock clamp, `%.17g` exact state), then applies
  `tokens -= delta`. The asymmetry is the whole design:
  - A **positive** delta (under-estimate) is an extra debit and is
    **unclamped** — it may drive the balance negative. A negative balance is
    the enforcement: the *unchanged* acquire scripts already deny while
    `cost > tokens` and grow `retry_after` as `(cost − tokens)/rate`, so
    subsequent acquires throttle until refill pays the debt back. No
    acquire-script change was needed.
  - A **negative** delta (over-estimate) is a refund and **clamps at
    capacity** — a bucket is never fuller than full, mirroring the refill
    clamp.
  The TTL rule composes with a negative balance for free: time-to-full
  `(capacity − tokens)/rate` grows past a minute of refill, so the debt
  survives in Redis until it is actually paid off (an early expiry, absent-key
  = full, would erase it). Rate-zero buckets `PERSIST`, unchanged.
- **The adapter reconciles only the token bucket.** `resource.Reconcile(ctx,
  name, est, actual)` re-resolves the name (exact → wildcard → unlimited, as
  `Acquire` does) and `Adjust`s the tokens bucket alone — the requests cost of
  1 is exact, never reconciled. An unlimited resource, a resource with no token
  dimension, or a zero delta reconciles nothing and touches no Redis.
- **Reconcile only what was debited, and only on real usage.** The
  `resource.Decision` grew a `TokensMetered` flag (true only for a dual acquire
  or a tokens-only single acquire). The engine middleware reconciles iff the
  acquire was *granted and token-metered* **and** the executor returned a usage
  report (`exec.Output.Usage != nil`). An errored call carries no usage, so its
  estimate stays debited — deliberately conservative: a failed call's true
  spend is unknowable, and leaving the estimate on the ledger protects the
  provider budget. A success that raced its timeout or a cancellation still
  reconciles — the tokens were spent regardless of how the completion is
  routed. The shutdown-abandon path (canceled handler context, no completion)
  skips it, matching its no-completion contract.
- **Reconciliation happens right after the executor returns, before the
  completion transaction.** The provider call *happened*; even a later fenced
  completion does not un-spend the tokens. A zombie and its takeover each
  reconcile against their own acquire, so there is no double-count.
- **Fail-open, like everything else here.** A `Reconcile` error (Redis
  unreachable) is logged and counted and never affects the step's outcome — a
  rate limit is never a correctness dependency.

**Metrics.** `engine_ratelimit_estimate_error_tokens{resource}` is a histogram
of the signed error `actual − estimate` (roughly log-scaled, symmetric about
zero), so a systematically biased estimator shows as a skewed distribution;
this is the first histogram whose unit is `_tokens` rather than `_seconds`
(ADR-008's unit table amended). `engine_ratelimit_reconcile_failures_total`
counts fail-open reconciliations. Both label by the resolved config-entry name
(the bounded `resource` label), never the raw `<provider>:<model>`.

**Tests.** `ratelimit`: an `Adjust` matrix (refund clamps at capacity; debit
crosses into negative and the debt throttles later acquires with a
correctly-grown `retry_after`, recovering after refill; rate-zero PERSIST;
debt-TTL outlives time-to-full) and a **conservation property test**
(`TestReconcileConservationProp`) — random interleavings of `AcquireAt` /
`AdjustAt` at non-decreasing injected times must match a pure-Go model's exact
float64 balance at every step. `resource`: no-op paths (unlimited /
requests-only / zero delta skip Redis) and the token-only debit/refund with the
requests ledger proven untouched. `engine`: the middleware passes the exact
`(est, actual)` and records the histogram, a non-token-metered grant is never
reconciled, the fail-open path, and the headline
`TestFleetActualTokensRespectResourceLimit` — 4 workers, a biased-3×-low
estimator, cumulative *actual* tokens held within `burst + refill × elapsed +
in-flight slack` (a bound that would be violated 3× over without
reconciliation). No migration, no config, and no store change in 9.3.

## Consequences

Positive:

- The fleet respects one shared provider budget regardless of worker count —
  the mandated distributed-systems property, enforced in one atomic Redis step.
- Throttling is orthogonal to failure: a rate-limited step neither burns its
  retry budget nor shows up as a failure in metrics or the DLQ, so an operator
  reading failures sees real problems, not backpressure.
- No worker slot ever blocks on a limit — a throttled step releases its slot
  and its lease immediately, so contention on one resource cannot throttle the
  throughput of runs using a different one.
- The `throttled` outcome and the retry-machinery reuse mean **zero new store
  primitives and no new reconciler duty** — the delayed ZSET, the
  `running → retrying` CAS, the `next_attempt_at` claim guard, and the overdue
  reconciler scan all already exist and all already do the right thing.
- Wait-vs-never (the 6.3 contract edges) means a genuinely impossible request
  (context bigger than the burst, or a dead quota) dead-letters instead of
  looping forever.

Negative:

- **Estimated token debiting is provisional** and can be wrong per call —
  addressed by 9.3's reconciliation, but between the estimate and the
  reconcile the bucket balance is approximate.
- **Global buckets are not fair across runs** — one big fan-out can starve a
  small run of the same resource until the (deferred) per-run cap exists. The
  hard limit (the provider's budget) is always respected; only the
  distribution is unfair, and only under contention.
- **The two-key atomic script is a second Lua script to maintain** alongside
  6.3's single-key acquire — the honest cost of not letting the two ledgers
  drift.
- **`throttled` is a second administrative outcome outside the taxonomy**, so
  every reader of `step_attempts.outcome` must now know two non-class outcomes
  (`lost`, `throttled`) rather than one. Bounded, and documented in ADR-006.
- **Not-found-means-unlimited can silently under-govern**: a typo in a
  resource name (or a model whose limit an operator forgot to add) runs
  unlimited rather than erroring. Accepted as the safe direction — the failure
  mode is "we respected the provider less than intended", not "the fleet
  stopped" — and observable via the throttle metrics being absent for a
  resource that should have them.

## Alternatives considered

- **Deny → `transient` failure** (route a throttle through the ordinary retry
  engine). Rejected: it burns the retry budget (a step throttled three times
  dead-letters despite nothing being wrong), pollutes failure metrics and the
  DLQ with backpressure, and couples a healthy step's fate to unrelated
  contention. The whole point of a distinct `throttled` outcome is that a
  limit is not a failure.
- **Block the worker in-process until tokens exist** (`acquire`, else
  `sleep(retry_after)`). Rejected as the worst option: it holds a bounded
  worker slot and its lease hostage to another resource's contention, so a
  handful of throttled LLM steps starve every other run's throughput. Releasing
  the slot via delayed re-dispatch is the entire backpressure design.
- **Rely on provider 429s alone** (no pre-emptive limiter; let the provider
  reject and retry the 429 as `transient`). Rejected: reactive, wasteful (every
  over-limit call is a wasted round trip and a burned retry attempt), and it
  cannot budget *tokens* at all — a provider 429 comes after the tokens are
  already spent. Provider 429s remain possible (external contention,
  mis-set limits) and keep their ADR-006 `transient` route; the limiter exists
  to make them rare, not to replace them.
- **Per-worker local limits of R/N** (divide the fleet limit by worker count).
  Rejected: it drifts the moment workers scale up or down or load is uneven,
  under-utilizes the budget when some workers are idle, and over-shoots it when
  the count is stale. A shared ledger is the only correct enforcement point.
- **Sequential non-atomic dual acquire** (`acquire(requests)` then
  `acquire(tokens)`, refund on second denial). Rejected: the refund is itself a
  non-atomic third round trip that can be lost to a crash, and 6.4's
  no-refund variant leaks a token per denial — negligible for rare abuse,
  systematic for steady-state throttling. One atomic two-key script is the
  clean fix and is exactly what 6.3 anticipated.
- **Fail-closed on unknown resources** (unknown = zero capacity). Rejected:
  converts a config omission (a new model name, a forgotten limit) into a total
  outage. Limits are protective opt-in; the safe direction is unlimited.
- **Fixed-quota (rate-zero) buckets in v1 config.** Rejected: a rate-zero
  limit denies forever once drained (`RetryAfterNever`), which is never a sane
  governance limit — it is a brick. The shared 6.3 library still supports them
  (6.4 uses `PERSIST` for them), and this ADR handles the sentinel defensively,
  but the M9 resource config forbids them, exactly as 6.4 forbids rate-zero API
  buckets.
