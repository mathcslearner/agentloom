# API usage guide

Curl walkthroughs for the main flows against a local stack. The formal
contract is [`api/openapi.yaml`](../api/openapi.yaml) (OpenAPI 3.1; the
workflow-definition schema is `$ref`'d from the generated
[`docs/schema/workflow-definition.v1.json`](schema/workflow-definition.v1.json));
this page shows what using it looks like. `ctl` wraps the same routes —
`go run ./cmd/ctl --help`.

Everything below assumes the compose app stack:

```bash
make up-app        # Postgres + Redis + migrate + api (127.0.0.1:8080) + 2 workers
```

and [`jq`](https://jqlang.org) for readability (optional).

## Authentication

Every `/v1` route requires a bearer API key with the route's scope
(`submit`, `read`, `admin`; `admin` implies all — ADR-007). Keys are
minted by the API itself, so the first one is bootstrapped with the
**root credential**: set `AGENTLOOM_API_ROOT_KEY` in `.env` to any
`sk_`-shaped value before booting (e.g. generate one with
`python3 -c 'import secrets; print("sk_" + secrets.token_urlsafe(32))'`),
mint a stored admin key with it, then unset it.

```bash
export ROOT_KEY=sk_...   # the value from .env

curl -s http://127.0.0.1:8080/v1/keys \
  -H "Authorization: Bearer $ROOT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name": "local-dev", "scopes": ["admin"]}' | jq
```

```json
{
  "id": "6f7c1a52-…",
  "prefix": "sk_9GJt4Wm",
  "name": "local-dev",
  "scopes": ["admin"],
  "created_at": "2026-08-12T14:00:00Z",
  "key": "sk_9GJt4Wm…"
}
```

`key` is the plaintext credential — **shown in this one response and
recoverable nowhere else** (only its SHA-256 hash and the 11-character
lookup `prefix` are stored). Export it for the rest of this page:

```bash
export API_KEY=sk_...   # the "key" value from the response
```

Credential failures are a uniform `401` (`{"error": {"code":
"unauthorized", …}}`); a valid key lacking the route's scope is a `403`
naming the missing scope. Scoped keys for real clients:

```bash
curl -s http://127.0.0.1:8080/v1/keys \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name": "ci-submitter", "scopes": ["submit", "read"], "ttl": "720h"}' | jq .id
```

List and revoke (revocation is soft, idempotent, immediate):

```bash
curl -s http://127.0.0.1:8080/v1/keys -H "Authorization: Bearer $API_KEY" | jq
```

```bash
curl -s -X DELETE http://127.0.0.1:8080/v1/keys/<key-id> \
  -H "Authorization: Bearer $API_KEY" -w '%{http_code}\n'   # 204
```

## Submit a run and watch it

Submit an inline definition (one of the canonical fixtures works
verbatim). The `Idempotency-Key` header makes the submission safely
retryable: same key + same payload replays the original run (`200`,
`"reused": true`) instead of creating a duplicate; same key + a
*different* payload is refused with `409 idempotency_key_conflict`.

```bash
curl -s http://127.0.0.1:8080/v1/runs \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-fanout-001' \
  -d "{\"definition\": $(cat examples/definitions/fanout.json)}" | jq
```

```json
{
  "run_id": "018f3b1c-…",
  "status": "running",
  "entry_steps": ["start"]
}
```

Execution is asynchronous — workers pick the run up from the dispatch
outbox. Poll the run until it settles (or use `ctl watch <run-id>` for a
rendered status tree):

```bash
export RUN_ID=...   # run_id from the submit response

curl -s http://127.0.0.1:8080/v1/runs/$RUN_ID \
  -H "Authorization: Bearer $API_KEY" \
  | jq '{status: .run.status, steps: [.steps[] | {id, status, attempts: .attempt_count}]}'
```

The full response carries the run rollup, every step with its complete
attempt history (outcomes `succeeded`, `transient`, `timeout`,
`permanent`, `cancelled`, `lost` — ADR-006), every edge with its
resolution, and the run's `dead_letters` (see the DLQ flow below).

A definition that fails validation is a `400` with path-qualified
issues:

```json
{
  "error": {
    "code": "invalid_definition",
    "message": "definition failed validation",
    "issues": [
      {
        "code": "edge_endpoint_unknown",
        "severity": "error",
        "path": "$.edges[0].from",
        "msg": "edge references unknown step \"fethc\""
      }
    ]
  }
}
```

## List runs (keyset pagination + filters)

Newest first; filters compose. Feed `next_cursor` back verbatim — its
absence means the last page. Pagination is stable under concurrent
inserts (no skips, no duplicates across a walk).

```bash
curl -s -G http://127.0.0.1:8080/v1/runs \
  -H "Authorization: Bearer $API_KEY" \
  --data-urlencode 'status=succeeded' \
  --data-urlencode 'created_after=2026-08-01T00:00:00Z' \
  --data-urlencode 'limit=10' | jq '{n: (.runs | length), next_cursor}'
```

```bash
curl -s -G http://127.0.0.1:8080/v1/runs \
  -H "Authorization: Bearer $API_KEY" \
  --data-urlencode 'cursor=<next_cursor from the previous page>' | jq
```

## The definition registry

Store a definition once, submit it by reference forever. Names are
unique; versions are immutable and append-only.

```bash
curl -s http://127.0.0.1:8080/v1/definitions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"definition\": $(cat examples/definitions/linear.json)}" | jq '{id, name, version}'
```

Re-registering an existing name is a `409` pointing at the versions
route — a new version is deliberate, never accidental:

```bash
curl -s http://127.0.0.1:8080/v1/definitions/linear-pipeline/versions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"definition\": $(cat examples/definitions/linear.json)}" | jq '{id, version}'
```

Browse and submit by ref:

```bash
curl -s http://127.0.0.1:8080/v1/definitions -H "Authorization: Bearer $API_KEY" | jq
curl -s http://127.0.0.1:8080/v1/definitions/linear-pipeline/versions -H "Authorization: Bearer $API_KEY" | jq
curl -s http://127.0.0.1:8080/v1/definitions/<definition-id> -H "Authorization: Bearer $API_KEY" | jq .spec
```

```bash
curl -s http://127.0.0.1:8080/v1/runs \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"definition_id": "<definition-id>", "params": {"url": "https://example.com"}}' | jq
```

## Steering a run: cancel, park, unpark

All three answer `200` with the updated run; a request the run's
current state refuses (cancelling a finished run, unparking a running
one) is a `409` with code `conflict` naming the actual state.

```bash
curl -s -X POST http://127.0.0.1:8080/v1/runs/$RUN_ID/cancel \
  -H "Authorization: Bearer $API_KEY" | jq '{status: .run.status, finalized, cancelled_steps}'
```

Cancellation is cooperative: claimless steps are cancelled in the
request itself (`cancelled_steps`); in-flight steps converge as their
workers notice, and the run rests at `cancelling` until everything
settles (`finalized: true` means nothing was in flight and it went
straight to `cancelled`).

Park pauses dispatch without holding anything — in-flight steps finish
normally, no new claims start; unpark resumes and re-dispatches any
ready step stranded while parked:

```bash
curl -s -X POST http://127.0.0.1:8080/v1/runs/$RUN_ID/park \
  -H "Authorization: Bearer $API_KEY" | jq .run.status
```

```bash
curl -s -X POST http://127.0.0.1:8080/v1/runs/$RUN_ID/unpark \
  -H "Authorization: Bearer $API_KEY" | jq '{status: .run.status, dispatched}'
```

## Dead letters and requeue

When a step exhausts its retry budget (or fails permanently, or trips
the poison threshold), it lands in the run's dead-letter queue. Discover
requeueable steps from the run view:

```bash
curl -s http://127.0.0.1:8080/v1/runs/$RUN_ID \
  -H "Authorization: Bearer $API_KEY" | jq .dead_letters
```

```json
[
  {
    "step_id": "flaky-fetch",
    "seq": 1,
    "source": "retries_exhausted",
    "class": "transient",
    "attempts_at_death": 3,
    "created_at": "2026-08-12T14:05:00Z"
  }
]
```

Requeue resets the step to `ready` with its retry budget re-armed,
re-opens the run if it rested at `failed`, and revives any descendants
that were written off because of this step:

```bash
curl -s -X POST http://127.0.0.1:8080/v1/runs/$RUN_ID/steps/flaky-fetch/requeue \
  -H "Authorization: Bearer $API_KEY" | jq
```

```json
{
  "run_id": "018f3b1c-…",
  "step_id": "flaky-fetch",
  "status": "ready",
  "run_resumed": true,
  "revived": ["summarize", "store"],
  "dispatched": ["flaky-fetch"]
}
```

Requeueing a step that is not dead-lettered — or any step of a
cancelled run — is a `409`.

## Cost

Every cost-bearing attempt (an LLM call or a priced tool invocation) is
priced against the versioned pricing catalog (ADR-012) and ledgered in the
same transaction that records the attempt's success, so a run's cumulative
cost is always exactly the sum of its ledger rows. Money is integer
**nano-USD** on the wire (1 USD = 1e9); the `*_usd` strings are the derived
human-readable rendering.

`GET /v1/runs/{id}` carries the cumulative summary on the run view:

```json
{
  "run": {
    "id": "018f3b1c-…",
    "status": "succeeded",
    "cost": {"spent_nano_usd": 2100000, "saved_nano_usd": 0, "spent_usd": "0.0021", "saved_usd": "0",
             "on_budget_exceeded": "park"}
  }
}
```

When the definition sets a `budget_usd`, the summary also carries
`budget_nano_usd` / `budget_usd`; they are absent on an unbudgeted run.
`on_budget_exceeded` is always present (`park` | `fail`).

`GET /v1/runs/{id}/cost` returns the full breakdown — the summary plus
per-step and per-model roll-ups and the per-attempt ledger:

```bash
curl -s "http://127.0.0.1:8080/v1/runs/$RUN_ID/cost" \
  -H "Authorization: Bearer $API_KEY" | jq
```

```json
{
  "run_id": "018f3b1c-…",
  "summary": {"spent_nano_usd": 2100000, "saved_nano_usd": 0, "spent_usd": "0.0021", "saved_usd": "0"},
  "by_step": [
    {"step_id": "synthesize", "entries": 1, "spent_nano_usd": 1400000, "saved_nano_usd": 0, "spent_usd": "0.0014", "saved_usd": "0"},
    {"step_id": "brainstorm", "entries": 1, "spent_nano_usd": 700000, "saved_nano_usd": 0, "spent_usd": "0.0007", "saved_usd": "0"}
  ],
  "by_resource": [
    {"resource": "mock:sim-1", "entries": 2, "input_tokens": 340, "output_tokens": 880,
     "spent_nano_usd": 2100000, "saved_nano_usd": 0, "spent_usd": "0.0021", "saved_usd": "0"}
  ],
  "entries": [
    {"step_id": "brainstorm", "attempt": 1, "entry": "attempt", "resource": "mock:sim-1",
     "usage": {"input_tokens": 120, "output_tokens": 260},
     "rate": {"input_per_mtok": 1, "output_per_mtok": 2}, "rate_source": "wildcard",
     "cache_hit": false, "overhead": false,
     "spent_nano_usd": 700000, "saved_nano_usd": 0, "created_at": "2026-08-15T12:00:00Z"}
  ]
}
```

A **cache hit** ledgers a `$0` row with `cache_hit: true` and a
`saved_nano_usd` figure — what the call would have cost had it not been
served from cache. A **priced tool** ledgers a flat per-call charge; a tool
with no catalog entry is free and writes no row. A model the catalog does
not know is priced at the conservative fallback rate (`rate_source:
"fallback"`) and emits a `cost_unknown_model` event in the run's event feed
— the cue to add a catalog entry. A run with no cost-bearing steps answers
with a zero summary and empty breakdowns.

Each cost-bearing attempt also appends a `cost_updated` event carrying that
charge and the run's running spend/saved totals (non-decreasing per run) —
the source the live cost meter will render (the event-feed read API and the
meter UI land in M16/M18). Spend is also exported as Prometheus counters by
resource (`engine_cost_spent_usd_total`, `engine_cost_saved_usd_total`, …)
on the worker's `/metrics`, surfaced in the Grafana Engine dashboard's Cost
row.

## Budgets

A definition may cap spend: a run-level `budget_usd` with an
`on_budget_exceeded` policy (`park`, the default, or `fail`), and per-step
`budget: {max_usd, max_tokens}` caps. Enforcement is at **claim time**,
before a cost-bearing step runs: the engine projects the step's cost (run
spend so far + a pre-flight estimate) and, if it would exceed the budget,
either parks the run (resumable) or dead-letters the step permanently. An
oversized request (`max_tokens` cap exceeded) is failed before the provider
is ever called.

Under the `park` policy a budget crossing parks the run with reason
`budget_exceeded` and emits a `budget_exceeded` event carrying the
projection detail. Raise the budget and unpark to resume:

```bash
curl -s -X PATCH "http://127.0.0.1:8080/v1/runs/$RUN_ID/budget" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"budget_usd": 5.00}' | jq '.run.cost | {budget_usd, spent_usd}'
```

```bash
curl -s -X POST "http://127.0.0.1:8080/v1/runs/$RUN_ID/unpark" \
  -H "Authorization: Bearer $API_KEY" | jq '{status: .run.status, dispatched}'
```

`PATCH …/budget` answers `200` with the updated run; `budget_usd` must be
positive (`400` otherwise), and the budget is immutable on a terminal run
(`409`). Raising the budget does not itself resume work — unpark does. The
`ctl budget <run-id> <usd>` command wraps the same endpoint.

## Per-step logs

Everything a step's executor logs through its step logger is captured
durably per attempt (retries, reclaims, and takeovers each get their own
attempt, so each has its own log stream). Read one attempt's lines —
`attempt` defaults to the step's latest:

```bash
curl -s "http://127.0.0.1:8080/v1/runs/$RUN_ID/steps/flaky-fetch/logs?attempt=1" \
  -H "Authorization: Bearer $API_KEY" | jq
```

```json
{
  "run_id": "018f3b1c-…",
  "step_id": "flaky-fetch",
  "attempt": 1,
  "lines": [
    {
      "seq": 1,
      "level": "info",
      "message": "fetching",
      "fields": {"url": "https://example.com"},
      "trace_id": "80f198ee56343ba864fe8b2a57d3eff7",
      "logged_at": "2026-08-12T14:04:58Z"
    }
  ],
  "truncated": false
}
```

Lines page in ascending `seq` order — feed `next_cursor` back as
`?cursor=` (limit up to 1000, default 200). `level=` filters to a
minimum severity (`level=warn` returns warn and error lines);
`trace_id` joins a line to its attempt's trace in Jaeger. Follow mode is
polling the cursor — there is no streaming channel in v1.

Storage per attempt is a size-capped ring (newest lines win) behind a
bounded capture buffer, so a flooding executor can never stall
execution. When lines were lost, the response says so:

```json
{"attempt": 1, "lines": [ … ], "truncated": true, "dropped_lines": 9900}
```

`seq` gaps mark the same thing line-by-line: every captured line
consumed a sequence number, stored or not.

## The plugin catalog

`GET /v1/plugins` (read scope) lists every plugin compiled into the
deployment (ADR-009): kind, name, semver version, capability flags, and
each plugin's generated config JSON Schema — the machine-usable form the
UI's config panels consume. In 8.1 the catalog is the executor set; tool,
retriever, model-provider, and validator plugins join it as their
tickets land.

```bash
curl -s http://127.0.0.1:8080/v1/plugins \
  -H "Authorization: Bearer $API_KEY" | jq '.plugins[] | {kind, name, version, capabilities}'
```

```json
{
  "kind": "executor",
  "name": "llm",
  "version": "0.1.0-stub",
  "capabilities": {
    "side_effectful": false,
    "cacheable": true,
    "cost_bearing": true
  }
}
```

A `-stub` pre-release version marks a dev-stub implementation (the real
executor replaces it in place with a bump to `1.0.0`). Each entry's
`config_schema` is a JSON Schema 2020-12 document generated from the
same Go structs the engine decodes with:

```bash
curl -s http://127.0.0.1:8080/v1/plugins \
  -H "Authorization: Bearer $API_KEY" | jq '.plugins[] | select(.name == "llm").config_schema'
```

The same listing renders as a table with `ctl plugins list`. Note the
catalog describes what the API binary compiles in — the compose stack
sets `AGENTLOOM_API_TEST_EXECUTORS` to mirror the worker's
`AGENTLOOM_WORKER_TEST_EXECUTORS`, so the listing matches what the fleet
executes.

## Response cache ops

Two admin-scoped endpoints operate the response cache (ADR-011). They are
available when the cache is enabled (`AGENTLOOM_CACHE_ENABLED`, on by
default); otherwise they answer `503 cache_unavailable`. The runbook
([`docs/ops-runbook.md`](ops-runbook.md)) covers when to bust versus bump a
plugin version versus let a TTL expire.

`GET /v1/cache/stats` reports per-plugin cumulative hit/miss/store counters
and the derived hit rate. The numbers reconcile against the worker fleet's
`engine_cache_*` Prometheus counters:

```bash
curl -s http://127.0.0.1:8080/v1/cache/stats \
  -H "Authorization: Bearer $ADMIN_KEY" | jq '.plugins'
# [{"kind":"model_provider","name":"mock","hits":412,"misses":88,"stores":88,"hit_rate":0.824}]
```

`POST /v1/cache/bust` removes entries by namespace: an empty body busts
everything, `plugin_kind` alone busts one kind, `plugin_kind` + `plugin_name`
busts one concrete plugin. Deletion is `SCAN`-batched and non-blocking, and
the action is audit-logged with the actor's key id:

```bash
curl -s -X POST http://127.0.0.1:8080/v1/cache/bust \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"plugin_kind":"retriever","plugin_name":"pg_fulltext"}'
# {"deleted": 128}
```

Both render through `ctl cache stats` and `ctl cache bust [--kind K] [--name N]`.

## Errors and rate limits

Every non-2xx response carries one envelope:

```json
{"error": {"code": "<machine-stable code>", "message": "<human explanation>", "issues": [...]}}
```

The code vocabulary is enumerated in the spec (`ErrorCode`); renaming a
code is a breaking change. `message` wording is not contract.

When rate limiting is enabled, every `/v1` response reports the
caller's per-key class bucket (`submit`, `read`, or `admin` — mirroring
the route's scope):

```text
X-RateLimit-Limit: 50          ← bucket capacity
X-RateLimit-Remaining: 42      ← whole tokens left
X-RateLimit-Reset: 4           ← whole seconds until full
```

A denial — from the per-key bucket or the shared global safety bucket —
is `429` with code `rate_limited` and `Retry-After` (whole seconds,
rounded up) from whichever bucket denied. Back off for `Retry-After`
seconds and retry; submissions carrying an `Idempotency-Key` are safe
to retry blindly.
