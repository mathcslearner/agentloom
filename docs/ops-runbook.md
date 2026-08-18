# Operations runbook

Operational procedures for running agentloom. This is seeded by the response
cache's invalidation surface (ticket 9.6) and grows with later milestones
(M20/M21 add deploy/teardown procedures).

## Response cache invalidation

agentloom's response cache (ADR-011) serves the stored output of a
deterministic step — an LLM call at `temperature: 0`, a pure tool like
`json_transform`, an opted-in retrieve — from Redis, ahead of the rate
limiter, so an identical repeat request skips the provider entirely. The
cache is **disposable derived data**: losing it costs a re-computation, never
correctness. Every read and write is fail-open, so a Redis outage degrades to
"every step executes uncached", never a run failure.

There are three ways an entry leaves the cache. Reach for them in this order.

### 1. TTL (the default; nothing to do)

Every written entry carries a TTL — the step's `cache.ttl` if set, otherwise
the fleet default (`AGENTLOOM_CACHE_DEFAULT_TTL`, 24h by default, capped at 30
days). Idle entries self-evict. If a cached result only needs to be fresh
"within a few hours", author a short `cache.ttl` on the step and let Redis do
the work — no operator action, no bust.

### 2. Version bump (preferred for behavioral changes)

The cache key includes the concrete plugin's **behavioral version** and the
key-builder's `KeySchemaVersion` (ADR-009/011). When a plugin's behavior
changes in a way that should invalidate cached outputs — a new prompt
template, a provider upgrade that changes responses, a tool rewrite — bump the
plugin's version. Old entries become **unreachable** (nothing will ever build
their key again) and TTL out on their own. This is the cleanest invalidation:
no scan, no window, no coordination. Prefer it whenever the change is a code
or config change you control.

### 3. Admin bust (for corpus re-ingest and emergencies)

When there is no version to bump — most commonly a **retriever whose corpus
was re-ingested** (the corpus is not versioned, so the retrieve key doesn't
change) — bust by namespace. Also the right tool for an emergency ("a bad
response got cached; drop it now").

Granularity is the Redis key namespace: **all** entries, **one plugin kind**
(all model providers / all tools / all retrievers), or **one concrete
plugin**. A single run's entries cannot be busted — a run-scoped entry
(`cache.scope: run`) mixes the run id into the entry hash, not the key, so its
only bound is its TTL.

Bust with `ctl` (needs an `admin`-scoped key):

```bash
# One concrete plugin — e.g. after re-ingesting the pg_fulltext corpus:
ctl cache bust --kind retriever --name pg_fulltext

# A whole kind:
ctl cache bust --kind model_provider

# Everything under the cache prefix:
ctl cache bust
```

Or over the API directly:

```bash
curl -sS -X POST "$AGENTLOOM_API_URL/v1/cache/bust" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"plugin_kind":"retriever","plugin_name":"pg_fulltext"}'
# → {"deleted": 128}
```

**Safe on a live fleet.** The bust uses Redis `SCAN` + `UNLINK`, so neither
the scan nor the delete blocks Redis, and workers that are mid-execution are
unaffected — a cache miss just re-computes. The action is **audit-logged**
with the actor's key id, the namespace, and the deleted count.

**Point-in-time, not a barrier.** The `deleted` count is what the scan found.
An entry a live worker writes *after* the scan passed its slot survives the
bust — busting does not fence concurrent writers. If you need certainty that a
specific stale entry is gone (not merely that a bust ran), bust again, or rely
on the version bump above, which no future write can resurrect.

**Counters survive a bust.** The per-plugin hit/miss/store statistics live in
a separate Redis namespace, so a bust removes entries without resetting the
cumulative counters.

## Reading cache statistics

`GET /v1/cache/stats` (admin) reports per-plugin cumulative counters and the
derived hit rate:

```bash
ctl cache stats
# KIND            NAME            HITS  MISSES  STORES  HIT RATE
# model_provider  mock            412   88      88      82.4%
# retriever       pg_fulltext     30    120     120     20.0%
```

These counters are kept in Redis by the worker fleet's cache store, so the API
serves them without a worker. They **reconcile against** the fleet's
`engine_cache_{hits,misses,stores}_total` Prometheus counters on the normal
path — use the endpoint for a quick per-plugin view, and the Prometheus
series (and the Grafana panels, `docs/observability.md`) for rates over time
and alerting.

A low hit rate on a plugin you expected to cache well usually means one of:
non-deterministic requests (an LLM step above `temperature: 0`, which bypasses
by default — opt in with `cache: {mode: read_write}` only if you accept
serving one sampled response for all callers), a churning input (rendered
prompts that differ every run), or a TTL shorter than the repeat interval.

## Disabling the cache

Set `AGENTLOOM_CACHE_ENABLED=false` on the worker fleet to run every step
uncached (the API's `/v1/cache/*` routes then answer `503 cache_unavailable`).
Because the cache is fail-open, you rarely need this — a misbehaving Redis
already degrades to uncached execution on its own.

## Model context windows (ticket 12.6)

The provider-window guardrail keeps an llm step's assembled context plus its
completion `max_tokens` under the model's context window, so a provider
`context_length_exceeded` 400 is unreachable by construction — but only for
models the pricing catalog gives a window. Windows live in the same catalog as
rates (ADR-012): each model entry may carry a `context_window` (a positive token
count), resolved exact → `<provider>:*` wildcard → miss.

The embedded defaults window the real families (Anthropic 200k, OpenAI gpt-5
400k / o3 200k / `openai:*` 128k) and the mock models. A model with **no**
window — no entry, no wildcard window — is **unguarded**: its requests are never
window-checked, exactly as an unpriced model is never rate-limited. So the fix
when you see a `context_window_exceeded` dead-letter (or want a private model
guarded) is to add the window to your pricing override:

```json
{
  "schema_version": 1,
  "models": [
    {"name": "anthropic:my-model", "effective_from": "2026-01-01",
     "context_window": 200000, "input_per_mtok": 3.0, "output_per_mtok": 15.0}
  ]
}
```

Set it via `AGENTLOOM_PRICING` (inline) or `AGENTLOOM_PRICING_FILE` (path) on the
worker fleet — the same override that sets rates. A larger `context_window`
raises the default budget (window − `max_tokens` − 5% headroom) that a
context-bearing step with no explicit `budget_tokens` auto-compacts to; an
author who wants a tighter budget sets `context.budget_tokens` (it may only
*tighten* the window default, never loosen it). Watch the **Context** row on the
Engine dashboard: `engine_context_window_rejections_total` should sit at zero,
and `engine_context_utilization_ratio` p95 should stay below 1.0.

## Approval-notification webhooks (ticket 15.5)

A parked `human_approval` step can page an external endpoint. Set on the worker
fleet:

```bash
AGENTLOOM_NOTIFY_WEBHOOK_URL=https://hooks.example.com/agentloom/approvals
AGENTLOOM_NOTIFY_WEBHOOK_SECRET=<shared secret>   # required alongside the URL
# AGENTLOOM_NOTIFY_WEBHOOK_TIMEOUT=5s
# AGENTLOOM_NOTIFY_WEBHOOK_MAX_ATTEMPTS=3
```

Each new pending approval POSTs a JSON body (`schema_version: 1`, the approval,
its run, and `/v1/approvals/...` links) with these headers:

- `X-Agentloom-Event: approval.requested`
- `X-Agentloom-Timestamp: <unix seconds>`
- `X-Agentloom-Signature: v1=<hex HMAC-SHA256(secret, "<timestamp>.<body>")>`
- `X-Agentloom-Delivery-Id: <approval id>`

**Verify** every request in constant time over the raw body, and **dedupe** on
the delivery id (a delivery may arrive more than once in the rare crash window
between the POST and the journal commit — the id is stable across a delivery's
retries). A worker's Go receiver can call `notify.Verify` / `notify.VerifyWithin`
directly.

Delivery is **best-effort and effectively-once**: retries are capped
(`MAX_ATTEMPTS`), a 4xx is permanent, and a failure records an
`approval_notification_failed` event and increments
`engine_approval_notifications_total{result="failed"}` — the run stays parked
and fully decidable via `POST /v1/approvals/{id}:decide` regardless. A webhook
outage never affects run correctness; `GET /v1/approvals?status=pending` is the
source of truth for what is awaiting a decision.

## Tailing a run's live event feed (ticket 16.2)

After each transaction commits, the worker and API fan the new event envelopes
out to Redis pub/sub best-effort: a per-run channel `<prefix>:run:{id}` and a
firehose `<prefix>:firehose` (`<prefix>` is `AGENTLOOM_EVENTS_CHANNEL_PREFIX`,
default `events`). The durable truth is always the Postgres event log
(`GET /v1/runs/{id}` and the M16.3 WebSocket); pub/sub is a low-latency hint, so
a subscriber that misses a message heals by re-reading rows after its `last_seq`.

Tail one run's events from the command line:

```bash
redis-cli SUBSCRIBE events:run:<run-id>
```

Or the whole fleet:

```bash
redis-cli SUBSCRIBE events:firehose
```

Each message is one event envelope as JSON (`schema_version`, `run_id`, `seq`,
`type`, `ts`, `step_id`, `payload`) — the same shape a DB backfill returns.

**Health.** `engine_events_publish_failures_total` and
`engine_events_publish_dropped_total` (ADR-008 `events` subsystem, on both
deployables) rising means Redis pub/sub is degraded — a stalled or unreachable
Redis. This never loses events (they are durable in Postgres and WebSocket
clients heal via backfill); it only raises tail latency. To run with only the
durable log and no pub/sub, set `AGENTLOOM_EVENTS_PUBSUB_ENABLED=false`.
