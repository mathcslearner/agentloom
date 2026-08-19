# ADR-018: Event feed — envelope, taxonomy, delivery & WebSocket streaming

- **Status:** Accepted
- **Date:** 2026-08-18
- **Ticket:** ROADMAP.md ticket 16.1 (opens Milestone 16)

<!--
This ADR opens M16. Ticket 16.1 fixes the whole event-feed contract — the
normalized envelope, the closed event taxonomy, the delivery semantics, and the
WebSocket protocol & auth — so the later M16 tickets conform to it without
re-litigating the design:

  - 16.2 live publish path (Redis pub/sub run:{id} + firehose, after-commit)
  - 16.3 run WebSocket endpoint (ticket auth, snapshot → backfill → tail)
  - 16.4 multi-run firehose endpoint (server-side filters, connection registry)
  - 16.5 typed TS event client (types generated from events.v1.json, CI drift)

Sections tagged "(arrives in 16.x)" state the contract now; those tickets add
"### … (as built, 16.x)" subsections under ## Decision as they land, the way
ADR-014 / ADR-015 / ADR-016 / ADR-017 grew across their milestones.
-->

## Context

The dashboard (M18) renders a run as it executes — status changes, attempts,
cost ticking up, the graph expanding, an approval appearing. Everything it shows
arrives through one layer, and that layer must survive dropped connections and
missed messages without gaps or duplicates. The durable truth already exists:
ADR-004's append-only `events` table, with a **per-run monotonic `seq`** the
appending transaction allocates from `runs.next_seq` after its guarded CAS
succeeds. M0–M15 accreted ~40 event types onto it, but with three problems that
block a clean streaming layer and a generated TS client:

1. **Inconsistent payloads.** Some events had exported typed structs
   (`CostUpdatedEvent`, `GraphExpandedEvent`), some unexported ones
   (`stepClaimedPayload`), and a few wrote a bare `struct{}{}`. The attempt field
   was `attempt_no` on the step-lifecycle events but `attempt` on the
   cost/approval/context ones. There was no single place a consumer — or a code
   generator — could learn the shape of a `graph_expanded` payload.

2. **No envelope.** A row is `(run_id, seq, type, payload, created_at)`. A
   consumer that wants "the step this event is about" had to know, per type,
   which field carries it (`step_id`? `origin_step`? `loop_source_instance`?).

3. **No fence.** Any store code could call `Events().Append` with a hand-written
   type string and an arbitrary payload. Nothing stopped a `(type, shape)`
   mismatch, and nothing enumerated the vocabulary for the schema/TS generator.

M16 needs the event feed to be a **versioned contract** — the same discipline
ADR-003 gives the workflow definition — decoupled from the persistence layer so
the WS protocol and the TS client depend on the shape of an event, not on
`internal/store` (pgx/sqlc). This ADR fixes that contract.

## Decision

### The envelope

Every event on a run's feed is projected into a normalized **envelope**:

```json
{
  "schema_version": 1,
  "run_id": "…",
  "seq": 42,
  "type": "step_succeeded",
  "ts": "2026-08-18T12:00:00Z",
  "step_id": "draft",
  "payload": { "step_id": "draft", "attempt": 1 }
}
```

- **`seq`** is the durable per-run monotonic sequence (ADR-004). It is
  **contiguous and gap-free from 1** and is the *only* ordering key. The WS
  protocol resumes from a client's `last_seq`.
- **`ts`** is `events.created_at` (the append transaction's wall clock). It is
  **display only** — never an ordering key, because two events in the same
  transaction share a timestamp and clocks are not the truth.
- **`step_id`** is the step the event concerns, **lifted from the payload** so a
  consumer can filter a run's feed by step without decoding each payload. It is
  empty for run-scoped events (`run_created`, `run_succeeded`, …). The
  projection reads it through a Go `StepScoped` interface (`EventStepID()`), so
  the canonical step is a code decision per type — the origin step for
  `graph_expanded`, the concrete completing instance (`critique#2`) for a loop
  event, the author step for `blackboard_updated` — not a fixed JSON key.
- **`schema_version`** is the **envelope** version, stamped at projection time
  from a constant. There is **no `schema_version` column** on the `events`
  table. Payload evolution within a version is **additive-only** (new optional
  fields a `1`-generation client ignores); a breaking payload change bumps the
  envelope version.

### The taxonomy — a leaf package with one payload struct per type

The vocabulary and payloads move into a new **leaf package `internal/event`**,
importing only other leaf contracts (`internal/dag` for a planner's
`PlanOutput`, `internal/cost` for the unknown-model rate) plus the standard
library. `internal/store` depends on it; it depends on nothing heavy. This is
the same layering as `internal/dag`, `internal/cost`, `internal/cache`,
`internal/tokens`, `internal/notify` — the WS/TS contract must not drag the
persistence layer in.

Each of the ~40 event types has **exactly one payload struct** implementing
`Payload` (`EventType() Type`), and the step-scoped ones also implement
`StepScoped` (`EventStepID() string`). Structurally-identical events
(`step_ready` / `step_skipped` / `step_requeued`) get **distinct named types** on
purpose — a payload names exactly one event type, which is what makes the writer
fence total. The store re-exports the payload structs the engine/api construct as
type aliases (`store.CostUpdatedEvent = event.CostUpdated`), so those packages
keep referencing `store.XxxEvent` unchanged.

A **`Catalog`** registers every type with its payload factory, whether it is
step-scoped, the ticket it arrived in, and a one-line doc. `TestCatalogComplete`
pins the catalog against the `Type` vocabulary both directions, so a new type
cannot ship without its payload and catalog entry. `Decode(type, raw)` and
`DecodeEnvelope(...)` are the typed read path the store's `EventEnvelope` adapter
and the M16.2+ WS layer use.

The **attempt field is normalized to `attempt`** across all step-lifecycle
payloads (they used `attempt_no`; the cost/approval/context payloads already used
`attempt`). This is a wire change for `step_claimed` / `step_reclaimed` /
`step_succeeded` / `step_retry_scheduled` / `step_throttled` /
`step_semantic_retry_scheduled`. It is acceptable because the feed is history,
not state: pre-16.1 rows keep their `attempt_no` key (an append-only log is never
rewritten), and no consumer of a live feed straddles the change. The
`step_attempts.attempt_no` **column** is unrelated and unchanged.

### Step logs are a separate channel

Per-step log lines (M7.4, `step_logs` + `GET /v1/runs/{id}/steps/{sid}/logs`)
stay **out of the main feed**. They are high-volume, size-capped rings with their
own pagination and level filter, and they are not run-lifecycle facts. The feed
carries what the dashboard renders as run/step/graph/cost/approval state; logs
are a drill-down the UI fetches on demand. This keeps the feed's volume bounded
and its schema small.

### The writer fence — one typed append path

The store has exactly **two sanctioned event writers**: the transitions'
package-level `appendEvent(ctx, gq, op, runID, event.Payload)` and the
instantiation plan's `appendEvent(..., event.Payload)`. Both **derive the type
string from `payload.EventType()`** — a writer can never emit a mismatched
`(type, shape)` pair. `eventRepo.Append` is the single low-level primitive both
call. `TestNoAdHocEventWrites` parses the store package with `go/ast` and fails if
any function other than those three calls `AppendEvent` / `Events().Append`
directly — the "no ad-hoc event writes" CI check.

### Delivery semantics (the M16 contract)

- **Durable truth is Postgres.** The `events` table, per-run gap-free `seq`, is
  the source. A consumer that has row `seq=N` for a run has every row `≤ N`.
- **Delivery is at-least-once; consumers dedupe and order by `(run_id, seq)`.**
  The live pub/sub path (16.2) is a **latency hint**, never a correctness
  dependency: a subscriber that detects a `seq` gap (missed or out-of-order
  message) **falls back to a DB backfill** from its `last_seq`. A duplicate is
  dropped by seq.
- **Backfill is a primary-key range scan** (`WHERE run_id=$1 AND seq>$2 ORDER BY
  seq LIMIT $3`, the existing `Events().List` shape).

### Live publish path (arrives in 16.2)

After a completion/transition transaction **commits**, workers/API publish the
new envelope(s) best-effort to a Redis pub/sub channel `run:{id}` and a firehose
channel. Publishing is **after-commit, async, and never affects the engine
transaction** — a publish failure is logged and metered, nothing more. A
consumer that misses a message recovers via the gap-detected DB backfill above.

### Live publish path (as built, 16.2)

The publish path hangs off **one seam at the store's transaction boundary**, not
the engine, so all ~52 event-append sites — the engine's completion/fan-out
writers, `engine.Control`'s API-side cancel/park/decide/budget, and API-side run
instantiation (`run_created`, `step_ready`) — publish through it with **zero
call-site change**.

- **`store.EventSink`** — `EventsCommitted(ctx, []event.Envelope)`. `WithTx`
  carries a per-transaction `*txState` buffer on its context; both sanctioned
  `appendEvent` helpers, right after the `AppendEvent` `RETURNING` row lands,
  record the **projected envelope** (built from the payload in hand +
  `row.created_at` via `event.NewEnvelope` — no re-decode, no possibility of a
  projection error). After the tx **commits**, `WithTx` hands the buffer to the
  sink (via `context.WithoutCancel`, so a caller context cancelled right after
  commit cannot drop the fan-out). A rolled-back tx delivers nothing; a
  committed tx that appended no event delivers nothing; with no sink wired the
  path costs nothing. `WithEventSink(sink)` is a `store.Option` on
  `Open`/`NewFromPool` (variadic — every existing caller is unaffected). The
  contract is **best-effort, non-blocking, never-panics**: the tx has already
  committed, so the sink can never affect correctness.

- **New leaf `internal/event/pubsub`** (imports `internal/event` + go-redis,
  never `internal/store` — the `cache/redisstore` / `retrieval/pgfts`
  precedent). It owns:
  - **Channels** — `RunChannel(prefix, runID)` = `<prefix>:run:<uuid>`,
    `FirehoseChannel(prefix)` = `<prefix>:firehose`.
  - **`Publisher`** — satisfies `store.EventSink` structurally.
    `EventsCommitted` copies the batch and does a **non-blocking** enqueue into a
    bounded channel (`select { default: PublishDropped }`); a single **drain
    goroutine** publishes each envelope's marshaled JSON to its run channel and
    the firehose under a per-batch timeout. One drain goroutine ⇒ per-process
    publish order = commit order; cross-process interleaving of one run's events
    is legal (it is a gap → backfill → dupe-drop by the contract above). A
    publish error / a marshal error is logged (rate-limited by go-redis' own
    pool logging) and metered; `Close(ctx)` drains the buffer bounded by ctx.
    Internal `publishFn` seam (`export_test.go`) drives the overflow/failure
    unit tests with no live Redis.
  - **`Subscription`** — `SubscribeRun` / `SubscribeFirehose` **block until the
    SUBSCRIBE is confirmed** (so 16.3's subscribe → snapshot → backfill ordering
    has no window), then pump `event.ParseEnvelope`'d envelopes onto a channel; a
    message that fails to parse (unknown type / bad JSON / unsupported envelope
    version) is logged and dropped — a gap the consumer heals via backfill.
  - **`Tailer`** — the pure, single-run gap/dedupe/backfill state machine (the
    `Backfiller` interface reads a run's events after a seq cursor; the store
    side adapts `Events().List` + `EventEnvelope`). `Offer(env)` drops a dupe
    (seq ≤ last), delivers the next (seq = last+1), and on a gap (seq > last+1)
    **backfills to head** (paged) before re-evaluating the live envelope —
    delivering it if now-next, else dropping it as already-backfilled. `Catchup`
    is the initial/resume backfill. This is the exact assembly 16.3's WS server
    and 16.5's TS client reuse.

- **`ParseEnvelope([]byte)`** on the leaf: decode the outer envelope, reject an
  unknown `schema_version`, decode the payload by type through the catalog. The
  wire form is the envelope JSON itself, so a published message parses back into
  the same typed envelope the store projected — publish and backfill agree by
  construction.

- **Config** — `AGENTLOOM_EVENTS_PUBSUB_ENABLED` (default true),
  `_CHANNEL_PREFIX` (default `events`), `_PUBLISH_BUFFER` (1024 batches),
  `_PUBLISH_TIMEOUT` (2s). Both deployables build the publisher over the **same
  Redis client the queue/cache use** (the shared coordination Redis, ADR-002 — a
  latency hint is neither a dispatch nor a correctness read) and pass it as the
  store's sink; the API's client stays fail-soft, so Postgres remains its only
  hard dependency.

- **Metrics** — new ADR-008 subsystem `events`, embedded in **both**
  `WorkerMetrics` and `APIMetrics` (both deployables publish):
  `engine_events_published_total{channel}` (`run`/`firehose`),
  `engine_events_publish_failures_total`, `engine_events_publish_dropped_total`,
  `engine_events_publish_latency_seconds`.

**Decisions.** The store transaction boundary is the seam (one hook covers every
writer; the engine never learns pub/sub exists). The buffer holds
already-projected envelopes built at append time (no re-decode, no projection
error, and what publishes is exactly what committed). Fire-and-forget with a
bounded drop-on-overflow buffer (a slow Redis degrades the hint, never the
engine — the events stay durable and backfill heals). One drain goroutine
(per-process order = commit order; cross-process reorder is a legal gap).
`WithTx` does **not** recover a sink panic — the enqueue is trivially
non-panicking, and swallowing panics in the store would hide real bugs.

**Accepted residuals.** A crash strictly between a commit and its publish loses
that one live message (the event is durable in Postgres; the next consumer
backfill heals it). A full buffer drops a batch (metered; healed by backfill).
Cross-process publish interleaving of one run's events can arrive out of order at
a firehose subscriber (a gap → backfill → dupe-drop, no event lost).

### Run WebSocket endpoint (as built, 16.3)

`GET /v1/runs/{id}/ws` streams a run's live event feed. It composes the 16.2
leaf (`pubsub.Subscriber` + `pubsub.Tailer`) with the store's `EventsAfter`
backfill — **no migration, no new metric**; one config block
(`AGENTLOOM_API_WS_TICKET_{SECRET,TTL}`), one dependency
(`github.com/coder/websocket`).

- **Auth is a short-lived signed ticket** minted at `POST /v1/runs/{id}/ws-ticket`
  (`read` scope, `read` rate class). The ticket is opaque:
  `base64url(payload).base64url(HMAC-SHA256(secret, payloadB64))`, payload =
  `{v:1, run_id, key_id, exp, nonce}`. The WS handshake verifies it with a
  constant-time MAC compare, an unexpired `exp`, and `run_id` == the route's run,
  and every failure is the uniform 401 (ADR-007). It exists so a browser never
  puts a long-lived bearer key in a WebSocket URL (URLs land in logs, proxies,
  history). It is **not single-use** — a reconnect inside the TTL reuses it — so
  revocation lag is bounded by the TTL (60s default, clamped `[5s, 1h]`). The
  documented alternative for non-browser clients (ctl, Node) is a normal
  `Authorization: Bearer` `read` key on the upgrade request; `requireReadOrTicket`
  accepts either and is why the `/ws` route is absent from the requireScope-based
  auth matrix. The signing secret is `AGENTLOOM_API_WS_TICKET_SECRET`; empty ⇒
  cmd/api generates a random per-process secret (a boot warning notes tickets are
  then not valid across replicas/restarts — a multi-replica deploy sets a shared
  secret).

- **Protocol** (JSON text frames, discriminated by `type`): one `snapshot` frame
  (the `GET /v1/runs/{id}` body) → `event` frames backfilled from the client's
  `?last_seq` → a `caught_up` frame → live `event` frames. The client dedupes and
  orders by `(run_id, seq)` and resumes after a disconnect by reconnecting with
  `last_seq` = the highest seq it saw. Every mechanism reduces to "read rows after
  `last_seq`", so recovery is deterministic (DoD-1: connect → kill mid-stream →
  reconnect → the union of both connections' events is exactly seqs 1..N).

- **Subscribe-before-backfill.** The driver subscribes to the run's pub/sub
  channel (blocks until SUBSCRIBE is confirmed) *before* the backfill, so no live
  event is missed in the window between reading to head and the tail beginning.
  A subscribe failure (Redis down, or no subscriber wired) degrades to **DB
  polling** at `PollInterval` — the feed still completes, just at poll latency.
  While live, a slow `ResyncInterval` `Catchup` heals the 16.2 residual (a final
  event whose publish was lost with no later event to trigger a gap).

- **Slow-client policy.** Frames go through a bounded per-connection buffer drained
  by one writer goroutine. A client that keeps the buffer full for
  `SlowClientTimeout` is closed with the application close code **4001** ("slow
  consumer") and resumes with its `last_seq`. The non-obvious mechanic:
  `coder/websocket` closes the connection when a `Write`'s context is cancelled,
  so the slow signal is the **buffer enqueue timeout, not a context cancel** — on
  a slow client the driver stops feeding and lets the writer drain naturally (a
  resuming client unblocks it), then sends the 4001 frame. `connCtx` is cancelled
  only for a genuine teardown (peer gone, server shutdown, write error), where a
  clean close is moot. A periodic ping detects a dead peer.

- **Seams.** `api.WithWebSocket(WSOptions{...})` carries the ticket secret/TTL,
  the `WSSubscriber` (nil ⇒ poll-only), and the connection tuning. `WSSubscriber`
  / `WSEventStream` are narrow api interfaces `*pubsub.Subscriber` /
  `*pubsub.Subscription` satisfy through a thin cmd/api adapter, so the api
  package never imports go-redis (the `CacheOps` discipline). Without the option
  the `/ws` and `/ws-ticket` routes are still mounted (route coverage stays
  static) but answer **503 `stream_unavailable`**. The `statusRecorder` grew
  `Unwrap()` so the hijack reaches the underlying `http.Hijacker`; the request log
  skips the latency histogram for the 101 upgrade so a long-lived connection does
  not skew the read-route p95.

- **Accepted residuals.** The ticket is bearer-equivalent within its TTL (no
  server-side revocation before expiry — the short TTL is the mitigation). A
  truly-stalled peer that never resumes reading cannot receive the 4001 frame
  (its TCP is wedged); the write timeout / ping then tears the connection down
  abnormally, which is correct for a dead peer. Origin is authorized against the
  request host by default (same-origin dashboards need nothing more);
  cross-origin support is a later `OriginPatterns` knob.

### Multi-run firehose (as built, 16.4)

`GET /v1/events/ws` streams a filtered, cross-run event feed for the dashboard's
run list — the run WebSocket (16.3) generalized to many runs on one connection.
It reuses the 16.2/16.3 machinery: **no migration, no new config var, no new
store table**; one additive event-payload field (`run_created.definition_id`)
and one new API subsystem metric group. The connection transport (bounded
outbound buffer + sole writer goroutine + the 4001 slow-client discipline) is the
shared `wsLink` the run WS was refactored onto, so both endpoints back-pressure
and close identically.

- **One process-wide firehose subscription, refcounted.** A per-`Handler`
  `firehoseHub` holds at most one Redis `SubscribeFirehose` subscription for the
  whole process, opened lazily on the first firehose connection and released when
  the last one leaves. It fans each committed envelope out to every connected
  client's bounded inbox (non-blocking — a full inbox drops the envelope, a seq
  gap the client heals via backfill, metered as `ws_hub_dropped_total`). There is
  no background goroutine or `Close` to manage on the `Handler`. When no live
  subscriber is wired (or the subscribe fails), connections self-discover new
  runs by polling the run list at `PollInterval` — the 16.3 poll-fallback parity.

- **Per-connection subscriptions + per-run Tailers.** A client manages
  subscriptions with control messages: `subscribe {id, filter, cursors}` opens or
  replaces one filtered subscription; `unsubscribe {id}` cancels it (bounded by
  `MaxSubscriptions`). The connection tracks each run some subscription wants
  through one `pubsub.Tailer` (the same seq-order/dedupe/gap-heal as 16.3), so
  cursors and gap-healing are per client. A run is **discovered and backfilled to
  head** the first time a live envelope for it matches a subscription, so the
  complete per-run feed is delivered even though `run_created` (written by the
  API) and step events (written by workers) publish from **different processes**
  and can arrive out of order — the run list must never miss a run's
  `run_created`. A supplied cursor resumes from a later seq instead. Terminal
  runs are marked so resync skips them; tracked runs are bounded by
  `MaxTrackedRuns` (terminal-first eviction).

- **Server-side filters.** `run_ids`, `types` (validated against the event
  catalog), `definition_id`, and `definition_name` are ANDed (values within
  `run_ids`/`types` ORed). `definition_name` matches inline (unstored) runs, which
  have no `definition_id`. Run→definition resolution rides a shared, bounded
  hub-level cache filled cheaply from the new `run_created.definition_id`/`name`
  payload (added additively under the one envelope version) — a per-run DB `Get`
  only for a definition-filtered run first seen mid-stream, cached once across all
  connections. An event is delivered once, tagged with the `subscriptions` ids it
  matched (a connection fanning into several UI views knows which to route it to);
  a run tracked for a subscription whose type filter excludes this event advances
  the cursor but sends no frame.

- **Protocol** (JSON text frames): `subscribe`/`unsubscribe` in, `subscribed`
  ack → cursor-backfilled `event` frames → a `caught_up {id, cursors}` frame →
  live `event` frames → `unsubscribed` acks. A malformed or over-limit control
  message yields an **in-band `error` frame** (`bad_message` / `filter_invalid` /
  `subscription_limit` / `unknown_subscription`) and leaves the connection open.
  Unlike the run WS (which `CloseRead`s), the firehose runs a control-reader
  goroutine (`conn.Read`) with a 64 KiB read limit.

- **Auth** is the same ticket, minted at `POST /v1/events/ws-ticket` with a new
  **audience** claim (`aud: "firehose"`, no `run_id`) — a run ticket is rejected
  at the firehose and vice-versa. A `read` bearer is the non-browser alternative.
  Both routes are absent from the requireScope auth-matrix walk (covered by
  `TestFirehose*`), and answer **503 `stream_unavailable`** when WS is not wired.

- **Backpressure metrics** (ADR-008 `api` subsystem, `kind` ∈ {run, firehose}):
  `engine_api_ws_connections{kind}` + `engine_api_ws_subscriptions` gauges,
  `engine_api_ws_frames_sent_total{kind}`, `engine_api_ws_slow_closes_total{kind}`,
  `engine_api_ws_hub_dropped_total`, and the `engine_api_ws_send_queue_fill_ratio
  {kind}` histogram (observed per enqueue — the direct slow-client signal). The
  16.3 run WS was retrofitted onto the same instruments. Per-connection stats
  (frames, high-water) are logged at close, not labelled.

- **Decisions.** One process-wide subscription fanned out (not one Redis sub per
  connection); per-connection Tailers (cursors are per client); backfill-from-0 on
  cursorless discovery (complete feed under cross-publisher reorder beats
  live-only); ticket audience split; the api never imports go-redis (the
  `WSSubscriber` seam gained `SubscribeFirehose`). **Accepted residuals:** a
  hub-inbox drop of a run's only event delays that run's discovery to its next
  event or a resync; the poll fallback is degraded (poll latency, run-level
  discovery); `subscriptions` tags are computed at delivery time (a mid-flight
  filter change is not retroactive); the 16.3 ticket residuals carry over.

- **Tests.** Unit: ticket-audience split, filter compile/match, cursor parse,
  run-meta extraction, frame encodings. Integration (`firehose_integration_test`):
  `TestFirehoseFilteredDelivery` (DoD-1 — run/type/definition filters + a
  two-subscription connection each get exactly their filter's events with correct
  tags, no gaps/dupes), cursor resume, auth matrix, control errors, slow-client
  4001, poll fallback, metrics. Load (`firehose_load_integration_test`,
  `make test-firehose-load`): `TestFirehoseHundredClients` (DoD-2 — 100 clients
  tailing a continuous run load; zero slow-closes, commit→receipt p95 within
  budget, REST p95 within a degradation budget of the no-client baseline).

### Typed TS client (arrives in 16.5)

`web/lib/engine-client`: TS types **generated from `docs/schema/events.v1.json`**
(the schema this ADR's leaf package generates via `internal/dag/gen -events-out`,
committed and CI-drift-checked alongside the definition and PlanOutput schemas),
implementing ticket auth, snapshot/backfill/tail, seq dedupe, resume, and
reconnection backoff.

## Consequences

- **The feed is a versioned, generator-backed contract.** `events.v1.json` is
  emitted from the Go structs and CI-diffed, so the TS client (16.5) cannot
  drift from what the engine writes — the same guarantee ADR-003 gives the
  definition format.
- **Recovery is deterministic.** Every delivery mechanism reduces to "read rows
  after `last_seq`", so dropped WS connections and lost pub/sub messages heal by
  construction. The hard part (gap-free per-run `seq`) was already true (ADR-004);
  this ADR only makes the shape consumable.
- **The writer fence is total and machine-checked.** A new event type is a
  compile-time-typed payload + a catalog entry, or it does not build/pass CI.
- **One wire break, absorbed.** `attempt_no → attempt` on five step-lifecycle
  types is a payload change. It is safe on an append-only feed (old rows keep
  their key; no live consumer straddles it) and buys a uniform `attempt` field
  the TS client and API already use.
- **Cost:** ~40 payload structs and a catalog to maintain, and the store carries
  a thin alias layer. The alias layer is the price of not churning every
  engine/api `store.XxxEvent` reference; it is mechanical and lint-excluded from
  the per-type doc rule (the docs live on the event package types).

## Alternatives considered

- **Keep payloads in `internal/store`, put only the envelope/catalog in a new
  package importing store.** Rejected: it inverts the dependency (the WS/TS
  contract would depend on pgx/sqlc), and it makes the typed-append fence
  impossible — `store` could not import a package that imports `store`. The leaf
  package is what lets the store's append helper take `event.Payload`.
- **A `step_id` column on `events`.** Rejected: the canonical step differs per
  type (`origin_step`, `loop_source_instance`, `author_step_id`), so a column
  would need per-writer population and could drift from the payload. Lifting it
  from the payload via a Go interface keeps one source of truth and needs no
  migration.
- **A global (cross-run) sequence.** Rejected for the same reason ADR-004
  rejected it: a global `BIGSERIAL` serializes all runs and buys nothing — the
  WS protocol is per-run, and the firehose merges per-run streams client-side by
  `(run_id, seq)`.
- **Full event sourcing (state = fold of the feed).** Out of scope and rejected
  by ADR-004: the state-machine tables are the truth; the feed is audit + UI. M16
  builds a *view*, not a second source of truth.
- **Version each payload independently (`payload.schema_version`).** Rejected as
  premature: one envelope version covering additive payload evolution is enough
  for v1, matches the `Verdict`/`PlanOutput`/`Feedback` precedent, and avoids 40
  independently-drifting version numbers. A single payload that needs a breaking
  change can bump the envelope version when that day comes.
- **Ship step logs on the same feed.** Rejected: log volume would dominate the
  feed and force its schema to carry a large, differently-shaped payload; logs
  already have a paginated REST channel the UI fetches on demand.
