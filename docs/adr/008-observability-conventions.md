# ADR-008: Observability conventions

- **Status:** Accepted
- **Date:** 2026-08-12
- **Ticket:** ROADMAP.md ticket 7.1

## Context

M7 makes observability first-class: engine metrics (7.2), distributed
tracing across the queue (7.3), per-step log capture (7.4), and
dashboards + alerts (7.5). Everything after M7 is built with these
instruments on — queue-depth metrics become KEDA autoscaling signals in
M20, and the histograms are the measurement substrate for M19's load
tests. Four decisions must be fixed before the first metric is
registered, because they are near-impossible to change once dashboards,
alerts, and autoscalers depend on names and shapes:

- **Metric naming.** Renaming a metric later silently breaks every
  dashboard panel, alert rule, and KEDA trigger that references it.
- **Label cardinality.** Prometheus stores one time series per unique
  label-value combination. A single label carrying `run_id` would create
  unbounded series — a memory leak in the monitoring system that shows
  up only under production load, long after the code shipped. The
  project invariant ("no metric labels with unbounded cardinality") needs
  an enforceable, enumerated form.
- **Log fields.** `internal/obs/log` (ticket 0.5) already pins canonical
  field names; M7 adds trace correlation, and 7.4 stores step logs
  durably — the dictionary must be complete before rows are written.
- **Trace propagation.** A run's execution hops processes at every step:
  API → outbox → Redis Streams → worker A → outbox → worker B. The
  envelope fields (`traceparent`/`tracestate`) were reserved in ADR-005;
  how context flows through *durable* state (outbox rows survive their
  enqueuer; the reconciler re-outboxes steps whose original dispatcher is
  long dead) is the part that needs design, not just wire format.

Constraints from the existing system: the API's public port is
bearer-authed (ADR-007) and covered by route-coverage and OpenAPI drift
tests — an unauthenticated `/metrics` cannot live there. The worker has
no HTTP listener at all. Every test layer (unit, storetest/queuetest
harnesses, the crash suite's real subprocess workers) must keep running
with zero new ports bound and zero collector dial attempts. Time is
injectable everywhere; telemetry must not smuggle wall-clock dependencies
into logic under test.

## Decision

### Metric naming scheme

We will name every metric `engine_<subsystem>_<name>[_<unit>]` on
**instance-scoped `prometheus.Registry` instances** — never the package
global `prometheus.DefaultRegisterer` — created by
`internal/obs/metrics.NewRegistry` and threaded explicitly to the
components that record on them (the same injection discipline as clocks
and loggers). Tests get isolated registries for free; nothing can
register twice or leak between tests.

Rules, following Prometheus upstream conventions:

- One namespace: `engine_`. Both deployables share it — where the
  emitting service matters, it is the scrape target (Prometheus `job`/
  `instance` labels), not the metric name. API-surface metrics use the
  `api` subsystem (`engine_api_requests_total`), not a second namespace.
- Subsystem vocabulary (extended only by ADR amendment): `build`,
  `queue`, `outbox`, `dispatch`, `reconcile`, `step`, `steplog` (7.4),
  `run`, `api`, `worker`, `ratelimit` (9.2).
- Base units and suffixes: durations in seconds (`_seconds`), sizes in
  bytes (`_bytes`), token counts in tokens (`_tokens`, 9.3's
  estimate-error histogram), counters end `_total`, gauges carry no suffix
  (`engine_queue_ready_depth`). Histograms are the default for
  latency/duration (M19 needs percentiles) and for the signed token-cost
  estimate error; summaries are banned (not aggregatable across the fleet).
- `engine_build_info{service, version} 1` is the conventional info gauge,
  registered by `NewRegistry` itself alongside the standard Go runtime
  and process collectors — every scrape proves the pipeline end-to-end
  even before 7.2's instrumentation lands.

### Label cardinality budget

**`run_id`, `step_id`, `attempt`, `claim_id`, `worker_id`, `key_id`, raw
URL paths, and error message strings are never metric labels.** They are
log and trace fields. This is the project invariant made enforceable:
the allowlist below enumerates every permitted label key with its
bounded vocabulary, and 7.2's cardinality audit (and every later
ticket's review) checks new metrics against this table. A new label key
requires amending this table first, and must be a closed vocabulary.

| Label key | Values | Bound |
|---|---|---|
| `service` | `agentloom-api`, `agentloom-worker` | 2 |
| `version` | build version (one per deployed build) | ~1 per rollout |
| `step_type` | the dag catalog: `noop`, `echo`, `sleep`, `fail_n_times`, `join`, `branch`, `counter`, `effectful_echo`, `llm`, `tool`, `retrieve`, … | ~12, grows by catalog ticket |
| `outcome` | attempt outcomes: `succeeded`, `lost`, `transient`, `permanent`, `timeout`, `cancelled` | 6 |
| `status` | run/step status vocabularies (ADR-004) | ≤ 8 each |
| `reason` | outbox reasons: `step_ready`, `retry`, `reconcile_ready`, `reconcile_running`, `reconcile_retry`, `dlq_requeue`, `unpark` | 7 |
| `class` | error classes (ADR-006) or rate-limit classes (`submit`, `read`, `admin`, `global`) | ≤ 5 |
| `source` | dead-letter sources: `retries_exhausted`, `permanent`, `poison` | 3 |
| `route` | chi route *pattern* (`/v1/runs/{id}`), never the raw path; unrouted requests (404/405) collapse to the single value `unmatched` | ~17, grows by endpoint ticket |
| `method` | HTTP methods actually routed; unrouted requests clamp unrecognized verbs to `other` (client-supplied methods must never mint values) | ≤ 8 |
| `code` | HTTP status code | ~10 in practice |
| `duty` | consumer duties: `consume`, `heartbeat`, `reclaim`, `janitor`, `trim`, `promote` | 6 |
| `result` | claim decisions (7.2): `won`, `ack_drop`, `redeliver`, `takeover` | 4 |
| `bucket` | API rate-limit bucket kind (7.2): `per_key`, `global`; fleet rate-limit denying dimension (9.2): `requests`, `tokens`, `both` | ≤ 5 |
| `decision` | rate-limit decision (7.2): `allowed`, `denied` | 2 |
| `resource` | fleet-limit resource name (9.2): the resolved config-entry name (`anthropic:*`, `mock:sim-1`, `tool:http_request`) — operator-authored, bounded | ~config size |

Worst-case series count per metric is the product of its label bounds;
any metric whose product exceeds ~1,000 needs an explicit justification
in its registering ticket.

### Metric inventory (as built by ticket 7.2)

Every instrument is declared in `internal/obs/metrics/instruments.go`;
`TestInstrumentConformance` in that package is this table's executable
form — it gathers the full instrument set and fails on any name outside
the subsystem vocabulary, any counter without `_total`, any histogram
without a unit suffix (`_seconds` or `_tokens`), or any label key missing
from the allowlist above.

| Metric | Type | Labels | Recorded by |
|---|---|---|---|
| `engine_queue_ready_depth` | gauge | — | worker sampler: XINFO GROUPS lag; when Redis reports lag as unknowable (possible after trims), falls back to XLEN − PEL, which overstates only by acked-untrimmed entries |
| `engine_queue_stream_length` | gauge | — | worker sampler (XLEN) |
| `engine_queue_pel_size` | gauge | — | worker sampler (XPENDING) |
| `engine_queue_delayed_depth` | gauge | — | worker sampler (ZCARD) |
| `engine_queue_reclaimed_total` | counter | — | reclaim duty, per XAUTOCLAIMed entry |
| `engine_queue_poison_total` | counter | — | poison diversion, once consumed |
| `engine_queue_promoted_total` | counter | — | promoter duty |
| `engine_queue_promote_lag_seconds` | histogram | — | promoter duty (`PromoteResult.MaxLag` per pass) |
| `engine_outbox_backlog` | gauge | — | worker sampler (row count) |
| `engine_outbox_oldest_age_seconds` | gauge | — | worker sampler (oldest row age; 0 when empty) |
| `engine_dispatch_dispatched_total` | counter | `reason` | dispatcher, per XADDed row, post-commit |
| `engine_dispatch_lag_seconds` | histogram | — | dispatcher: dispatch time − row `created_at` (DB clock vs app clock — gauge-grade skew accepted) |
| `engine_reconcile_healed_total` | counter | `reason` | reconciler sweeps (`reconcile_ready` / `reconcile_running` / `reconcile_retry`); no series until a heal happens |
| `engine_step_claims_total` | counter | `result` | claim path, one decision per delivery |
| `engine_step_scheduling_latency_seconds` | histogram | — | claim path: claim time − the step's ready `updated_at`, both injected clocks, read under the run lock; **ready→running only** — retrying claims are skipped (backoff is not scheduling) |
| `engine_step_duration_seconds` | histogram | `step_type`, `outcome` | executor invocation; outcome vocabulary = classed failures + `succeeded`/`cancelled` (`lost` never observable in-process) |
| `engine_step_retries_total` | counter | `class` | retry routing, post-commit |
| `engine_step_takeovers_total` | counter | — | worker takeover path + reconciler heals |
| `engine_step_fencing_rejections_total` | counter | — | abandoned fenced completions + stale takeovers |
| `engine_step_dead_letters_total` | counter | `source` | judged dead-letter completions + poison handler |
| `engine_run_duration_seconds` | histogram | `status` | run-terminalizing transactions on workers: terminal time − run `started_at` (both injected clocks; `created_at` is a DB default and would mix clocks). API-side cancel finalizations are not recorded — a known, documented gap |
| `engine_worker_active` | gauge | — | worker sampler: consumers with idle ≤ 3× read block (each worker reports the fleet-wide count; dashboards take `max`) |
| `engine_api_requests_total` | counter | `route`, `method`, `code` | API request middleware, post-routing |
| `engine_api_request_duration_seconds` | histogram | `route`, `method` | same; `code` deliberately excluded to keep route×method×code×buckets under the series budget |
| `engine_api_requests_in_flight` | gauge | — | request middleware (7.5): started/finished bracket around every request; unlabeled — route is only known after routing |
| `engine_api_ratelimit_decisions_total` | counter | `class`, `bucket`, `decision` | the 6.4 `RateLimitMetrics` seam's Prometheus implementation |
| `engine_api_ratelimit_failopen_total` | counter | `class` | same (errored acquire allowed through) |
| `engine_steplog_captured_total` | counter | — | step-log capture (7.4): lines accepted into the sink's queue |
| `engine_steplog_dropped_total` | counter | — | step-log capture (7.4): lines lost before storage — queue overflow (drop-oldest) or a failed flush; ring-cap evictions are not drops (stored, then rotated out) |
| `engine_steplog_flush_failures_total` | counter | — | step-log capture (7.4): flush transactions that failed and dropped their batch |
| `engine_ratelimit_throttled_total` | counter | `resource`, `bucket` | fleet limiter (9.2): steps deferred by backpressure, by resource and denying dimension |
| `engine_ratelimit_throttle_wait_seconds` | histogram | `resource` | fleet limiter (9.2): the clamped+jittered re-dispatch delay a throttle adds |
| `engine_ratelimit_fail_opens_total` | counter | — | fleet limiter (9.2): acquire errored (e.g. Redis down) and the step proceeded unlimited |
| `engine_ratelimit_estimate_error_tokens` | histogram | `resource` | reconciliation (9.3): signed token-cost error `actual − estimate` corrected on the token bucket; negative = over-estimate (refund), positive = under-estimate (extra debit) |
| `engine_ratelimit_reconcile_failures_total` | counter | — | reconciliation (9.3): correction could not be applied (e.g. Redis down); estimate stays debited, step proceeds |

Gauges are sampled by a cmd/worker loop every
`AGENTLOOM_WORKER_METRICS_SAMPLE_INTERVAL` (default 10s, under the 15s
scrape convention), running only when the admin listener is configured;
counters and histograms record at their event sites through narrow
per-package seams (`queue.ConsumerMetrics`, `engine.Metrics`,
`api.RequestMetrics`) whose defaults are no-ops — every test layer keeps
running with recording off. `make smoke-metrics` is the acceptance
script: it drives a mixed workload on compose and asserts every metric
above is visible in Prometheus (crash-only counters at presence, the
rest at movement).

### Log field dictionary

`internal/obs/log` remains the single home of canonical field names; ad
hoc key strings stay banned. The dictionary, extended for M7:

| Field | Type | Meaning |
|---|---|---|
| `run_id` | string (UUID) | the run (0.5) |
| `step_id` | string | the step within its run (0.5) |
| `attempt` | int | 1-based attempt number (0.5) |
| `worker_id` | string | queue consumer name (0.5) |
| `trace_id` | string | active trace, hex (0.5; stamped from span context in 7.3) |
| `span_id` | string | active span, hex (new; stamped alongside `trace_id` in 7.3) |
| `key_id` | string | API key row id or `"root"` (6.1) |
| `service` | string | `agentloom-api` / `agentloom-worker` (new) |
| `error` | any | error value (existing `slog.Any("error", err)` convention, now canonical) |

`trace_id`/`span_id` are how logs join to Jaeger; `run_id`/`step_id` are
how they join to Postgres state. A log line on a hot path should carry
both worlds.

### Trace propagation design

*(Designed here; implemented by ticket 7.3.)*

- **Format:** W3C Trace Context (`traceparent`/`tracestate`), propagated
  with the composite `tracecontext` + `baggage` propagator registered
  globally at boot.
- **Root span at submission:** `POST /v1/runs` creates the run inside an
  API server span; that span's context is the run's root.
- **Durable context, not just wire context:** the run's trace context is
  **persisted on the run row** at instantiation (column added by 7.3's
  migration). Envelopes carry `traceparent`/`tracestate` for the common
  path, but re-enqueues that do not descend from any live span — the
  reconciler's `reconcile_*` heals, retry promotion after a crash,
  `dlq_requeue`, `unpark` — restore linkage from the run row. Without the
  durable copy, every healed dispatch would start an orphan trace.
- **Span topology:** each delivery handled by a worker starts an attempt
  span whose parent is the enqueuing span (from the envelope). Retries
  and reclaim/takeover re-executions are **span links**, not
  parent-child — the retry is caused by the failed attempt but is not
  inside it. Fan-in joins carry links from all firing parents. Within an
  attempt span, child spans cover the claim CAS, the executor
  invocation, the completion transaction, and the ACK.
- **Envelope compatibility:** populating the reserved fields is additive
  within envelope version 1 (ADR-005: decoders ignore unknown fields,
  absent trace fields mean "no context"), so mixed fleets during rollout
  are safe by construction.

**As built (ticket 7.3).** The design above, realized with three durable
homes for trace context (migration 0010, all TEXT nullable, NULL = "no
context" — every test layer keeps running span-free on the no-op
provider):

- `runs.trace_parent`/`trace_state` — the root context, captured from
  the `POST /v1/runs` otelhttp server span (which the `requestLog`
  middleware renames to `<METHOD> <chi route pattern>` after routing,
  closing 7.1's naming placeholder).
- `task_outbox.trace_parent`/`trace_state` — the enqueuing span's
  context, stamped only by writers inside a live span (the completion
  transaction's fan-out rows carry the `step.completion` span;
  instantiation's entry rows carry the submission span). All other
  writers (reconciler, unpark, dlq_requeue) leave NULL, and the drain
  read coalesces to the run row's root — one rule, so healed dispatch
  paths needed no changes at all.
- `run_steps.trace_span` — the current attempt's span context, stamped
  by the claim CAS. The pre-claim read that already serves the 7.2
  scheduling-latency metric surfaces the value being overwritten, and
  that previous value is the **link source for every re-execution**:
  a due retry links to the failed attempt, a worker or reconciler
  takeover links to the lost attempt — uniformly, with no link fields in
  the envelope and no dependence on the dying worker handing anything
  over. The delayed retry envelope carries the run root as parent
  (constant per run, preserving ADR-005's byte-identical delayed-member
  dedup).

Span inventory: the queue consumer starts `step.attempt`
(SpanKind=consumer; attrs `run_id`, `step_id`, `reason`,
`delivery_count`, `worker_id`, plus `attempt`/`step_type` once claimed)
around the whole delivery — it owns the span because the ACK must be a
child — with `step.claim`, `step.executor`, `step.completion` (engine)
and `queue.ack` (consumer) as children. Fan-in joins add links to every
firing parent's attempt span, read from the parents' `trace_span`
columns (`step_type = join` gates the extra query). Tracers come from
injectable providers (`queue.ConsumerConfig.TracerProvider`,
`engine.WithTracerProvider`) defaulting to the global (no-op unless
`obs/trace.Setup` enabled export); the propagation helpers in
`internal/obs/trace` use an explicit W3C propagator so they round-trip
without global setup. `trace_id`/`span_id` are stamped into the log
context per delivery and per API request. Acceptance: `make smoke-trace`
asserts one Jaeger trace per run spanning both compose worker replicas
with a FOLLOWS_FROM retry link; the hermetic span-topology tests run
against an in-memory recorder in `internal/engine`
(`trace_integration_test.go`).

### Per-step log capture (as built by ticket 7.4)

Executor log lines — everything emitted through
`exec.StepContext.Logger`, and only that (the engine's own pipeline
lines are diagnostics, not step logs) — tee into the durable `step_logs`
table (migration 0011), serving
`GET /v1/runs/{id}/steps/{sid}/logs?attempt=&level=&cursor=&limit=` and
M18's per-step log view. The capture side is `internal/exec/steplog`: a
per-worker `Sink` holding a bounded in-memory queue, drained to Postgres
by an async flusher (`AGENTLOOM_WORKER_STEPLOG_*` knobs; capture on by
default at level `info`, ring cap 1000 lines per attempt). The design
decisions worth remembering:

- **Execution never waits on log persistence.** The capture path is a
  non-blocking O(1) enqueue; when the queue is full the *oldest* queued
  line is dropped, and a failed flush drops its batch rather than
  retrying — a poisoned batch must not wedge the flusher. The flood
  acceptance test (10k lines against a small buffer) pins this.
- **Rings are per attempt, with exactly one writer.** Retries, reclaims,
  and takeovers all mint a new attempt number at claim (ADR-004), so
  per-line `seq` is allocated in-process (an atomic counter on the
  attempt's capture handler) with no coordination. A displaced zombie's
  late flush lands under its old attempt number — harmless, preserved
  diagnostics.
- **The truncation marker is derived, never stored.** Every captured
  line consumes a seq whether or not it survives (the ring-cap trim
  keeps the newest `cap` lines; buffer overflow drops before storage),
  so the API reports `dropped_lines = max(seq) − stored rows` from one
  aggregate read. No marker row to keep consistent.
- **Levels are canonicalized at capture** onto the closed vocabulary
  `debug|info|warn|error` (schema CHECK); the capture level filters
  *before* seq allocation (a filtered line is invisible, not dropped),
  while the API's `level=` parameter filters within what was stored.
  Lines carry `trace_id` (the attempt span's, NULL when tracing is off)
  and the canonical correlation fields as columns, never duplicated
  into the `fields` JSONB.
- **Shutdown loses nothing in hand:** the flusher rides cmd/worker's
  loop context (outliving SIGTERM through the consumer drain, like the
  dispatcher) and performs one final bounded flush before the store
  closes.

Logs remain poll-based in v1 (the recorded non-goal); follow mode is
polling the endpoint's cursor.

### Dashboards & alert rules (as built by ticket 7.5)

- **Dashboards are code.** Two Grafana dashboards — **Engine**
  (`agentloom-engine`) and **API** (`agentloom-api`) — live as JSON in
  `deploy/observability/grafana/dashboards/`, provisioned by a file
  provider (`provisioning/dashboards/dashboards.yml`) with UI edits
  disabled; the provisioned Prometheus datasource carries the stable
  `uid: prometheus` the panels reference. Fleet-wide gauges (queue
  depths, outbox, active workers) are reported identically by every
  worker replica, so panels and rules aggregate them with `max()`.
- **Example alert rules** ship in
  `deploy/observability/prometheus-rules.yml` (queue depth growing, DLQ
  rate spike, reclaim spike, outbox dispatch lag), loaded via
  `rule_files` in the compose Prometheus. Thresholds are dev-scale by
  design so `make smoke-dashboards` can test-fire them. promtool unit
  tests (`prometheus-rules.test.yml`) run in CI via `make obs-lint`,
  pinned to the same Prometheus image tag compose runs.
- **Anti-drift audit.** `TestDashboardsAndRulesReferenceRegisteredMetrics`
  (internal/obs/metrics) extracts every `engine_*` name referenced in
  the dashboards and rules files and fails unless it is a registered
  instrument — renaming a metric breaks the build, not a panel.
- **In-flight gauge.** `engine_api_requests_in_flight` (7.5) is the one
  instrument added for the dashboards: an unlabeled gauge bracketing
  every API request via the extended `api.RequestMetrics` seam.
- See `docs/observability.md` for the operator-facing tour of the
  dashboards, key signals, and the documented alert test-fire.

### Wiring: admin ports, providers, and the off switch

- **Admin listener, both deployables.** `/metrics` (and a plain
  `/healthz`) is served by a small dedicated HTTP server on
  `AGENTLOOM_OBS_METRICS_ADDR`, **empty by default = no listener**. It is
  never part of the API's public route tree: the public port is
  bearer-authed and contract-tested (ADR-007, 6.6), and Prometheus does
  not present bearer keys. In compose, admin ports stay in-network
  (never published to the host), which also sidesteps the host-port
  collision between the two worker replicas.
- **Traces via OTLP.** The OTel SDK exports OTLP/gRPC to
  `AGENTLOOM_OBS_OTEL_ENDPOINT` (Jaeger all-in-one with OTLP enabled in
  dev; a collector is a config change, not a code change). Resources
  carry `service.name` (`agentloom-api`/`agentloom-worker`),
  `service.version` (`internal/version`, ldflags-injected), and
  `service.instance.id` (the worker's consumer name; the API's
  hostname). Sampling is `ParentBased(TraceIDRatioBased(ratio))`,
  default ratio 1.0 — dev traffic is small; production tuning is an env
  knob, not a redeploy.
- **Cleanly disabled by default.** `AGENTLOOM_OBS_OTEL_ENABLED=false`
  installs the no-op `TracerProvider`; an empty metrics addr starts no
  listener. Every existing test layer and the crash suite's subprocess
  workers run with telemetry fully off, no ports, no dials. Compose and
  the integration tests that exercise telemetry opt in explicitly.
- **Telemetry must never take the service down.** OTel SDK internal
  errors are routed to slog at warn (an unreachable collector is a log
  line); a failed admin-port *listen* is a boot error (fail fast on
  misconfiguration), but serve errors after boot are logged and the
  deployable carries on — the same philosophy as the 4.2 health loop.

## Consequences

Easier: 7.2–7.5 implement against fixed names, label budgets, and an
already-wired pipeline; dashboards and KEDA triggers (M20) can reference
metric names with confidence; the cardinality table turns "no unbounded
labels" from a review vibe into a diffable checklist; instance-scoped
registries keep tests hermetic; the durable trace context makes healed
dispatches traceable, which is exactly when an operator needs the trace.

Harder: every new metric/label goes through this ADR's tables — friction
by design; two more ports and three more compose services to know about;
the OTel dependency tree is large (accepted: it is the industry-standard
wire format and the only serious multi-process tracing option); span
links render less intuitively than parent-child in Jaeger's default view
(accepted: links are the semantically honest shape for retries).

## Alternatives considered

- **`/metrics` on the existing service ports.** Rejected: the API port
  is bearer-authed and contract-covered by the 6.2 route-coverage and
  6.6 OpenAPI drift tests — an anonymous scrape route there would
  either punch a hole in auth or force Prometheus auth config; the
  worker has no port at all, so a listener is new either way.
- **Global default Prometheus registry.** Rejected: double-registration
  panics across tests, and it invites `promauto` sprinkled anywhere —
  instance registries keep registration explicit and injectable, per
  project discipline.
- **A second metric namespace per deployable (`api_*`, `worker_*`).**
  Rejected: the ticket and roadmap standardize `engine_*`; the emitting
  service is already a scrape label, and one namespace keeps dashboard
  queries uniform.
- **Trace context only in envelopes (no durable copy).** Rejected: the
  reconciler and requeue/unpark paths enqueue without any live parent
  span, so their dispatches would start orphan traces exactly when
  tracing matters most (crash heals). The run-row column costs one write
  at instantiation.
- **StatsD/OpenTelemetry metrics instead of Prometheus client.**
  Rejected: pull-model Prometheus is the compose/K8s-native choice, is
  what KEDA consumes (M20), and client_golang is the boring,
  battle-tested library. OTel *metrics* remain immature relative to
  client_golang; OTel is adopted for traces only.
- **Pushing spans through an OTel Collector from day one.** Deferred:
  Jaeger all-in-one accepts OTLP directly; a collector adds a hop and a
  config file with no dev benefit. The exporter endpoint is env config,
  so inserting a collector later is a compose change only.
