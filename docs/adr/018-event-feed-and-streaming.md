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

### Typed TS client (as built, 16.5)

`web/lib/engine-client` is the typed client for the feed — the first package in
the new `web/` pnpm workspace (managed via Corepack; the Next.js app is added in
M17). It is a **pure library with no React/UI imports and no runtime dependency**
(the global `WebSocket` and `fetch`; Node >= 22), so M17/M18 build directly on
it, and it runs headless in Node.

- **Generated event types + drift check.** `src/generated/events.ts` is emitted
  from the committed `docs/schema/events.v1.json` by `scripts/gen-events-types.ts`
  — a small, **dependency-free deterministic emitter** over the exact vocabulary
  the invopop reflector produces (object/enum/primitive/array defs, `$ref`,
  `const`, `true`). Determinism (byte-stable output across environments) is what
  a drift check needs; a code-gen library's formatting could flake the diff. The
  emitter renders one interface/alias per `$defs` entry, then a discriminated
  layer: `EVENT_TYPES` (runtime const) + `EventType` (union) + `EventPayloadMap`
  + `EventEnvelope` (`{ [K in EventType]: base & { type: K; payload: Map[K] } }[EventType]`),
  so `switch (env.type)` narrows `env.payload`. The one non-obvious override:
  the reflected `UUID` def is a 16-byte array (`[16]byte`), but google/uuid
  marshals a **string** on the wire — the client consumes the wire, so `UUID` is
  emitted as `string`. **Drift is two-layer:** Go structs → `events.v1.json`
  (existing Go-CI diff) → `events.ts` (the new `web` CI job regenerates and
  `git diff --exit-code`s). A Go payload change that isn't reflected in the TS
  types fails CI on one side or the other.
- **Ticket auth, two modes.** `{ apiKey }` mints tickets itself with a `read`
  bearer (Node/server side; the key stays server-side); `{ mintTicket }` lets the
  caller supply them (a browser calling its own server-side proxy so the API key
  never reaches the browser — the M17 path). A fresh ticket is minted on **every
  (re)connect** (TTL ~60s). A `read` bearer on the upgrade is the documented
  non-browser alternative, reachable via a custom `webSocketFactory` (standard
  `WebSocket` can't set headers).
- **Deterministic recovery.** `RunStream` runs `snapshot → backfill from
  last_seq → live tail`; `close`d connections reconnect with `last_seq` = the
  highest seq seen (a fresh snapshot is re-emitted — consumers replace state),
  and the server re-backfills the exact tail, so the union across reconnects is
  gap-free and dup-free by seq. A **4001 slow-consumer close resumes
  immediately**; every other close backs off (exponential + full jitter, reset on
  `caught_up`). `FirehoseStream` manages subscriptions client-side and **re-issues
  every subscription with its tracked per-run cursors on reconnect** (bounded to
  the server's `MaxCursorRuns`, non-terminal runs preferred), deduping each run's
  events by seq across the re-backfill. In-band firehose `error` frames are
  surfaced and leave the connection open.
- **Wire frames are hand-mirrored** from the OpenAPI `WS*Frame` schemas (M16.5
  predates the generated OpenAPI REST client of M17.1; the one type that actually
  drifts — the event vocabulary — is the generated part). Every transport seam
  (the socket, the timers, `fetch`) is injectable, so the streams are unit-tested
  against an in-memory fake transport + fake clock (26 tests: backoff, cursor
  dedupe/gap classification, run resume/dedupe/4001/user-close/mint-failure-backoff/
  mintTicket-auth/terminal-close, firehose filtered delivery/cross-run dedupe/
  cursor re-subscribe/in-band-error/unsubscribe/cursor-bound, and a generated-types
  sanity check). The `make smoke-ws-tail` compose harness (DoD-2) tails a live run
  from the Node example (`examples/tail-run.ts`) through an api restart mid-run and
  asserts the received seqs are exactly `1..max(events.seq)` with a reconnect.
- **Accepted residuals.** Frame types are hand-mirrored until the M17.1 OpenAPI
  client replaces them; a bearer header on the WS upgrade needs a custom
  `webSocketFactory` (the standard `WebSocket` limitation); the resume cursor map
  is bounded to `MaxCursorRuns` (terminal-first drop — a dropped terminal run
  re-backfills from 0 on rediscovery and is deduped client-side, wasteful but
  correct). The 16.3/16.4 ticket residuals carry over.

### Dashboard scaffolding — the as-of cursor and the cross-origin knob (as built, 18.1)

The M18 live dashboard (ROADMAP 18.1) consumes this feed from the browser. Two
additive facilities landed here; neither changes the event contract, the WS
protocol, or any persisted state.

- **`RunView.event_seq` — the as-of / resume cursor.** The run view (both the
  REST run body and the WS snapshot's `run`) now carries `event_seq`, the run's
  `runs.next_seq`. Because `next_seq` is bumped in the same run-locked
  transaction as every event append (`AllocateEventSeq` returns the post-increment
  value), it equals `max(events.seq)` — no new column, no new read (the run row is
  already loaded). It is the exact point a client patches derived state from and
  resumes the WS stream at: the dashboard applies a live event to derived run
  state **only when its seq exceeds `event_seq`** (events at or below it are
  already reflected in the snapshot and are timeline-only), and subscribes the
  firehose per run from `{run_id: event_seq}`. Every transition sets an absolute
  status (never an increment), so re-applying an already-seen suffix event after
  a reconnect — or replaying the backfill over a fresher snapshot read whose
  later step/attempt rows advanced past it — is idempotent. The run **list** rows
  have no per-step map, so there terminal step events *increment* counters; the
  same `seq > event_seq` guard makes each event count at most once, so a
  redelivery never double-counts.

- **Cross-origin WS allowlist (`AGENTLOOM_API_WS_ORIGINS`).** A WebSocket upgrade
  cannot be forwarded through a Next.js route handler, so the browser dashboard
  dials the API's `/ws` endpoints **directly** at a public origin — a different
  origin than the app itself (app on `:3000`, API on `:8080`). coder/websocket's
  default `Accept` authorizes the `Origin` only against the request `Host`, so a
  cross-origin upgrade 403s. The forecast "later `OriginPatterns` knob" (16.3) is
  now `config.APIConfig.WSOrigins` → `WSOptions.OriginPatterns`, threaded into
  both `Accept` calls (run WS and firehose). Empty (the binary default) keeps the
  same-host-only 16.3/16.4 behaviour unchanged; compose defaults it to the local
  dev origins. The key never rides the upgrade — the browser mints a ws-ticket
  through the same-origin proxy (the `mintTicket` auth mode 16.5 built), and the
  ticket is the credential. `AGENTLOOM_API_PUBLIC_URL` (web app config, defaults
  to `AGENTLOOM_API_URL`) is the browser-reachable origin the app dials for the
  WS; it is public (an origin, no secret), carried to client code via a small
  runtime-config provider, never `NEXT_PUBLIC_`-baked.

- **Not this ticket:** the live cost meter/budget UX (18.4), the tabbed step
  inspector with log follow and semantic-retry diffs (18.3), and the React Flow
  status-skinned DAG with elkjs layout and expansion animation (18.2). 18.1 ships
  the run list with live chips, the run-detail scaffold (a status-badged steps
  pane, a basic inspector, the event timeline strip), and the snapshot →
  backfill → live-tail wiring through the 16.5 client.

### Live DAG view — graph endpoint additions and a decode fix (as built, 18.2)

The live DAG canvas (ROADMAP 18.2) renders a run's graph over WebSocket, so it
needs both the run's topology-with-provenance and reliable live delivery of
`graph_expanded`. Two additive facilities plus one bug fix landed here; none
changes the event contract or the WS protocol.

- **`graph_expanded` now re-decodes.** The live run WS Tailer and the firehose
  re-project a published/stored envelope through `event.Decode`. The
  `graph_expanded` payload's `delta` is a `dag.PlanOutput` whose `Step.Config` is
  the `dag.StepConfig` *interface*, which a plain `json.Unmarshal` cannot
  populate — so every live delivery of an expansion event failed with
  `cannot unmarshal object into … dag.StepConfig`, the Tailer dropped it, and a
  subscribed run WS stalled at the seq just before the expansion and resync-
  looped. This was latent since 16.2 (no e2e had ever live-tailed an expanding
  run). Fix: a custom `GraphExpanded.UnmarshalJSON` routes the delta through the
  canonical `dag.DecodePlanOutput` (which knows the per-type config shapes); a
  delta with no steps decodes plainly (no interface field to populate — the
  zero-value catalog sample). `TestDecodeGraphExpandedWithConfig` pins it; the
  integration proof is `dashboard-graph.spec.ts`'s planner-expansion test (a
  planner injects steps live and the browser sees them appear). No schema change.

- **`GET /v1/runs/{id}/graph` gained three fields (ticket 18.2).** `event_seq`
  (= `run.NextSeq`, the same as-of / resume cursor `RunView` carries) so the
  dashboard folds a live `graph_expanded` over the graph read only when its seq
  exceeds the read's; per-node `position` lifted from the run's definition
  snapshot `ui.nodes.<id>.position` (only for authored nodes; `ui` is
  engine-opaque, so a malformed or absent block simply yields no positions —
  this makes the graph endpoint self-sufficient for layout on inline runs too);
  and per-edge `decision` (the 15.3 approval routing marker) so the dashboard
  renders such an edge from the matching source port. All three are pure
  projections of already-persisted data (`buildRunGraphResponse` reads them off
  the run/edge rows) — no migration, no new read.

### Step inspector — worker identity on attempts & detail-body additions (as built, 18.3)

The tabbed step inspector (ROADMAP 18.3) reads a step's durable projection to
render its Overview/Output/Logs/Validation/Cost tabs. Three additive backend
facilities support it; none changes the event or WS contract.

- **`step_attempts.worker_id` (migration 0028) + `step_claimed.worker_id`.** The
  DoD requires a reclaimed step to name *both* workers — the one that lost the
  lease and the one that took over. A `claim_id` is a fencing token, not an
  identity, so the claiming worker's consumer name (`queue.NewConsumerName`,
  already on every log line as `worker_id`) is now stamped on the attempt row by
  the claim CAS (`ClaimStepArgs.WorkerID`, threaded through the one
  `CreateStepAttempt` insert both the fresh-claim and takeover-then-reclaim paths
  flow through) and, additively, on the `step_claimed` event payload
  (`worker_id`, `omitempty`) so the inspector's claim history is event-sourced
  live before the next detail refetch. `TestClaimStampsWorkerID` proves a
  takeover + re-claim leaves two attempts naming both workers; an empty worker id
  stores NULL (a programmatic claim). The `step_claimed.worker_id` schema
  addition is on an append-only feed, so it is a safe additive wire change (the
  16.5 two-layer drift regenerated `events.v1.json` and the TS client).

- **`StepView.config` + `StepView.idempotency_key` on the run-detail body.** The
  Validation tab's semantic-retry prompt diff (the killer demo) needs the model
  call's *authored* prompt to reconstruct each attempt's effective prompt (base
  prompt + the attempt's `feedback.text`, mirroring `LLMExecutor.WithFeedback`).
  `config` is the materialized `run_steps.config` emitted verbatim (a pure
  projection — the graph endpoint still carries no config, keeping 13.6's
  "no config in nodes"); the base prompt is *unrendered* (`${{ }}` refs visible)
  but identical across a step's semantic attempts, so the inter-attempt diff is
  exactly the feedback augmentation. `idempotency_key` is the derived
  `effects.Key(run_id, step_id)` (ticket 5.5) surfaced for correlation — a pure
  read, no storage. Both are additive `StepView` fields.

- **Exported Go goldens.** `internal/api/testdata/run_{detail,cost}_fixture.json`
  and `step_logs_fixture.json` are the exact wire shapes the pure builders
  produce (`TestRunDetailFixtureGolden` etc.), and the frontend inspector tests
  read them directly — tying the tabs' rendering to the backend contract, the
  13.6 `run_graph_fixture.json` precedent.

### Live cost meter & budget UX (as built, 18.4)

The run header's cost meter and budget controls (ROADMAP 18.4) are **entirely
under `web/app`** — no Go change, no migration, no config var, no metric. The
meter is stateless off the event feed: the running totals ride the `cost_updated`
event (`run_spent_nano_usd`/`run_saved_nano_usd`, non-decreasing in seq order by
construction — 10.5) and the budget rides `cost_updated`/`run_budget_updated`
(the latter's payload carries the new total). The 18.1 run-state reducer folds
those into `run.cost` under the same `asOf` seq guard the rest of derived state
uses, so the meter updates live with no cost refetch — the 10.5 note that "the
M18 meter reads the total straight off the stream, stateless" realized.

- **`run-state.ts` additions.** `cost_updated` now also carries the run's
  budget when the payload includes it; `run_budget_updated` sets the budget from
  its payload (correcting the 18.1 placeholder that assumed the budget was not on
  the payload — it is); and `mergeRunResponse` folds the body's run-level
  projection (`cost`/`status`/`park_reason`) when the body is at least as fresh
  as `asOf`, so a refetch after a raise/unpark reconciles those fields but a
  stale body never regresses live status.

- **Pure derivations** (`src/lib/pure/dashboard/`, the no-React boundary):
  `cost-meter.ts` projects the folded `CostSummaryView` onto a meter model with
  a `BudgetTier` (`unbudgeted`/`ok`/`warn`/`danger`/`exceeded` at 75%/90%/100%,
  `exceeded` also when the run is parked for budget); `foldCostEvents` proves in
  tests that the totals a run of `cost_updated` events converges to equal the
  cost summary — the DoD-3 consistency assertion, pre-paid at the reducer level.
  `budget-banners.ts` projects `model_downgraded`/`budget_exceeded`/
  `run_budget_updated` onto typed, seq-keyed banners (dedupe across a
  re-backfill; downgrade banners carry from/to models + a resolved trigger label
  for the DoD-2 assertion), and pins a live "parked at budget cap" affordance
  while `run.status==="parked" && park_reason==="budget_exceeded"` (gone on
  unpark) that carries the Raise action.

- **Raise = PATCH + optional unpark.** `useBudgetActions` drives the documented
  resume path (ADR-012): `PATCH /v1/runs/{id}/budget` then, if the user opted to
  resume, `POST /v1/runs/{id}/unpark`. The park→resume is reflected live through
  `run_budget_updated` + `run_unparked`, so the hook holds no optimistic state —
  it tracks pending/error and asks the controller to reconcile the detail body as
  belt-and-braces. A 409 on the unpark (the run already resumed via a concurrent
  action, or was not parked) is tolerated — the raise still succeeded. The
  existing `setRunBudget`'s 400 (non-positive) / 409 (terminal) / 403 map onto
  inline dialog errors.

- **Components.** `CostMeter` (header ticker + `role="progressbar"` budget bar
  with tier colouring + saved-by-cache indicator), `BudgetBanners` (dismissible
  stack), `RaiseBudgetDialog` (prefilled USD input, warns-not-blocks when the new
  budget is below current spend, a resume checkbox defaulting on when parked for
  budget). They replace the 18.1 ad-hoc `$` span in `RunDetail`.

**Decisions:** the meter is stateless off `cost_updated` (no client accumulation
— the stream totals are authoritative and monotonic); raise combines PATCH +
optional unpark in one dialog; warn-not-block on a sub-spend budget; no backend
change (every fact the meter needs was already on the feed and the REST budget
PATCH). **Accepted residual:** an unbudgeted run shows only the spend ticker (no
bar); the meter's budget arithmetic in the e2e is calibrated to the offline mock
pricing (`mock:*` = $1/$2 per Mtok) with response caching disabled per step (the
global cache would serve a repeat run for $0).

### Approval inbox & decision UI (as built, 18.5)

18.5 built the HITL approval inbox and the decision dialog on the existing
15.3/15.4 contract with **no new API surface** — the six `approval_*` events and
the two REST routes (`GET /v1/approvals`, `POST /v1/approvals/{id}:decide`) were
already complete, and the generated api-client types already carried them.

- **The approval events are the low-latency signal, the record is the truth.**
  An `approval_requested` event carries only id/step/title/allowed_decisions/
  allow_edit/timeout_at — NOT the proposed payload, description, or edit_schema —
  so the dashboard folds it into a *partial* `ApprovalRecord` and completes it
  from the authoritative run body (`GET /v1/runs/{id}`'s `approvals[]`). The
  run controller refetches the body on a live `approval_requested` (cheap, rare)
  so the decision dialog can render the proposed action; each record carries a
  per-record `lastSeq` cursor so a re-backfilled or reconnected suffix folds
  idempotently, absolutely by status. `approval_expired` with
  `action: run_parked` (the `on_timeout: park` escalation) stamps `expired_at`
  but leaves the record `pending` — the inbox renders that "parked at timeout,
  still decidable" state distinctly from a settled `expired` row.

- **Two additive, non-runtime backend touches.** (a) A proxy fix: the
  same-origin Next.js proxy joined path segments with `encodeURIComponent`,
  turning the `:decide` verb suffix into `%3Adecide`, which the chi router
  (matching on the raw path with `:` as its param delimiter) 404s before the
  handler; the proxy now preserves a literal `:` in a segment (encoding each
  colon-delimited part independently), pinned by a `proxy.test.ts` case. (b) An
  exported Go golden `internal/api/testdata/approval_list_fixture.json`
  (`TestApprovalListFixtureGolden`, the 13.6/18.3 precedent) — the exact
  `GET /v1/approvals` wire shape covering pending / approved-with-edit /
  rejected / expired / park-expired rows, which the frontend approval tests read
  as ground truth. No migration, no config var, no metric, no OpenAPI change.

- **The 409 is a first-class UI state, not an error toast.** A concurrent
  decision from another session (or the approval's own timeout) makes the second
  decide a 409 `approval_not_pending`; the dialog re-reads the approval (there is
  no `GET /v1/approvals/{id}`, so it re-lists the run's page and finds the row)
  and flips to a read-only "decided in another session" summary with a link into
  the run — DoD-2. Because the record also updates live from the
  `approval_decided` event, the dialog reaches that state whether the click loses
  the race or the event arrives first.

- **Edit validation is a client pre-check; the server 422 is authoritative.**
  The dialog validates an edited payload against the gate's `edit_schema` with a
  small CSP-safe walker that mirrors JSON-Schema *validator* semantics (enforces
  `required`/`enum`/`type`, allows extra properties unless
  `additionalProperties:false`) — deliberately NOT the strict Go decoder /
  graphdef `checkShape`, so it does not over-reject. The backend's
  `approval_decision_invalid` (422) `issues[]` (RFC-6901 pointers) are shown too
  and are the final word.

**Accepted residuals:** no nav-wide pending badge (the inbox page counts only);
a per-field structured edit form stays deferred (a raw JSON editor with
pointer-keyed issues); the firehose's `MaxTrackedRuns` bound applies to the live
inbox (a REST refetch heals). The flagship's own gate is decided in the Go
`TestFlagshipResearchCriticWriter`; the Playwright e2e drives the identical UI
path on the deterministic `approval_gate.json` shape offline.

### Ops views (as built, 18.6)

The operator surface — a cross-run dead-letter list, a queue-health snapshot,
and the caller-identity endpoint that powers rendered permissions — is three
additive `read`-scoped REST endpoints plus one migration (0029, two additive
indexes). No new event, metric, config var, outcome, or class.

- **`GET /v1/system/stats`** composes a **read-only** queue-introspection seam
  (`api.QueueIntrospector` — a narrow `QueueStats(ctx) (QueueStatsView, error)`
  interface satisfied by a thin cmd/api adapter over the `*queue.Queue` +
  `*queue.Delayed` cmd/api already builds for 15.4, so the api package never
  imports `internal/queue` or go-redis — the CacheOps discipline) with Postgres
  counts. Postgres is the only hard dependency, so the endpoint **always answers
  200**: when queue introspection is unwired (no Redis on this API) or a queue
  read fails, `queue` is `null` + a `queue_error` string and the Postgres blocks
  (DLQ backlog, active runs, outbox) still render. XLEN/XPENDING/XINFO/ZCARD are
  reads, not dispatches, so ADR-002 holds. `workers` is the roster from a new
  additive `queue.ListConsumers` (XINFO CONSUMERS → name/idle/pending, sorted);
  a not-yet-bootstrapped group (`ErrNoGroup`) is reported as an empty-but-present
  queue (zeros), never an error.
- **`GET /v1/dead-letters`** is a cross-run keyset page of dead-letter records,
  newest-first (uniform-descending keyset, the run-list convention), each joined
  to its **live** step/run status so `open` (still `dead_lettered` at its latest
  death) is honest. A real list endpoint — not a fan-out over
  `GET /v1/runs?status=failed` — because `step_dead_lettered` events carry no
  error document and the run body is per-run; the list is the honest minimum,
  the `GET /v1/approvals` precedent. The requeue action itself stays the 6.5
  `POST …/steps/{sid}/requeue`.
- **`GET /v1/auth/whoami`** returns the caller's own key id + scopes — what makes
  rendered permissions possible at all (the browser talks through a server-held
  key and otherwise cannot learn its scopes). `read`-scoped (a key without `read`
  cannot render the dashboard anyway); the server still enforces every scope, so
  this is a UX affordance, never the authorization.

Migration 0029: `dead_letters_created_idx` (the DLQ-list keyset) and a partial
`run_steps_dead_lettered_idx WHERE status='dead_lettered'` (the open-count /
open-join). Exported Go goldens `internal/api/testdata/{dead_letter_list,
system_stats}_fixture.json` are the frontend contract; OpenAPI gains a `System`
tag + the three ops (100/100).

**Accepted residuals:** the queue-health panel polls (no event carries queue
depth); the stats endpoint degrades to `queue: null` rather than 503 when Redis
is unwired.

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
