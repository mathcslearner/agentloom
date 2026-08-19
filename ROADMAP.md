# Roadmap — Durable Execution Engine for AI Agent Workflows

A distributed, durable execution engine purpose-built for AI agent workflows: **Temporal-grade distributed-systems guarantees, AI-native orchestration semantics.**

**Positioning.** The project sits deliberately between three categories:

- **n8n / Zapier** — easy visual automation, but not built for production-grade reliability (no leases, no durable replay, no crash recovery guarantees).
- **Temporal / Airflow** — production-grade durable execution, but not AI-native (a failed LLM step is just "retry the same input"; no cost budgets, no context management, no dynamic agent-driven graphs).
- **LangGraph** — excellent graph-based agent logic, but an in-process library: no distributed worker coordination, no lease-based crash recovery, no fleet-wide rate limiting.

This engine provides: DAG workflows (LLM calls, tool calls, conditionals, fan-out/fan-in), distributed execution across independent worker processes coordinated via Redis Streams, durable state in Postgres with resume-from-last-completed-step, lease-based task claiming with safe expiry and reclaim, and a set of **AI-native differentiators as core features**: semantic/self-correcting retries, dynamic runtime DAG generation, cost-aware scheduling, context/memory management, multi-agent handoff, human-in-the-loop, and a pluggable tool/agent/retrieval interface.

---

## How to use this roadmap

- **Linear execution.** Milestones and tickets are ordered so that dependencies only ever point backwards. Work strictly top-to-bottom; an agent (or engineer) picks the next unfinished ticket, completes it to the Definition of Done, and moves on.
- **One ticket ≈ one focused session (1–4h, ~a couple hundred LOC).** If a ticket balloons past ~4 hours or ~400 LOC, split it and record the split here — the roadmap is a living document.
- **Status convention.** Check off acceptance-criteria boxes as you go; when all are checked, append ` ✅` to the ticket heading. A milestone is done when all tickets are ✅ and its exit criteria hold.
- **Design changes.** If implementation reveals a design flaw, update the governing ADR *and* any affected future tickets in the same change. Never let code and ADRs diverge silently.
- **Architecture docs come first.** Most milestones open with an ADR ticket. Implementation tickets in that milestone must conform to it. ADRs live in `docs/adr/NNN-title.md` (scaffolded in M0).

### Definition of Done (every ticket)

1. `make lint` and `make test` pass; `make test-integration` passes when the ticket touches cross-process behavior (Postgres/Redis/API).
2. Tests are written **in the same ticket** as the behavior: unit tests always; integration tests for anything crossing a process or datastore boundary.
3. New hot paths emit structured logs (and, once M7 lands, metrics/traces) per the observability conventions.
4. ADRs updated if a decision changed; user-facing docs updated if behavior is user-visible.
5. No secrets in code, fixtures, logs, or committed config. No TODOs without a ticket reference.

---

## Stack decisions (and alternatives considered)

| Layer | Choice | Why (and what was rejected) |
|---|---|---|
| Backend language | **Go** | Concurrency primitives map directly onto worker dispatch/lease heartbeats; matches Temporal's implementation language (credibility). Rejected: Python (weaker concurrency story for this workload), Rust (slower iteration for a several-month solo/small-team scope). |
| Work queue + leases | **Redis Streams** (consumer groups) | The pending-entries list (PEL) + `XCLAIM`/`XAUTOCLAIM` natively provide claim/heartbeat/reclaim semantics — the lease mechanism is *the* distributed-systems centerpiece. Redis also serves rate limiting (Lua token bucket), delayed delivery (ZSET), caching, and pub/sub fan-out: one infra component, four roles. Rejected: NATS JetStream (good, but adds a second infra component for little signal), Kafka (operational overkill), SQS (poor local-dev story, vendor lock in the core path), Postgres-only queue via `SKIP LOCKED` (viable but forfeits the required Redis Streams design signal and native lease mechanics). |
| Durable state | **Postgres** (pgx v5 + sqlc, golang-migrate) | Single source of truth; transactional state transitions + outbox in one commit. sqlc gives compile-time-checked queries (fewer hand-rolled scan bugs — valuable for agent-driven development). |
| Expressions (conditional edges, guards) | **cel-go** | Sandboxed, non-Turing-complete, JSON-friendly; same engine used across edge conditions and validators. Rejected: embedded JS (sandboxing burden), Go templates for logic (not analyzable). |
| HTTP / WS | stdlib + **chi**, **coder/websocket** | Boring, maintained, middleware-friendly. |
| Observability | **Prometheus client_golang + OpenTelemetry** (OTLP → Jaeger dev / cluster stack later), `slog` JSON logging | Mandated; trace context propagates through queue message envelopes. |
| Frontend | **Next.js (App Router, TS) + React Flow (`@xyflow/react`) + zustand + Tailwind/shadcn + elkjs** | React Flow is the mandated canvas; elkjs handles auto-layout of runtime-mutated graphs; zustand keeps graph state management simple + undoable. |
| API contract | OpenAPI (`api/openapi.yaml`) + `openapi-typescript` client | One contract drives backend tests and the typed frontend client. |
| Load testing | Custom Go `loadgen` | Needs workflow-lifecycle tracking via API/WS and shared Go types; k6 fits request-level load, not run-level orchestration tracking. |
| Infra | Docker multi-stage → **Helm on EKS**, **RDS**, **ElastiCache**, **ECR**, **Terraform**, **KEDA** | Managed stateful services (backups/HA/patching are not this project's signal); EKS over self-managed control plane for the same reason. KEDA scales workers on queue-depth metrics from our own Prometheus exposition — ties observability directly into autoscaling. Full justification written in ADR-020. |

---

## Scope boundaries (non-goals)

- **No model training or fine-tuning.** LLM steps call real provider APIs (Anthropic, OpenAI) through a provider interface.
- **No building LLMs or the tools themselves** — the engine orchestrates them; integrations arrive via the plugin SPI.
- **Not a general-purpose non-AI workflow platform.** AI-specific features (semantic retries, cost, context, agents) are central to the design, not bolted on. Generic automation breadth (hundreds of connectors) is explicitly out.
- **No fine-grained microservices.** Exactly two long-running deployables (API server, worker) plus shared internal Go packages. A separate scheduler service exists only if ADR-002's escape criteria are ever met.
- **v1 is single-tenant-per-deployment** (API keys scope clients, not isolated orgs). Multi-tenancy is backlog.

---

## Milestone overview

**Phases:** A — core distributed engine (M0–M7) · B — AI-native layer (M8–M15) · C — realtime + UI (M16–M18) · D — scale, infra, release (M19–M21).

The AI-native layer lands immediately after the engine core and *before* any UI or cloud infra — the differentiators are core features, and the earliest milestones deliberately carry their foundations (per-run graph copies for dynamic DAGs, outcome taxonomy reserved for semantic retries, executor middleware chain, park/resume primitives).

| # | Phase | Milestone | Key outcome | Tickets |
|---|---|---|---|---|
| 0 | A | Foundation & architecture | Repo, CI, logging/config, ADR-001/002 | 5 |
| 1 | A | Workflow definition & DAG core | JSON schema + validated graph library | 6 |
| 2 | A | Durable state (Postgres) | Schema, store layer, transactional instantiation; Compose stack | 6 |
| 3 | A | Queue & lease layer (Redis Streams) | Claim/heartbeat/expire/reclaim + delayed delivery | 6 |
| 4 | A | Distributed execution MVP | Multi-worker end-to-end runs; crash-recovery demo #1 | 7 |
| 5 | A | Fault tolerance & execution control | Retries/backoff, timeouts, DLQ, idempotency, cancel/park, drain | 8 |
| 6 | A | API server & auth | API keys, per-client rate limits, full lifecycle API, OpenAPI | 6 |
| 7 | A | Observability | Metrics, cross-worker tracing, step logs, dashboards | 5 |
| 8 | B | Plugin SPI & LLM/tool execution | Providers (Anthropic/OpenAI/mock), tools, retrieval, templating | 8 |
| 9 | B | Distributed rate limiting & caching | Fleet-wide token buckets + response cache w/ key design | 6 |
| 10 | B | Cost tracking & budgets | Real-time $ ledger, budgets, model downgrade, park-on-exceed | 5 |
| 11 | B | Validation & semantic retries | Validator chain, critique-augmented retry loop | 6 |
| 12 | B | Context & memory management | Token accounting, blackboard, automatic compaction | 6 |
| 13 | B | Dynamic DAG (planner steps) | Atomic runtime graph expansion + map fan-out | 7 |
| 14 | B | Multi-agent orchestration | Agent roles, handoffs, critic⇄writer loops | 5 |
| 15 | B | Human-in-the-loop | Park-without-lease approvals, decide API, timeouts | 5 |
| 16 | C | Realtime events & WebSocket | Ordered per-run event feed, backfill-then-tail protocol | 5 |
| 17 | C | Frontend: visual DAG builder | React Flow builder, schema-driven config, serialization boundary | 6 |
| 18 | C | Frontend: live dashboard | Live DAG statuses, cost meter, step inspector, HITL inbox | 6 |
| 19 | D | Load testing & bottlenecks | 1,000+ concurrent runs locally; find & fix bottleneck; publish numbers | 6 |
| 20 | D | Kubernetes & AWS (Terraform) | EKS/RDS/ElastiCache/ECR, KEDA autoscaling, K8s chaos + load re-run | 10 |
| 21 | D | v1.0 polish & release | Example gallery, security pass, docs, versioned release | 5 |

**Total: 22 milestones · 134 tickets.** At one ticket per session, this is roughly 4–7 months of steady part-time work — consistent with a serious multi-month open-source build.

---

# Phase A — Core distributed engine

---

## Milestone 0 — Foundation & architecture

**Goal.** Establish the repository, tooling, CI, and the two architecture decisions everything else leans on: service boundaries and the scheduling model. Also lay down the config and structured-logging packages so *every* subsequent ticket logs consistently from day one.

**Role.** Nothing here is throwaway: the ADR convention, lint/test harness, and `internal/obs`/`internal/config` packages are used by all 21 following milestones.

**Architecture docs:** `docs/architecture.md` (system overview), ADR-001 (service boundaries), ADR-002 (scheduling model).

**Exit criteria:** clean checkout → `make lint test` green in CI; architecture doc + ADR-001/002 merged; a demo binary emits structured JSON logs configured from env.

#### 0.1 — Repo scaffold & tooling ✅
**Depends on:** —
Initialize the monorepo: pick the project name and Go module path (placeholder `github.com/OWNER/NAME` until decided), `go.mod`, directory skeleton (`cmd/`, `internal/`, `web/` placeholder, `deploy/`, `docs/`, `examples/`, `test/`), `Makefile` (`lint`, `test`, `test-integration`, `fmt`), golangci-lint config, `.editorconfig`, `.gitignore`, Apache-2.0 `LICENSE`, README stub with one-paragraph positioning.
**Done when:**
- [x] `make lint` and `make test` pass on the empty skeleton
- [x] Directory layout documented in README and matches CLAUDE.md
- [x] Module path finalized; name recorded in README (`agentloom`, `github.com/mathcslearner/agentloom`)

#### 0.2 — CI pipeline (lint + unit tests)
**Depends on:** 0.1
GitHub Actions workflow: golangci-lint, `go vet`, unit tests with race detector, Go build cache. Runs on PRs and main. (Integration-test job with services arrives in 2.2; frontend job in 17.1.)
**Done when:**
- [x] CI runs lint + `go test -race ./...` on every PR
- [ ] A deliberately failing test fails the pipeline (verified once, then removed)
- [x] Status badge in README

#### 0.3 — Architecture overview document ✅
**Depends on:** 0.1
Write `docs/architecture.md`: component diagram (API, workers, Postgres, Redis, providers, UI), the execution data flow (submit → instantiate → dispatch → claim → execute → complete → fan out), tech-stack justifications (condensed from this roadmap), glossary (run, step, attempt, lease, claim ID/fencing token, outbox, reconciler, blackboard, expansion, semantic retry, park). Mermaid diagrams.
**Done when:**
- [x] All listed sections present; diagrams render on GitHub
- [x] Glossary terms match the vocabulary used in this roadmap
- [x] Doc index page `docs/README.md` links it

#### 0.4 — ADR template, ADR-001 service boundaries, ADR-002 scheduling model ✅
**Depends on:** 0.3
Create `docs/adr/` with a template. **ADR-001:** exactly two long-running deployables — API server and worker; everything else (DAG model, leasing, retries, cost, context, cache, plugins) is shared internal Go packages; rationale + rejected alternatives (microservices, embedded single binary). **ADR-002:** event-driven scheduling with *no central scheduler* — completing workers compute successor readiness transactionally and dispatch via an outbox; every worker participates in dispatch; document the escape criteria that would justify a dedicated scheduler service (e.g., cross-run fairness policies that can't be expressed at claim time) and the planned scale lever (sharded streams, see M19).
**Done when:**
- [x] Both ADRs merged with context/decision/consequences/alternatives sections
- [x] ADR-002 explicitly addresses "why no scheduler bottleneck" and names the sharding lever
- [x] ADR index in `docs/adr/README.md`

#### 0.5 — Config & structured logging foundation ✅
**Depends on:** 0.1
`internal/config`: env-driven config with defaults, validation, and typed sub-configs per component (fail fast on bad config). `internal/obs/log`: `slog` JSON logger factory, canonical field names (`run_id`, `step_id`, `attempt`, `worker_id`, `trace_id`), context-carried logger helpers. A `cmd/demo` throwaway (deleted in M4) proves wiring.
**Done when:**
- [x] Config precedence (default < env) unit-tested; invalid config errors are actionable
- [x] Log output is one JSON object per line with canonical fields
- [x] Logger retrievable from `context.Context`; nil-safe

---

## Milestone 1 — Workflow definition & DAG core

**Goal.** Define the workflow language — the JSON definition format and its in-memory graph model — as a pure, IO-free library: parsing, strict validation, cycle detection (with marked loop edges as the only sanctioned cycles), readiness computation, join semantics, and CEL edge conditions.

**Role.** This schema is the system's central contract: the API accepts it, Postgres snapshots it per run, planners extend it at runtime (M13), and the visual builder serializes to it (M17). Getting strictness and versioning right here prevents an entire class of downstream drift.

**Architecture docs:** ADR-003 (workflow definition format & versioning).

**Exit criteria:** fixture suite of valid/invalid definitions passes; a 10k-node synthetic graph validates and computes readiness in <100ms; generated JSON Schema published in docs.

#### 1.1 — ADR-003: workflow definition format ✅
**Depends on:** 0.4
Decide and document: step types (`llm`, `tool`, `retrieve`, `map`, `planner`, `agent`, `human_approval`, `join`, plus test types like `noop`/`echo`); edges carry optional `when` CEL predicates (parallel fan-out = multiple unconditioned edges; a `branch` step is sugar for exclusive conditioned edges); `join` steps declare mode `all|any`; **loop edges** are explicitly marked (`type: loop`, `condition`, `max_iterations`) and are the only permitted cycles (executed via unrolling, M14); `schema_version` field; per-definition limits (max steps/edges); an optional `ui` block (node positions) the engine ignores but round-trips. Go structs are the source of truth; JSON Schema is *generated* (invopop/jsonschema) for docs and later UI forms.
**Done when:**
- [x] ADR covers every step/edge construct with a JSON example each
- [x] Skip-propagation and join semantics for conditional branches specified
- [x] Versioning and unknown-field policy (strict reject) decided and recorded

#### 1.2 — Definition types & JSON codec ✅
**Depends on:** 1.1
`internal/dag`: Go structs for the definition, strict decoding (unknown fields rejected), canonical encoding, `schema_version` handling, and JSON Schema generation wired into `make generate` with a CI drift check.
**Done when:**
- [x] Round-trip (decode→encode→decode) is lossless for all fixtures, including the `ui` block
- [x] Unknown fields and wrong types produce path-qualified errors
- [x] Generated JSON Schema committed under `docs/schema/` and drift-checked in CI

#### 1.3 — Structural validation ✅
**Depends on:** 1.2
Validate: unique IDs, edge endpoints exist, at least one entry step, no orphan/unreachable steps (warning vs error per ADR), per-type required config present, definition limits enforced. Multi-error reporting with stable error codes (machine-readable for the UI later).
**Done when:**
- [x] Table-driven tests cover every rule with valid + invalid cases
- [x] All violations reported in one pass with error codes and JSON paths
- [x] Fixture corpus extended with one invalid fixture per rule (count/size limits are exercised with generated definitions instead of committed multi-hundred-KB files)

#### 1.4 — Graph algorithms: cycles, topology, readiness ✅
**Depends on:** 1.3
Adjacency construction, cycle detection that rejects all cycles *except* marked loop edges, topological ordering, reachability, and `ReadySteps(completed, skipped, failed)` implementing join modes and skip propagation (a step whose upstreams are all skipped is skipped; `join all` treats skipped parents per ADR-003; `join any` fires on first success). Property-based tests (rapid) for invariants; benchmark on synthetic 10k-node graphs.
**Done when:**
- [x] Loop-marked cycles accepted; any other cycle rejected with the offending path in the error
- [x] Property tests: readiness is monotonic, never returns a step with unmet deps, skip propagation terminates
- [x] Benchmark: 10k nodes validated + readiness computed <100ms

#### 1.5 — CEL edge conditions ✅
**Depends on:** 1.4
Integrate cel-go: compile `when` expressions at validation time (syntax/typecheck against a declared environment: `output` of the source step, `run.params`), evaluation helper used later by the engine. Policy per ADR: evaluation *error* is a step-level failure (recorded), never silently treated as `false`.
**Done when:**
- [x] Invalid expressions rejected at definition-validation time with position info
- [x] Eval unit tests: truthy/falsy routing, missing fields, type errors → typed eval-error
- [x] Expression environment documented in `docs/expressions.md`

#### 1.6 — Canonical example definitions & golden fixtures ✅
**Depends on:** 1.5
Author `examples/definitions/`: linear pipeline, fan-out/fan-in, conditional branch, loop-marked critic cycle, and a kitchen-sink using every construct. These are the shared fixtures for engine tests, serialization round-trips (M17), and docs.
**Done when:**
- [x] All examples pass validation; kitchen-sink exercises every step/edge type
- [x] Fixtures wired into the M1 test suites as golden files
- [x] Each example has a short header comment explaining what it demonstrates

---

## Milestone 2 — Durable state: Postgres persistence

**Goal.** Postgres becomes the source of truth: schema and migrations for definitions, runs, per-run graph copies, attempts, events, and the transactional outbox; a typed store layer; atomic run instantiation; guarded state transitions (CAS) that later underpin fencing. Docker Compose arrives now — the first moment two services exist to run together.

**Role.** Every durability claim ("survives a full crash/restart, resumes from last completed step") reduces to this schema and its transition discipline. The **per-run graph copy** decided here is what makes runtime DAG mutation (M13) a natural extension instead of a rework.

**Architecture docs:** ADR-004 (persistence model & state machines).

**Exit criteria:** `make up` boots Postgres+Redis with healthchecks; migrations apply/rollback cleanly in CI; run instantiation is atomic under injected failures; concurrent CAS test proves single-winner transitions; data survives `make down && make up` (volumes).

#### 2.1 — Docker Compose dev stack ✅
**Depends on:** 0.1
Root `docker-compose.yml`: Postgres 16 + Redis 7 with named volumes, healthchecks, and ports; `.env.example`; Make targets `up`, `down`, `psql`, `redis-cli`, `nuke` (destroy volumes, confirmation-gated). Used for local dev and integration tests from here on.
**Done when:**
- [x] `make up` on a clean checkout reaches healthy state for both services
- [x] Data survives `make down && make up`; `make nuke` documented as destructive
- [x] README quickstart section updated

#### 2.2 — Migration tooling & integration-test harness ✅
**Depends on:** 2.1, 0.2
golang-migrate with migrations embedded via `embed.FS`; `make migrate-up/down/new`. CI gains an integration job (build tag `integration`) that boots Postgres+Redis as services and runs tagged tests. Test helper package provides a per-test isolated schema/database.
**Done when:**
- [x] Up/down migrations tested in CI; dirty-state detection surfaces a clear error
- [x] `make test-integration` works locally against the compose stack
- [x] Parallel tests are isolated (no cross-test data bleed)

#### 2.3 — ADR-004 & core schema v1 ✅
**Depends on:** 2.2, 1.1
**ADR-004:** state-machine tables + append-only event log (why not full event sourcing: replay complexity buys little here since Postgres holds authoritative state; the event log serves audit/UI). Schema v1 migrations: `workflow_definitions` (name, version, spec JSONB), `runs` (status, params, definition snapshot ref, aggregates), `run_steps` + `run_edges` (**per-run graph copy**, `graph_version`, `remaining_deps` counter), `step_attempts` (claim_id, outcome, error, timings), `events` (per-run monotonic `seq`), `task_outbox`. Status enums and the **allowed-transition matrix** documented in the ADR; indexes for hot paths (claimable steps, run status rollup, outbox drain).
**Done when:**
- [x] ERD diagram in ADR; migrations apply on CI
- [x] Transition matrix enumerates every legal `(from, to, guard)` for runs and steps
- [x] `remaining_deps`/join bookkeeping design matches M1 readiness semantics

#### 2.4 — Store layer & transaction helpers ✅
**Depends on:** 2.3
`internal/store`: pgxpool wiring, sqlc-generated queries, repository interfaces (definitions, runs, steps, attempts, events, outbox) with a Postgres implementation, `WithTx(ctx, fn)` helper with correct rollback/commit semantics and error wrapping.
**Done when:**
- [x] CRUD integration tests for definitions and runs pass
- [x] Tx helper tested: panic and error paths roll back; nested use rejected or documented
- [x] sqlc generation wired into `make generate` with CI drift check

#### 2.5 — Atomic run instantiation ✅
**Depends on:** 2.4, 1.4
`CreateRun`: in a single transaction — snapshot the definition, materialize `run_steps`/`run_edges` with `remaining_deps` computed, mark entry steps `ready`, write `task_outbox` rows for them, append `run_created`/`step_ready` events. Idempotent submission via a client-supplied token (unique index).
**Done when:**
- [x] Injected mid-transaction failure leaves zero rows (all-or-nothing verified in test)
- [x] Duplicate submission with the same token returns the original run (no second run)
- [x] Fan-out fixture instantiates with correct `remaining_deps` on join steps

#### 2.6 — Guarded state transitions (CAS) ✅
**Depends on:** 2.5
Transition functions as conditional UPDATEs: e.g., claim = `ready → running` only if status matches *and* sets a fresh `claim_id` + attempt row; completion requires matching `claim_id`. Illegal transitions return typed errors; every transition appends an event. This is the substrate for lease fencing (M4).
**Done when:**
- [x] Concurrency test: N goroutines race to claim one step; exactly one wins, losers get typed `ErrConflict`
- [x] Transition matrix from ADR-004 enforced by tests (every illegal edge rejected)
- [x] Events appended atomically with their transition (same tx)

---

## Milestone 3 — Queue & lease layer (Redis Streams)

**Goal.** The distribution fabric as a standalone library: task envelopes on Redis Streams with consumer groups, the full lease protocol (claim via `XREADGROUP`, heartbeat via `XCLAIM JUSTID`, reclaim via `XAUTOCLAIM` with min-idle), delayed delivery via a ZSET promoter, and a reusable multi-consumer chaos test harness.

**Role.** This is the concrete mechanism beneath crash recovery — designed and tested explicitly, per requirements, before any worker exists. The PEL *is* the lease ledger; Postgres CAS (M2.6) is the fencing backstop. Together they give at-least-once delivery + effectively-once execution.

**Architecture docs:** ADR-005 (dispatch & lease protocol).

**Exit criteria:** two in-process consumers demonstrate kill → reclaim within lease TTL + ε; heartbeats keep long tasks unreclaimed; delayed tasks promote on schedule; the failure matrix in ADR-005 has a test or explicit rationale per cell.

#### 3.1 — ADR-005: dispatch & lease protocol ✅
**Depends on:** 0.4, 2.3
Document the full protocol: task envelope schema (versioned; `run_id`, `step_id`, enqueue reason, trace context placeholder); one `steps:ready` stream + `workers` consumer group (sharding by `hash(run_id)` documented as the M19 scale lever); **lease = PEL entry** — lease TTL is the `XAUTOCLAIM` min-idle threshold; heartbeat = `XCLAIM JUSTID` to self (JUSTID so delivery count is *not* inflated — delivery count is the poison-message signal); ack discipline (ACK only after the Postgres completion/park transition commits); fencing tokens (`claim_id`) reject zombie writes; the crash matrix (die before claim / after claim before PG transition / mid-execute / after PG commit before ACK / after ACK) with the recovery path for each; orphan-consumer cleanup.
**Done when:**
- [x] Sequence diagrams for happy path, crash-reclaim, and zombie-fenced-write
- [x] Every crash-matrix cell has a stated recovery mechanism
- [x] Envelope versioning and compatibility policy recorded

#### 3.2 — Stream primitives: producer & group bootstrap ✅
**Depends on:** 3.1, 2.1
`internal/queue`: go-redis client wiring, envelope encode/decode with version field, `XADD` producer, idempotent consumer-group creation (race-safe `BUSYGROUP` handling), stream/PEL introspection helpers (`XLEN`, `XPENDING` summaries) for later metrics.
**Done when:**
- [x] Integration tests: produce/consume round-trip; group creation race-safe under concurrency
- [x] Envelope rejects unknown versions with a typed error
- [x] Introspection helpers return depth + PEL counts used by M7 metrics

#### 3.3 — Consumer loop with ack/nack semantics ✅
**Depends on:** 3.2
Blocking `XREADGROUP` batch loop feeding a per-message handler; explicit ACK on handler success; no-ACK on failure (message stays in PEL for redelivery); delivery-count surfaced to the handler; clean shutdown via context.
**Done when:**
- [x] At-least-once proven in test: kill consumer pre-ACK → message redelivered to another consumer
- [x] Handler panic is contained (message survives for redelivery; consumer loop lives)
- [x] Batch size/block timeout configurable; shutdown drains in-flight handler

#### 3.4 — Lease heartbeat & reclaimer ✅
**Depends on:** 3.3
Heartbeater goroutine per in-flight task: periodic `XCLAIM JUSTID` to self with jitter, stops on completion/cancel. Reclaimer loop in every consumer: `XAUTOCLAIM` with min-idle = lease TTL, cursor handling; reclaimed messages re-enter the local handler path; delivery count > threshold flags poison (handed to a callback — DLQ wiring lands in M5.4). Periodic orphan-consumer janitor (`XGROUP DELCONSUMER` for dead consumers with empty PEL).
**Done when:**
- [x] Integration: consumer A killed mid-task → B reclaims within TTL + ε and completes
- [x] Long task with heartbeat is never reclaimed across 3× TTL
- [x] Heartbeat uses JUSTID (delivery count unchanged); reclaim increments it (asserted)

#### 3.5 — Delayed delivery (ZSET promoter) ✅
**Depends on:** 3.2
`sched:delayed` sorted set (score = fire-at epoch ms) + promoter loop in every consumer: atomic Lua script pops due entries and `XADD`s them to the ready stream (duplicate-safe: promotion is at-least-once; claims dedupe downstream). Foundation for retry backoff (M5), throttled requeue (M9), and approval timeouts (M15). Injectable clock for tests.
**Done when:**
- [x] Due tasks promote within one tick; not-yet-due tasks never promote (fake clock tests)
- [x] Lua move is atomic (no lost/duplicated entries under concurrent promoters — stress test)
- [x] Promotion latency observable (hook for M7 metric)

#### 3.6 — Queue chaos harness ✅
**Depends on:** 3.4, 3.5
`internal/queue/queuetest`: harness that spawns N consumers in-process with individual kill switches, scripted handler behaviors (succeed/fail/hang/panic), fake-clock injection where possible, and assertion helpers (delivered-exactly-once-per-claim, PEL empty at quiescence). Used by M4/M5 chaos tickets.
**Done when:**
- [x] Harness supports kill-at-phase hooks (pre-handle, mid-handle, pre-ack)
- [x] M3 integration tests refactored onto the harness
- [x] Quiescence assertions (stream drained, PEL empty, delayed empty) reusable

#### 3.7 — Post-M3 audit: stream retention & heartbeat ownership guard ✅
**Depends on:** 3.6
Two protocol gaps found by the M3 completion audit. (a) Retention: `XACK` clears the PEL but not the stream, so nothing ever removed acked entries — Redis memory grew without bound. Add a trim duty to every consumer: exact `XTRIM MINID` at the smallest pending entry ID (or the successor of last-delivered when the PEL is empty), never touching pending or undelivered entries. (b) Heartbeat: `XCLAIM` min-idle 0 unconditionally transfers ownership, so a stalled consumer resuming after its lease was reclaimed silently stole the entry back from the legitimate holder — safe (fenced) but invisible in logs and contrary to ADR-005's R1(b) narrative. Guard the beat with an atomic owner check; displacement logs and stops the heartbeater.
**Done when:**
- [x] `TrimAcked` + per-consumer trim duty (`TrimInterval`, default 1m, config-wired); pending and undelivered entries never trimmed (integration-tested)
- [x] Group lag — the 3.6 quiescence probe's drained signal — stays computable after trims (asserted)
- [x] Heartbeat claims only entries still owned by the beater; displacement is detected, logged, and stops the heartbeater (integration-tested)
- [x] ADR-005 amended: retention section, ownership-guarded heartbeat, tuning-table row

---

## Milestone 4 — Distributed execution MVP

**Goal.** First end-to-end life: `cmd/worker` and a minimal `cmd/api`. Submit a JSON workflow → entry steps dispatched → multiple independent worker processes claim, execute (test executors), complete, and fan out successors via the transactional outbox → run completes. Includes the flagship crash-recovery integration test.

**Role.** This milestone welds M1–M3 into the event-driven engine of ADR-002: no central scheduler — the completing worker computes readiness; the outbox + reconciler close every dual-write gap between Postgres (truth) and Redis (dispatch).

**Architecture docs:** none new (ADR-002/004/005 govern); `docs/architecture.md` updated with the realized execution walkthrough.

**Exit criteria:** compose stack + 2 workers execute linear and fan-out/fan-in fixtures to completion; `make demo-crash` kills a worker mid-run and the run completes with reclaim visible in attempt history; full stack restart mid-run resumes from last completed step.

#### 4.1 — Executor interface v0 & test executors ✅
**Depends on:** 1.2
`internal/exec`: `Executor` interface (`Type() string`, `Execute(ctx, StepContext) (Output, error)`), registry keyed by step type, typed config decoding. Test executors: `noop`, `echo` (returns input), `sleep` (duration), `fail_n_times` (state via attempt number). Full plugin SPI arrives in M8 — keep v0 minimal but shaped for that refactor. *(As built: a fifth test executor, `counter`, joined in 4.7.)*
**Done when:**
- [x] Registry lookup + config decode unit-tested (unknown type → typed error)
- [x] `StepContext` exposes step config, rendered input, attempt number, logger
- [x] All four test executors behave per spec in unit tests

#### 4.2 — Worker skeleton: consume → claim ✅
**Depends on:** 4.1, 3.4, 2.6
`cmd/worker`: config, queue consumer wiring, and the claim path — on message receipt, attempt the Postgres CAS `ready → running` (fresh `claim_id`, new attempt row, heartbeater started). Already-terminal/already-claimed/unknown steps: ACK and drop (this is what makes at-least-once delivery safe). Structured logs on every branch.
**Done when:**
- [x] Two workers racing one message/step: exactly one executes (integration test)
- [x] Duplicate delivery of a completed step is ACKed-and-dropped without side effects
- [x] Worker starts/stops cleanly under compose with health logging

#### 4.3 — Execute & complete pipeline (readiness fan-out) ✅
**Depends on:** 4.2, 1.5
After executor success, one transaction: persist output, CAS `running → succeeded` (fenced by `claim_id`), evaluate outgoing edges (CEL conditions; skip propagation), decrement successors' `remaining_deps`, collect newly-ready steps, write their outbox rows, append events. Then ACK the message and nudge the dispatcher.
**Done when:**
- [x] Linear, fan-out/fan-in, and conditional fixtures complete correctly across 2 workers
- [x] Join `all`/`any` and skip propagation verified end-to-end
- [x] Completion is a single tx (asserted by failure-injection leaving pre-completion state)

#### 4.4 — Outbox dispatcher & reconciler ✅
**Depends on:** 4.3
Every worker runs: (a) outbox drain loop — `SELECT … FOR UPDATE SKIP LOCKED` batches → `XADD` → delete outbox row; (b) periodic reconciler — re-outboxes steps stuck in `ready` beyond a threshold (covers crash between commit and dispatch, and lost messages) and flags runs with impossible states. Both safe under many concurrent workers.
**Done when:**
- [x] Kill-between-commit-and-XADD test: reconciler recovers the step; run completes
- [x] SKIP LOCKED drain shows no duplicate dispatch under 4 concurrent drainers (dupes that do occur are ACK-dropped at claim — asserted)
- [x] Reconciler is idempotent and rate-bounded (no thundering herd)

#### 4.5 — Fencing enforcement (zombie writes) ✅
**Depends on:** 4.3
Completion/failure transitions require the matching `claim_id`; a stale worker (lease expired, step reclaimed) gets a typed fencing error, logs it, ACKs nothing new, and abandons. Integration test: pause worker A past lease TTL (simulated stall), let B reclaim and complete, resume A → A's write is rejected; no duplicate successor dispatch (outbox keyed idempotently per step transition). Also upgrades 4.4's reconciler on stale-`running` steps from flag to heal (takeover + re-outbox, ADR-005 R1(c)) — the `running → ready` takeover CAS it needs is built here.
**Done when:**
- [x] Zombie write rejected with fencing error; state reflects B's result only
- [x] Successors dispatched exactly once (event log asserted)
- [x] Fencing rejection visible in logs with both claim IDs

#### 4.6 — Minimal ingest API & `ctl` CLI ✅
**Depends on:** 2.5
`cmd/api` (dev mode, no auth yet): `POST /v1/runs` (inline definition or stored definition ref), `GET /v1/runs/{id}` (status + step tree + attempts), `GET /healthz`. Cobra `cmd/ctl`: `submit <file>`, `watch <run-id>` (poll + render status tree), `validate <file>` (local M1 validation). Compose gains the api + worker services (Dockerfiles: simple builder stage now; hardened in M20). To make the canonical fixtures executable, `llm`/`tool`/`retrieve` run as deterministic dev-stub executors until M8/M9.
**Done when:**
- [x] `ctl submit examples/definitions/fanout.json && ctl watch …` reaches `succeeded` on compose
- [x] Invalid definitions rejected with M1's path-qualified errors (400)
- [x] `docker compose --profile app up` (`make up-app`) runs api + 2 worker replicas + stores end-to-end — the `app` profile keeps `make up`/CI stores-only

#### 4.7 — Flagship crash-recovery integration test & demo ✅
**Depends on:** 4.4, 4.5, 3.6
The headline guarantee, automated: harness (or compose-driven test) starts 2 workers, submits a workflow of `sleep` steps, SIGKILLs the worker holding a lease mid-step, asserts: another worker reclaims after lease expiry, the run completes, attempt history shows the reclaim, no step executed effects twice (echo-side-effect counter). Also: full-stack restart (api+workers) mid-run → resumes from last completed step. Packaged as `make demo-crash` with narrated output, documented in `docs/demos/crash-recovery.md`. *(As built: the CI test spawns real `cmd/worker` processes and SIGKILLs them — enabled by new `AGENTLOOM_QUEUE_STREAM`/`_GROUP`/`_DELAYED_KEY` isolation knobs; the side-effect counter became a fifth test step type, `counter`, appending one line per execution to a per-step file — it also serves M5's no-double-fire exit criterion.)*
**Done when:**
- [x] Test is deterministic in CI (tuned TTLs; no sleeps-as-synchronization)
- [x] Attempt history proves reclaim (worker A claimed, worker B completed)
- [x] `make demo-crash` runs the scenario against compose with human-readable narration

---

## Milestone 5 — Fault tolerance & execution control

**Goal.** Production failure semantics: retry policies with exponential backoff + jitter (via the delayed queue), execution timeouts, dead-letter handling with requeue, idempotency keys + a side-effect journal, run-level cancel/park/resume and deadlines, graceful worker drain, and a sustained chaos test.

**Role.** This turns "it runs" into "it survives." The outcome taxonomy introduced here (transient / permanent / timeout — with `validation_failed` deliberately reserved) is the hook semantic retries (M11) plug into. Park/resume built here is the substrate for budget halts (M10) and human approvals (M15).

**Architecture docs:** ADR-006 (failure taxonomy & retry semantics).

**Exit criteria:** `fail_n_times` fixtures retry per policy and land in DLQ when exhausted; DLQ requeue works; side-effect counter proves no double-fire across kill/reclaim/retry; rolling worker restart loses nothing; chaos suite green in CI short mode.

#### 5.1 — ADR-006 & retry policy schema ✅
**Depends on:** 0.4, 4.3
**ADR-006:** error classification (transient / permanent / timeout / cancelled; `validation_failed` reserved for M11), step-vs-run failure semantics, workflow failure policy (`fail_fast` vs `continue_independent_branches`), DLQ model (Postgres as truth; steps become `dead_lettered`), retry policy fields on steps (`max_attempts`, backoff base/cap/multiplier, jitter, retryable error classes) with engine defaults. Definition schema + validation updated (M1 types extended).
**Done when:**
- [x] Taxonomy table maps every executor error path to a class and disposition
- [x] Policy schema validated with sensible bounds (e.g., backoff cap required)
- [x] Defaults documented; fixtures updated with explicit policies

#### 5.2 — Retry engine with backoff ✅
**Depends on:** 5.1, 3.5
On classified-transient failure: record failed attempt, compute backoff (exponential + full jitter), CAS step to a retry-wait state, schedule re-dispatch via the delayed ZSET, ACK the original message. Exhausted attempts → permanent failure path (5.4). Attempt counter durable in Postgres (survives worker death). *(Post-M4 audit note resolved in 5.2's migration: `task_outbox(run_id, step_id)` is indexed — the retry reconciler heal added a third anti-join over the outbox.)*
**Done when:**
- [x] `fail_n_times(2)` with `max_attempts=3` succeeds on attempt 3; timings honor backoff (fake clock)
- [x] Exhaustion transitions to the permanent-failure path with full error history
- [x] Crash between attempt-fail commit and delayed-schedule is healed by the reconciler

#### 5.3 — Step execution timeouts ✅
**Depends on:** 5.2
Per-step `timeout` config: executor context cancelled at deadline; attempt recorded as `timeout` class (distinct from crash — the worker is alive to report it) and routed through the retry engine. Watchdog interplay documented: lease heartbeat continues *during* a hung executor until the watchdog kills it, so timeout ≠ reclaim. *(As built: `timeout` is a step-envelope field like `retry`, materialized onto `run_steps` by migration 0004; enforcement is synchronous cooperative cancellation — no detached goroutine, so no leak and no intra-process double execution by construction — with a joined watchdog goroutine that logs a deadline overrun; the class is judged from context state, parent cancellation is not a timeout, and a success racing the deadline is honored.)*
**Done when:**
- [x] `sleep(10s)` with `timeout=1s` records a timeout attempt and retries per policy
- [x] Executor goroutine actually terminates (no leak — asserted with goroutine count)
- [x] Timeout vs crash distinguishable in attempt history/events

#### 5.4 — Dead-letter handling ✅
**Depends on:** 5.2, 3.4
Terminal failures (exhausted retries, permanent class, poison messages from 3.4's delivery-count cap): step → `dead_lettered` with a `dead_letters` record (full attempt/error context, payload ref); run disposition per workflow failure policy; events emitted. Internal requeue op (reset to `ready`, clear attempts counter per policy, outbox) — exposed via API in M6.5. *(As built: `running → dead_lettered` is a direct claim-fenced CAS like 5.2's retry route; `runs.on_failure` + `steps_cancelled` materialized by migration 0005; the continue write-off is a pure fixed-point walk (`planWriteOff`) that resolves no edges, making requeue revival a recompute + status flip; attempt history stays immutable — the requeue budget counts from the `dead_letters.attempts_at_death` baseline inside `CountCountedFailures`; poison dead-letters unfenced from any non-terminal status with the raw envelope preserved, undecodable-envelope poison is logged-and-consumed; the reconciler's step scans now require the run to be running, ending the failed-run re-outbox churn loop.)*
**Done when:**
- [x] Exhausted step lands in DLQ with complete context; `fail_fast` fails the run, `continue` lets independent branches finish
- [x] Poison message (handler crash loop) reaches DLQ instead of redelivering forever
- [x] Requeue op re-executes the step and completes the run (integration test)

#### 5.5 — Idempotency keys & side-effect journal ✅
**Depends on:** 4.5
`StepContext` exposes a **stable idempotency key** per (run, step) — unchanged across attempts and reclaims — for external calls. `internal/exec/effects`: journal helper (`record-intent → execute → record-result` in `side_effects` table) so executors can make external side effects effectively-once; journaled results short-circuit re-execution on retry. Test executor `effectful_echo` increments an external counter through the journal. *(As built: the key is derived, not stored — a UUIDv5 over a fixed project namespace and `run_id/step_id`, stable by construction; the journal runs each phase in its own short transaction, never spanning the external call, with dangling-intent takeover (the documented residual at-least-once window the key absorbs externally) and a first-wins result write; misuse loudness is `AGENTLOOM_EFFECTS_STRICT` — panic riding the consumer's panic path into poison DLQ (default, dev/test) vs. a permanent-classified dead-letter; `effectful_echo` gained a `fail_times` knob so a retrying step proves the short-circuit: N+1 attempts, one file line.)*
**Done when:**
- [x] Key stable across retry and reclaim (asserted in kill/reclaim test)
- [x] Journaled effect executes once despite retry + reclaim + zombie scenarios
- [x] Journal misuse (execute without intent) fails loudly in dev/test mode

#### 5.6 — Cancel, park/resume, run deadlines ✅
**Depends on:** 5.2
Run-level controls as engine ops (API exposure in M6.5): **cancel** (cooperative: run → `cancelling`; claim path refuses parked/cancelled runs; in-flight executors get context cancellation at next heartbeat; steps → `cancelled`; run terminal), **park/unpark** (pause dispatch with a typed reason — `manual`, later `budget_exceeded`/`awaiting_human`; unpark re-outboxes all `ready` steps), and optional run `max_wall_clock` deadline → cancel with reason. *(As built: cancel converges through three mechanisms — the request transaction sweeps every claimless non-terminal step and finalizes when nothing is in flight; completion transactions re-check the run status under the run lock (success honored without fan-out, failure settled as `cancelled` with no retry/DLQ judgment — ADR-006 row 8); a cancellation-watch goroutine polls run status every `AGENTLOOM_WORKER_CANCEL_POLL_INTERVAL` (default 10s ≈ heartbeat cadence) and cancels the executor context, pure latency over the in-tx check; the reconciler heals dead-worker cancelling runs with takeover + cancel + rollup. Park is a pure dispatch pause — completions and their fan-out proceed, rollups fire from parked, unpark re-outboxes ready-without-outbox steps under new reason `unpark`. The deadline is `max_wall_clock` on the definition envelope, materialized as `runs.deadline_at`, enforced by a fourth reconciler scan feeding the same cancel sweep.)*
**Done when:**
- [x] Mid-run cancel: no new claims, in-flight steps cancel, terminal state consistent (no orphan leases/PEL entries)
- [x] Park → fleet stops claiming that run; unpark resumes to completion
- [x] Deadline exceeded → run cancelled with `deadline_exceeded` reason and event

#### 5.7 — Graceful shutdown & drain ✅
**Depends on:** 5.6
SIGTERM: worker stops claiming, finishes in-flight steps (heartbeating until done), ACKs, then exits; configurable drain timeout after which it abandons (lease expires naturally → reclaim). This is what K8s `preStop`/rolling restarts (M20) rely on. *(As built: a two-phase shutdown inside the consumer — soft stop suspends reads and every periodic duty, while handlers keep running under a work context that survives the cancellation until a watchdog cancels it at `ConsumerConfig.DrainTimeout`; the drain covers the whole PEL in hand (in-flight handler, read-batch remainder, reclaimed entries mid-pass), each entry getting a logged disposition (drained/redeliver/abandoned) plus a summary; the abandon path is deliberately the crash path — the engine returns without attempting a completion on a canceled handler context, the entry stays un-acked, and the lease expires into reclaim/takeover; a fully clean drain deregisters the consumer from the group (guarded by a provably-stable empty-PEL check), so graceful restarts strand no orphan; the worker's dispatch loops outlive SIGTERM until the consumer finishes so draining completions' successors still dispatch; `DrainTimeout` zero preserves the pre-5.7 immediate-cancel semantics the queuetest kill switches rely on to simulate crashes, and the production knob `AGENTLOOM_WORKER_DRAIN_TIMEOUT` — default 25s, sized inside K8s's 30s grace — must be positive.)*
**Done when:**
- [x] Rolling restart of 2 workers under continuous load: zero lost runs, zero reclaim churn from drained workers
- [x] Drain-timeout path verified: abandoned step reclaimed by survivor
- [x] Shutdown sequence logged with per-step disposition

#### 5.8 — Sustained chaos suite ✅
**Depends on:** 5.7, 5.5, 3.6
Chaos test: continuous submitter (mixed fixtures incl. retries and effectful steps), random worker kills every few seconds, one Redis restart blip; assert at quiescence: all runs terminal-succeeded (or explainably dead-lettered), side-effect counters exactly at expected values, PEL/outbox/delayed all empty, reconciler healed every gap. CI short mode (~1 min) + longer local mode. *(As built: `TestSustainedChaos` in `test/crash` — five-fixture round-robin (chain, retry, effectful, fan-out/join, deliberate dead-letter) against a 3-worker fleet on a **dedicated `redis-chaos` compose service**, restarted mid-test via `SHUTDOWN NOSAVE` + `restart: always` (proven by the server run_id changing — go-redis client retries mask the sub-second downtime) so the blip never touches the shared test Redis; the reconciler runs hot (1s sweeps) because the blip's AOF tail loss is healable only by its scans; expected terminal states are kill-proof by construction (`lost` excluded from budgets, `fail_n_times` keyed off durable attempts, poison threshold raised out of kill range); journaled effects asserted exactly-once **by idempotency key** (raw line count is the journal's documented residual at-least-once window), unjournaled counters bounded by [1, attempts] as the motivating contrast; long mode via `AGENTLOOM_CHAOS_DURATION` / `make test-chaos-long`.)*
**Done when:**
- [x] CI short mode green and deterministic across 5 consecutive runs
- [x] Effect counters exact (no dupes/losses) after chaos
- [x] Quiescence invariants asserted via 3.6 helpers; failures dump full diagnostic state

---

## Milestone 6 — API server & auth

**Goal.** Harden the dev API into the real product surface: API keys with scopes, per-client rate limiting (the Redis token-bucket built here is reused fleet-wide in M9), the full run/definition lifecycle API with pagination and idempotent submission, and an OpenAPI contract.

**Role.** Auth and client-side rate limiting are explicit requirements, deliberately separate from internal LLM/tool rate limiting. The OpenAPI contract becomes the source for the typed frontend client (M17).

**Architecture docs:** ADR-007 (authn/z & API rate limiting).

**Exit criteria:** all `/v1` routes require scoped keys; per-key limits return 429 with correct headers; lifecycle endpoints (submit/list/get/cancel/requeue-DLQ) covered by contract tests; OpenAPI spec published and drift-checked.

#### 6.1 — ADR-007 & API key model ✅
**Depends on:** 0.4, 2.4
**ADR-007:** bearer API keys (`sk_`-prefixed, 32B random; store SHA-256 hash + short lookup prefix; plaintext shown once), scopes (`submit`, `read`, `approve`, `admin`), key lifecycle (create/revoke/expire), rate-limit design shared with M9, and why API keys over JWT for v1 (service-to-service simplicity; JWT/OIDC listed as backlog). Migration for `api_keys`; `ctl keys create/list/revoke` (admin bootstrap via env-provided root key). *(As built: key = `sk_` + base64url(32B), lookup prefix = first 11 chars UNIQUE with regenerate-on-collision, hash = fast SHA-256 by design (256-bit secret, no KDF); admin implies all scopes; TTL resolved server-side against the injected clock; revoke is soft and first-wins; the `AGENTLOOM_API_ROOT_KEY` bootstrap credential is hashed at boot, logged as `key_id="root"`, never stored; key management is API routes (`POST/GET /v1/keys`, `DELETE /v1/keys/{id}`) because ctl stays a pure HTTP client — the `/v1/keys` subtree is the only gated surface until 6.2, via a scope-parameterized `requireScope` middleware 6.2 generalizes; 401 collapses every credential failure indistinguishably, 403 names the missing scope; CI greps for committed `sk_`-shaped literals, so tests construct keys at runtime.)*
**Done when:**
- [x] Keys stored hashed; plaintext never persisted or logged (grep-test in CI)
- [x] `ctl keys` round-trip works against compose
- [x] Scope model documented with a route→scope table

#### 6.2 — Auth middleware ✅
**Depends on:** 6.1
Bearer parsing, prefix lookup + constant-time hash compare, revocation/expiry checks, scope enforcement per route, `key_id` injected into request context/logs. `/healthz`, `/readyz`, `/metrics` exempt. 401 vs 403 semantics per ADR. *(Post-M4 audit notes for this ticket: compose publishes the dev API on `0.0.0.0` — flip to `127.0.0.1` or make auth the gate here; and the `counter` test executor writes to an arbitrary submitted filesystem path — decide whether test executors stay registered on authed deployments.)* *(As built: 6.1's scope-parameterized `requireScope` mounted per-route per ADR-007's table — submit on `POST /v1/runs`, read on `GET /v1/runs/{id}`, admin on `/v1/keys` — with a chi.Walk route-coverage test failing any /v1 route missing from the route→scope table, so a new endpoint cannot ship anonymous by omission; 404/405 fallbacks deliberately anonymous (outside the middleware tree, route existence is public). The middleware stamps the authenticated identity into the request context (6.4's per-key rate-limit hook) and reports key_id back up to the per-request log line via a mutable slot. Both parked audit items resolved: compose api binds `127.0.0.1` by default (`AGENTLOOM_API_BIND` overrides), and the filesystem-writing test executors (counter, effectful_echo) moved out of the production default registry — `exec.CoreBuiltins()` unless `AGENTLOOM_WORKER_TEST_EXECUTORS=true` (binary default false; compose sets true), unregistered types dead-lettering permanent at claim time per 5.4. ctl already sent the bearer everywhere; demo-crash authenticates as the root credential, minting an ephemeral one when `.env` has none.)*
**Done when:**
- [x] Every `/v1` route rejects missing/invalid/revoked keys (table-driven route test)
- [x] Scope violations → 403 with machine-readable error body
- [x] Auth outcomes logged with `key_id`, never with the key itself

#### 6.3 — Redis token-bucket limiter (shared library) ✅
**Depends on:** 3.2
`internal/ratelimit`: atomic Lua token bucket (capacity, refill rate, variable cost per acquire), returning `allowed`, `remaining`, `retry_after`. Deliberately generic: M6.4 uses it per API key; M9 reuses it per LLM resource. Correctness under concurrency is the point. *(As built: `Limiter.Acquire(ctx, Bucket{Key, Capacity, RefillPerSec}, cost)` → `Result{Allowed, Remaining, RetryAfter}`; the script reads Redis `TIME` — one clock for all acquirers of a shared bucket, deliberately not caller-injected time, with fake time reaching the script only through a test-only ARGV override behind `export_test.go`; state is one hash per bucket key with absent-key-=-full-bucket and TTL re-armed to time-to-full so idle buckets self-clean (rate-zero quotas PERSIST instead); balance serialized `%.17g` so the float64 round-trips exactly, letting the rapid property test demand exact equality against a pure-Go model; `cost > capacity` is typed `ErrCostExceedsCapacity` and never-refilling denials report `RetryAfterNever`, both for M9's wait-vs-perm-fail distinction; no logging in the library — callers own deny/429 semantics.)*
**Done when:**
- [x] Stress test: N concurrent clients never over-grant beyond capacity (strict accounting)
- [x] Refill math property-tested with fake time (Lua uses Redis TIME — injectable in tests via wrapper)
- [x] Benchmark documents acquire latency (<1ms local target)

#### 6.4 — Per-client API rate limiting middleware ✅
**Depends on:** 6.3, 6.2
Per-`key_id` buckets (configurable per scope/route class: submits stricter than reads) plus a global safety bucket. 429 responses with `Retry-After` and `X-RateLimit-*` headers. Metrics hooks stubbed for M7. *(As built: `rateLimit(class)` middleware mounted after `requireScope` on every `/v1` route — buckets keyed `<prefix>:<key_id>:<class>` off the authenticated identity (root rides under `"root"`), so 401/403s consume no tokens; route→class mirrors the scope table with the same chi.Walk coverage test, so a new route cannot ship unclassified. Per-key acquired before global — an abusive client's 429 storm cannot drain the shared budget; the accepted cost (a global denial does not refund the per-key token) is documented in ADR-007. Fail-open on Redis errors: cmd/api's new Redis client serves rate-limit buckets only (ADR-002 untouched), opens without a boot dependency, and an acquire failure logs + allows — Postgres stays the API's only hard dependency. Headers always describe the caller's class bucket; `X-RateLimit-Reset` derived as ceil((capacity−remaining)/refill) per the 6.3 deferred decision; `Retry-After` whole-seconds rounded up from the denying bucket; 429 body carries new envelope code `rate_limited`. Config: `AGENTLOOM_API_RATELIMIT_{ENABLED,KEY_PREFIX,{SUBMIT,READ,ADMIN,GLOBAL}_{CAPACITY,REFILL_PER_SEC}}`, refill required strictly positive (a rate-zero API bucket would brick a key). `RateLimitMetrics` seam (decisions + fail-open) no-op until M7. Integration suite drives threshold exactness with header sequence, refill recovery by bounded polling (the limiter's clock is Redis's — the deliberate 6.3 divergence), global-bucket protection with every key under its own limit, per-key/per-class isolation, root-key limiting, and no-consumption on credential failures.)*
**Done when:**
- [x] Integration test drives a key to its limit; 429 exactly at threshold; recovery after refill
- [x] Route classes configurable via config file/env (tested)
- [x] Global bucket protects the API even when individual keys are under their limits

#### 6.5 — Run & definition lifecycle endpoints ✅
**Depends on:** 6.2, 5.4, 5.6
Definitions: create (validated), new-version, list, get. Runs: submit (by definition ref or inline; `Idempotency-Key` header honored), list with keyset pagination + filters (status, definition, time range), get (status tree, attempts, timings), cancel, park/unpark, requeue-dead-lettered-step. Consistent error envelope with codes. *(Post-M4 audit notes: bound the idempotency token's length with a 400 — today an over-long token 500s on the btree index limit — and fingerprint the token to its payload so replaying a token with a different definition 409s instead of silently returning the original run.)* *(As built: lifecycle handlers call `engine.Control` — Cancel/Park/Unpark/Requeue extracted from Engine, which now embeds it — built by `api.New` with no dispatcher nudge (worker drain cadence dispatches, ADR-002 untouched); wrong-state refusals → 409 with new envelope code `conflict` (Requeue's cancelled-run refusal became the typed `engine.ErrRunNotRequeueable`); definitions registry = `POST /v1/definitions` (canonical spec at version 1, 409 on an existing name) + `POST /v1/definitions/{name}/versions` (next version, allocation serialized by a per-name `pg_advisory_xact_lock` so concurrent appenders get consecutive versions) + get-by-id and both listings (latest-per-name keyset by name); `GET /v1/runs` = one `ListRunsPage` query, order flipped uniform `(created_at DESC, id DESC)` with a row-value cursor predicate served by migration 0009's two new indexes, opaque base64url cursors, nullable filters; idempotency per the audit notes — `Idempotency-Key` header replaces the body field, ≤ 200 bytes → 400, `runs.idempotency_fingerprint` (0009) binds token→payload with mismatch → 409 `idempotency_key_conflict`, params canonicalized before hashing, pre-0009 rows grandfathered, tokens deliberately global per ADR-007; `GET /v1/runs/{id}` gained `dead_letters` so clients can discover requeueable steps, and RunView the 5.x columns; ctl grew runs/cancel/park/unpark/requeue with `--token` moved to the header; e2e suites drive DLQ-requeue-to-success, in-flight cancel, and park/strand/unpark through the API against the production dispatcher + fleet.)*
**Done when:**
- [x] Contract tests for every endpoint (success + auth + validation failures)
- [x] Keyset pagination stable under concurrent inserts (no skips/dupes across pages)
- [x] DLQ requeue + cancel + unpark round-trip through the API in integration tests

#### 6.6 — OpenAPI contract & docs ✅
**Depends on:** 6.5
Hand-maintained `api/openapi.yaml` as the contract: all routes, schemas (reusing generated JSON Schema for definitions), auth scheme, error envelope, examples. CI: spec lints, and a route-coverage check compares the chi route table against the spec (drift fails). *(As built: OpenAPI 3.1 — its schema dialect is JSON Schema 2020-12, so the workflow-definition schema is `$ref`'d straight from the generated `docs/schema/workflow-definition.v1.json` (root points at `#/$defs/Definition`) with zero duplication; every wire type in `internal/api/types.go` has a hand-maintained component schema with the closed vocabularies (run/step statuses, attempt outcomes, DLQ sources, scopes, error codes) as enums; request schemas are `additionalProperties: false` matching `DisallowUnknownFields`, responses left open for additive evolution. Lint = vacuum (Go-native, `go run`-pinned like sqlc) via `make openapi-lint` with `--fail-severity warn`; `api/vacuum.ruleset.yaml` disables exactly three recommended rules with in-file justifications (snake_case is the contract, untyped = any-JSON is deliberate, per-property examples are noise) — spec scores 100/100 with everything else on. Drift check = `TestOpenAPIRouteCoverage` in `internal/api` (plain unit test → runs in the existing CI test job): chi.Walk vs the spec's `paths` compared both directions with path-param names normalized (`{runID}` vs `{run_id}` don't matter, spec params stay snake_case); `TestOpenAPIOperationContracts` additionally pins operationId + a schema'd 2xx on every operation and 401/403/429 on every /v1 operation. `docs/api.md` walks auth bootstrap → submit/inspect/list → definition registry → cancel/park/unpark → DLQ requeue → error envelope + rate-limit headers, runnable against `make up-app`.)*
**Done when:**
- [x] Spec validates; every implemented route present with request/response schemas
- [x] Route-coverage drift check wired into CI
- [x] `docs/api.md` renders usage examples (curl) for the main flows

---

## Milestone 7 — Observability

**Goal.** First-class observability, as its own milestone: Prometheus metrics for queue depth/throughput/latency/errors, OpenTelemetry tracing that follows a single run across multiple worker processes (context propagated through queue envelopes), per-step log capture with an API, and provisioned Grafana dashboards + alert rules in the compose stack.

**Role.** Everything after this milestone is built with its instruments on. Queue-depth metrics become KEDA autoscaling signals in M20; dashboards and histograms are the measurement substrate for load testing in M19.

**Architecture docs:** ADR-008 (observability conventions: metric naming, cardinality budget, trace propagation, log field dictionary).

**Exit criteria:** compose `--profile obs` boots Prometheus/Grafana/Jaeger; a fan-out run shows as one trace spanning two worker processes; dashboards render live engine metrics; per-step logs retrievable via API.

#### 7.1 — ADR-008 & telemetry wiring ✅
**Depends on:** 0.5, 4.6
**ADR-008:** metric naming scheme (`engine_*`), label cardinality budget (**never** `run_id`/`step_id` as metric labels; step *type* and outcome are fine), log field dictionary, trace propagation design. Wire Prometheus registries + `/metrics` on API and worker admin ports; OTel SDK (OTLP exporter, `service.name`/`service.version` resources). Compose profile `obs`: Prometheus, Grafana, Jaeger (or OTel collector → Jaeger), scrape configs. *(As built: ADR-008 pins `engine_<subsystem>_<name>[_<unit>]` on instance-scoped registries with an enumerated label-allowlist table, the log field dictionary + `span_id`/`service`, and the 7.3 trace design — trace context persisted on the run row so reconciler/requeue/unpark dispatches restore linkage, attempt spans linked not parented across retries. `config.ObsConfig` (`AGENTLOOM_OBS_*`) defaults everything off; `internal/obs/metrics` serves `/metrics` + `/healthz` on a dedicated admin listener (never the bearer-authed public port) with `engine_build_info` as the proof-of-life gauge; `internal/obs/trace` installs no-op or OTLP/gRPC SDK providers. Compose `obs` profile = Prometheus (worker replicas discovered via `dns_sd_configs` type A), Grafana with a provisioned datasource, Jaeger all-in-one accepting OTLP directly; `make up-obs` boots app+obs with export on. API requests get otelhttp server spans named `HTTP <method>` until 7.3 refines naming.)*
**Done when:**
- [x] `--profile obs` stack scrapes both services; Jaeger receives spans
- [x] ADR reviewed; conventions referenced by later tickets' ACs
- [x] Telemetry cleanly disabled via config (no-op providers) for tests

#### 7.2 — Core engine metrics ✅
**Depends on:** 7.1, 3.2
Instrument: queue ready depth, PEL size, delayed count, outbox backlog + drain lag, claims/s, step duration histograms (by step type + outcome), retries, reclaims, fencing rejections, DLQ counter, scheduling latency (ready→running) histogram, run completion latency, active workers (heartbeat gauge), API request histograms + 429 counters (hooks from 6.4). *(As built: every instrument declared in `internal/obs/metrics/instruments.go` — `WorkerMetrics`/`APIMetrics` structs on the 7.1 instance registries, satisfying narrow per-package seams structurally (`queue.ConsumerMetrics` on `ConsumerConfig`, `engine.Metrics` via `WithMetrics`/`WithDispatcherMetrics`/`WithReconcilerMetrics`, `api.RequestMetrics` via the new variadic `api.Option`, plus the 6.4 `RateLimitMetrics` seam finally wired) with no-op defaults so every test layer keeps recording off; scheduling latency ready→running measured exactly via `store.ClaimStepWithOrigin` — one extra PK read under the run lock returns the pre-claim status + `updated_at`, retrying-origin claims deliberately skipped (backoff ≠ scheduling); run completion latency = terminal − `started_at` (both injected clocks — `created_at` is a DB default and would mix clocks); depth gauges (ready depth via XINFO GROUPS lag with an XLEN−PEL fallback when Redis nulls it, stream length, PEL, delayed, outbox backlog + oldest age, active workers = consumers idle ≤ 3× read block) sampled by a cmd/worker loop every `AGENTLOOM_WORKER_METRICS_SAMPLE_INTERVAL` (default 10s) only when the admin listener is on; drain lag observed per XADDed row post-commit; API requests recorded in `requestLog` off the chi route pattern with 404/405 collapsed to `route="unmatched"` and client verbs clamped; ADR-008 gained label keys `result`/`bucket`/`decision` + the full metric inventory table, enforced by `TestInstrumentConformance` (names, suffixes, label allowlist); scheduling latency proven by an integration test with a scripted 7s delay on fully injected clocks (exact-equality assert); `make smoke-metrics` boots app+obs, drives fanouts + a retry + a retries-exhausted dead-letter, and asserts all listed metrics in Prometheus — verified green live.)*
**Done when:**
- [x] All listed metrics exposed and visible in Prometheus under load (smoke script)
- [x] Cardinality audit: label sets bounded and enumerated in ADR-008
- [x] Scheduling-latency histogram proven accurate against a scripted delay

#### 7.3 — Distributed tracing across the queue ✅
**Depends on:** 7.1
Inject W3C trace context into task envelopes at enqueue (root span at submission); workers extract and start attempt spans linked to the run trace; child spans for claim CAS, executor, completion tx, ACK; retries/reclaims linked via span links; fan-in joins use links from all parents. The demo artifact: one trace showing a run crossing two worker processes. *(As built: migration 0010 gives trace context three durable homes — `runs.trace_parent/trace_state` (the root, captured from the POST /v1/runs otelhttp span, which requestLog now renames to `<METHOD> <route pattern>` post-routing), `task_outbox.trace_parent/trace_state` (the enqueuing span, stamped only by live-span writers: completion fan-out and instantiation; the drain read COALESCEs NULL rows to the run root, so reconciler/unpark/dlq_requeue needed zero changes), and `run_steps.trace_span` (the attempt span, stamped by the claim CAS — its overwritten previous value, surfaced by the existing pre-claim read, is the uniform link source for retries AND takeovers, with no envelope link fields and nothing handed over by dead workers). The queue consumer starts `step.attempt` around each delivery (it owns the ACK child span) with `step.claim`/`step.executor`/`step.completion`/`queue.ack` children; joins add links to all firing parents via the new `ListFiringParentTraceSpans`; the delayed retry envelope carries the run root (constant per run — ZADD dedup preserved). Tracer seams `ConsumerConfig.TracerProvider` + `engine.WithTracerProvider` default to the global no-op; hermetic span-topology tests run on an in-memory recorder. `trace_id`/`span_id` stamped into log context per delivery and per request. Verified live: `make smoke-trace` — one Jaeger trace, both worker replicas, FOLLOWS_FROM retry link.)*
**Done when:**
- [x] Jaeger shows a single run trace spanning ≥2 worker processes, including a retry link
- [x] `trace_id` present in structured logs for correlation
- [x] Envelope schema change is versioned and backward-tolerant

#### 7.4 — Per-step log capture & API ✅
**Depends on:** 7.1, 6.5
`StepContext` logger tees to a `step_logs` store (per-attempt, level-filtered, size-capped ring — oldest dropped with a truncation marker). `GET /v1/runs/{id}/steps/{sid}/logs?attempt=` with pagination. This feeds the dashboard's per-step log view (M18). *(As built: migration 0011's `step_logs` table keyed `(run_id, step_id, attempt, seq)` — one ring per attempt with exactly one writer, because retries/reclaims/takeovers all mint a new attempt at claim, so `seq` is an in-process atomic counter and a zombie's late flush lands harmlessly under its old attempt. Capture is `internal/exec/steplog`: `Sink.LoggerFor` fans the engine's per-attempt logger out to the terminal handler and a capture handler (level-filtered before seq allocation — default info via `AGENTLOOM_WORKER_STEPLOG_LEVEL` — attrs marshaled to `fields` JSONB with slog group semantics, errors stringified, message/fields each truncated at `MAX_LINE_BYTES`), enqueueing non-blocking O(1) into a bounded drop-oldest ring buffer; an async flusher batches per-attempt COPY + ring-cap `Trim` transactions (cap default 1000 via `_CAP`), dropping failed batches — forward progress over completeness. The truncation marker is derived, never stored: every captured line consumes a seq, so dropped = max(seq) − stored, one aggregate read. Engine seam `WithStepLogs` (nil default keeps every test layer capture-free); the executor's logger — and only it — is teed, stamped with the attempt span's trace_id; cmd/worker runs the flusher on loopCtx with a final bounded flush after the consumer drains. API: `GET /v1/runs/{id}/steps/{sid}/logs` (read scope/class) with ascending-seq keyset cursor (default 200, max 1000), `attempt` defaulting to the step's latest (unattempted = empty page, not 404), min-`level` filter, `truncated`/`dropped_lines`; spec + route tables extended, lint 100/100. New counters `engine_steplog_{captured,dropped,flush_failures}_total` under new subsystem `steplog` (ADR-008 amended). Headline tests: 10k-line flood against a small buffer completes promptly with stored ≤ cap and the derived marker; per-attempt rings across a retry; trace_id/fields/level-filter end-to-end on a span recorder; API pagination/filter/404 matrix. Verified live on compose.)*
**Done when:**
- [x] Cap enforced (write 10k lines → stored ≤ cap with truncation marker)
- [x] Logs carry `trace_id` and attempt; endpoint paginates correctly
- [x] Executor log flooding cannot stall execution (async buffered writer, drop-oldest)

#### 7.5 — Grafana dashboards & alert rules ✅
**Depends on:** 7.2, 7.3
Provisioned-as-code dashboards: **Engine** (throughput, queue/PEL/delayed depth, scheduling latency, step latency p50/p95/p99, error/DLQ/reclaim rates) and **API** (RPS, latency, 429s, in-flight). Example alert rules (queue depth growing 10m, DLQ rate spike, reclaim spike, outbox lag). Screenshots into docs. *(As built: two hand-authored dashboards in `deploy/observability/grafana/dashboards/` (`agentloom-engine`, `agentloom-api`) provisioned by a file provider with UI edits disabled, panels referencing the datasource's new stable `uid: prometheus`; fleet-wide gauges aggregated `max()` since every worker replica samples the same values. One production change: `engine_api_requests_in_flight`, an unlabeled gauge bracketing every request via the extended `api.RequestMetrics` seam (`RequestStarted`/`RequestFinished`), integration-asserted to balance back to zero. Alert rules in `deploy/observability/prometheus-rules.yml` — `QueueDepthGrowing` (elevated AND above its 10m-ago value, 5m hold), `DeadLetterRateSpike`, `ReclaimRateSpike`, `OutboxDispatchLag` — with dev-scale thresholds by design, loaded via `rule_files`, and promtool-unit-tested (`prometheus-rules.test.yml`: each alert fired on a synthetic failure-shaped series plus a deep-but-draining negative case) through the new `make obs-lint`, which runs promtool from the exact compose Prometheus image tag; wired into the CI lint job. Anti-drift audit `TestDashboardsAndRulesReferenceRegisteredMetrics`: every `engine_*` name referenced anywhere in dashboards/rules must be a registered instrument. "Under the chaos suite" honored by `make smoke-dashboards` (`scripts/dashboard-smoke.sh`) — the sustained chaos suite's host subprocesses run on isolated queue keys compose Prometheus cannot scrape, so the script recreates its signal shape against the scrapable fleet: SIGKILL of the lease holder mid-step (reclaim + takeover) **first** (a restarted worker resets its counter registry, so vec-counter series only the victim recorded would go stale fleet-wide — learned the hard way), then a 12-run retries-exhausted burst paced across scrape intervals (a vec counter absorbed between two scrapes is born at its final value and rates as zero — the second hard-won lesson), fan-outs, a transient retry, and a 429 storm, then asserts every dashboard panel query non-empty (3 quiet-when-healthy queries allowlisted: reconcile heals, fail-open, 5xx), all 4 rules loaded, and DeadLetterRateSpike observed `firing` — the documented test-fire. `metrics-smoke.sh` gained the deferred `engine_steplog_*` + in-flight EXIST checks. `docs/observability.md` + screenshots; ADR-008 amended; closes M7.)*
**Done when:**
- [x] Dashboards auto-provision in compose; panels render non-empty under the chaos suite
- [x] Alert rules load in Prometheus; one rule test-fired and documented
- [x] `docs/observability.md` explains the dashboards and key signals

---

# Phase B — AI-native layer

---

## Milestone 8 — Plugin SPI & LLM/tool execution

**Goal.** The engine meets AI. Formalize the plugin SPI (executors, tools, retrievers, model providers, validators — self-describing via JSON Schema), input templating for data flow between steps, Anthropic + OpenAI providers behind one interface, a deterministic mock provider (the workhorse for tests and load), the LLM and tool-call step executors, and a reference retrieval backend.

**Role.** Differentiator #7 (pluggability) lands here as architecture, not afterthought: every later AI feature (validators, agents, retrievers) registers through this SPI, and plugin config schemas will drive the UI's config panels (M17.4) — one contract, three consumers.

**Architecture docs:** ADR-009 (plugin SPI: registration, config schemas, capability flags, versioning, in-process isolation stance).

**Exit criteria:** a real workflow (retrieve → LLM → tool) runs end-to-end on compose with the mock provider in CI and live providers behind an env flag; `GET /v1/plugins` serves self-describing schemas.

#### 8.1 — ADR-009 & plugin registry refactor ✅
**Depends on:** 4.1, 6.6
**ADR-009:** plugin kinds (executor, tool, retriever, model provider, validator), registration API, per-plugin JSON Schema for config (generated, served via API), capability flags (`side_effectful`, `cacheable`, `cost_bearing`), plugin version strings (feed cache keys in M9), in-process compilation model (out-of-process/WASM explicitly deferred to backlog). Refactor M4's registry to the SPI; `GET /v1/plugins` lists plugins + schemas; `ctl plugins list`.
**Done when:**
- [x] Existing executors migrated; registry rejects duplicate/invalid registrations
- [x] `GET /v1/plugins` returns machine-usable schemas (consumed later by UI forms)
- [x] Capability flags stored and queryable per plugin

#### 8.2 — Step input templating & data flow ✅
**Depends on:** 8.1, 1.5
Render step config/inputs with references to upstream outputs and run params — `${{ steps.<id>.output.<path> }}`, `${{ run.params.<key> }}` — via Go `text/template` with a restricted FuncMap (get, default, toJson, truncate; no arbitrary code) + JSON-path resolution. Static lint at definition-validation time (referenced step exists and is upstream); strict missing-reference errors at runtime. *(As built: templating lives in `internal/dag` (`template.go`, the CEL precedent) — `ParseConfigTemplates` walks a config's JSON string values, rewrites each `${{ ... }}` action's bare references into strict lookups, and compiles per-string text/templates with `${{`/`}}` delimiters, so plain `{{ }}` is inert; expressions are a single pipeline (control structures, variables, and every text/template builtin rejected at parse — the allowlist get/default/toJson/truncate is enforced at rewrite time, since builtins cannot be removed from the FuncMap), string literals may be single-quoted to spare JSON `\"` noise, and rendering happens exactly once so template text arriving through outputs/params is inert data. Bare refs are strict (missing → typed `*MissingRefError`); `get 'path' | default x` is the lenient opt-out, and quoted ref-shaped `get` paths still feed the prefetch set. Whole-expression strings are type-preserving (objects/arrays/numbers splice as JSON via a capture func); mixed strings interpolate (scalars naturally, composites as compact JSON). Static lint joins Validate's one-pass report under five new codes (`template_invalid`, `template_ref_invalid`, `template_ref_unknown_step`, `template_ref_not_upstream` — strict normal-edge ancestry via `Graph.Ancestors`, loop-edge-only reachability and self-refs rejected — and `template_ref_unknown_param`), with one carve-out: a templated sleep `duration` defers its parseability check to the executor. The engine renders in `execute` just before the executor (`renderConfig`, span `step.render`): template-free configs pass through with zero reads, otherwise one batched `ListRunStepsByIDs` read + the run row supply outputs (succeeded steps only) and params; deterministic failures land a permanent failure completion (ADR-006 row 15) with referenced-step statuses in the diagnostic, transport failures redeliver. `StepContext.Config` now carries the rendered config; `Input` stays nil, reserved. New canonical fixture `echo_pipeline.json` (corpus-pinned, construct-pinned) executes end-to-end in the engine integration suite: params → nested paths → multi-hop whole-object flow, plus missing-ref → dead-lettered permanent and a templated sleep duration. ADR-003 gained the templating contract section; ADR-006 the row-15 amendment.)*
**Done when:**
- [x] Template suite: nested paths, defaults, missing-ref → typed error, injection attempts inert
- [x] Validation flags references to non-existent or non-upstream steps
- [x] `echo` pipeline fixture proves multi-hop data flow end-to-end

#### 8.3 — Model provider interface & Anthropic provider ✅
**Depends on:** 8.1
`internal/llm`: unified `ChatRequest`/`ChatResponse` (messages, system, tool definitions, `max_tokens`, `temperature`), usage extraction (input/output tokens), provider error taxonomy mapped onto M5 retry classes (429/overloaded → transient with `retry_after`; invalid request → permanent). Anthropic implementation (Messages API); API keys via config/env only. Recorded-fixture tests; optional live smoke test behind `LIVE_LLM_TESTS=1`. Non-streaming v1 (streaming → backlog). *(As built: `internal/llm` is a leaf package like ratelimit — imports `dag` for the ADR-006 class vocabulary and `plugin` for the manifest, never exec/engine; no SDK dependency (hand-rolled `net/http` client, injectable base URL + HTTP client); providers make exactly one call per `Chat` (no internal retries, no logging) and self-describe via `Manifest()` (kind `model_provider`, name `anthropic`, cacheable + cost-bearing), with the typed `llm.Registry` facade and `GET /v1/plugins` listing deferred to 8.4's routing ticket. Failures surface as `*llm.Error` (class, status, provider code, `retry_after` from delta-seconds Retry-After only — the HTTP-date form would need a wall clock — and request id); context cancellation/deadline passes through unclassified so the engine keeps the timeout/cancelled judgment (ADR-006 rows 3/8); a 200 without usage is a malformed response, not a lenient zero; secret hygiene is structural (the error type has no field that can hold request headers or the payload) and pinned by an every-error-path assertion test with a positive control. `config.LLMConfig` (`AGENTLOOM_ANTHROPIC_API_KEY`) parses the key with empty = provider unconfigured — not a load error, since a worker running no llm steps must boot keyless; nothing consumes it until 8.6. Golden request fixtures pin the outgoing wire shape both ways. ADR-006 gained the "Provider error taxonomy" as-built section.)*
**Done when:**
- [x] Fixture-based tests cover success, rate-limit, overload, invalid-request mappings
- [x] Usage (tokens in/out) extracted and returned on every success
- [x] No API key material ever appears in logs/errors (assertion test)

#### 8.4 — OpenAI provider & model routing ✅
**Depends on:** 8.3
OpenAI implementation of the same interface; provider registry routes by model ID prefix/explicit provider field; identical error-taxonomy mapping and usage extraction; same fixture testing pattern. *(As built: `internal/llm/openai.go` mirrors the Anthropic provider — hand-rolled `net/http`, no SDK, one call per Chat, injectable base URL/client, `NewOpenAI` rejecting a missing key at construction — targeting the stable **Chat Completions** API (`POST /v1/chat/completions`); the Responses API is backlogged. The unified→wire mapping handles the two structural mismatches with the Anthropic content model: the system prompt becomes a leading `system` message, and a user turn's `tool_result` blocks fan out into standalone `tool`-role messages. `MaxTokens`→`max_completion_tokens` (the deprecated `max_tokens` is rejected by o-series models), tool_use→`tool_calls` with JSON-string arguments, auth via `Authorization: Bearer`. The response maps `choices[0]` (zero choices → malformed-200), surfaces a `refusal` as text, decodes `tool_calls`, and **normalizes `finish_reason` onto the Anthropic stop-reason vocabulary** (`stop`→`end_turn`, `length`→`max_tokens`, `tool_calls`→`tool_use`; unknown reasons pass through verbatim) so 8.6 branches on one vocabulary; usage is mandatory on success. Error mapping reuses 8.3's `classifyStatus`/`parseRetryAfter` unchanged (the taxonomy is provider-agnostic) plus a new clock-free `retry-after-ms` header (preferred over whole-second Retry-After); `code` prefers OpenAI's specific `code` over the coarse `type`; request id from `x-request-id`; context errors pass through unclassified. `internal/llm/registry.go` — the typed `llm.Registry` facade over `plugin.Registry` (kind model_provider) plus `Resolve(explicitProvider, model)`: explicit-provider-wins, then `"<provider>/<model>"` namespace form (reserved for 8.5's `mock/...`, prefix stripped), then a longest-vendor-prefix table; two deliberately distinct typed errors — `*UnknownModelError` (no rule matched) and `*ProviderUnavailableError` (routed provider not configured, i.e. key absent). `llm.NewRegistryFromKeys(ProviderKeys{...})` is the one shared constructor both deployables call — each provider built iff its key is present, empty registry valid, keeping llm a config-free leaf and satisfying independent-configurability. `config.LLMConfig` gained `OpenAIAPIKey`/`AGENTLOOM_OPENAI_API_KEY`; `cmd/api` folds the configured providers' manifests into `GET /v1/plugins`; compose + `.env.example` pass both keys through to api and worker. Recorded-fixture suite mirrors 8.3 one-for-one (golden wire requests both ways, taxonomy matrix incl. retry-after-ms precedence, finish-reason normalization incl. unknown-passthrough, malformed-200/transport/context/validation paths, secret-hygiene walk with positive controls, manifest conformance), plus routing-table + config-matrix tests and an OpenAI live smoke behind `LIVE_LLM_TESTS=1`. ADR-009 gained the model-provider registry/routing as-built section; ADR-006 the OpenAI taxonomy note.)*
**Done when:**
- [x] Routing table tested (model → provider; unknown model → typed error)
- [x] Fixture tests mirror 8.3's coverage for OpenAI response/error shapes
- [x] Providers configurable independently (either can be absent without breaking startup)

#### 8.5 — Mock/simulation provider ✅
**Depends on:** 8.3
Deterministic provider for tests and load: scripted responses (match on prompt substring/regex or call sequence), configurable latency distributions, token counts, failure/429 injection rates. Registered like any provider (`model: "mock/..."`). This is the load-test workhorse (M19) — cheap, deterministic, offline. *(As built: `internal/llm/mock.go` — a third `Provider` behind the 8.3 interface, registered under name `mock` and routed purely through the 8.4 registry's namespace form (`model: "mock/<model>"`, prefix stripped), so it needed zero changes to `Resolve`. `NewMock(MockConfig)` rejects a malformed script at construction (bad regex, out-of-range rates, `min > max` / negative latency, a rule with no responses) — the analogue of the HTTP providers' boot-time key check. Scripting: ordered `MockRule`s match on prompt **substring**, **regex**, or the 1-based global **call ordinal** (`OnCall`), first match wins; each rule's `Respond` sequence returns its entries in order with the last **sticky**. A `MockOutcome` is a success (single `Text`, or full `Blocks` for tool_use scripting, with explicit or estimated `Usage`) or, when `Status != 0`, a scripted `*llm.Error` classified through the **same** provider-agnostic `classifyStatus`; `MockInjection` adds global per-call 429/500 rates (429 before 500); a `Hang` outcome blocks on ctx and returns the context error **unclassified**, exactly like the real providers (ADR-006 rows 3/8). With no rules the mock deterministically **echoes** the last user text (`[mock] …`) with estimated usage — the zero-config behavior chained llm steps need to flow real data through 8.2 templating. Determinism: one seeded PCG PRNG (`math/rand/v2`) under a mutex; the whole draw sequence (call counter, injection lottery, latency sample, per-rule cursor) advances under the lock, so a given seed + sequential call order → a byte-identical transcript; the latency wait itself is outside the lock and time is injectable via a `Sleep` seam (the only clock the mock touches). Offline is proven structurally (the struct has no HTTP client or transport field) and dynamically (the full matrix runs with `http.DefaultTransport` swapped for a tripwire that fails on any use). Wiring: `ProviderKeys` gains `Mock *MockConfig` (nil = absent, never a boot error — no key, it's scripted not authenticated), built by the one shared `NewRegistryFromKeys`; `config.LLMConfig.MockEnabled` / `AGENTLOOM_LLM_MOCK_ENABLED` toggles it (binary default off; compose + `.env.example` default **on** so the M8 exit-criterion workflow runs on the mock in CI without any key); `cmd/api` folds its manifest into `GET /v1/plugins`. The reused e2e fixture is the new canonical `examples/definitions/mock_pipeline.json` — a converted linear M4 chain of two `llm` steps passing data via `${{ steps.draft.output.text }}` — executed end-to-end against the mock in the engine integration suite through a minimal in-test `llm` executor standing in for 8.6's production one. Manifest is (`model_provider`, `mock`, `1.0.0`, cacheable-only) — the first provider that is cacheable but **not** cost-bearing (a free provider), pinned by the API catalog test. ADR-009 gained the mock as-built section; ADR-006 a note that injected/scripted failures classify through the shared taxonomy.)*
**Done when:**
- [x] Scripting covers: fixed response, sequence, conditional-on-prompt, injected errors/latency
- [x] Deterministic under seed; zero external calls (asserted)
- [x] Reused to convert one M4 e2e fixture into an `llm`-step fixture

#### 8.6 — LLM step executor ✅
**Depends on:** 8.5, 8.2, 5.5
The `llm` step: render messages via templating, call the provider through the interface, persist structured output (text and/or tool-call payload), record usage on the attempt row (feeds M10 cost), honor `max_tokens`/`temperature` config, map provider errors to retry classes. Registered with full config schema. *(As built: `internal/exec/llmexec.go` — `exec.LLMExecutor` (version `1.0.0`, cacheable + cost-bearing) replaces the dev stub in place, decoding the already-8.2-rendered `LLMConfig`, routing the model through `llm.Registry.Resolve` (routing failures — unknown model / unconfigured provider — are deterministic ⇒ permanent), building a unified `ChatRequest` (prompt → one user message; `messages[]` mapped onto the two conversational roles; `max_tokens` default 1024 when absent; `temperature` passed through), making exactly one `Chat` call (no retries — the M5 engine owns retry), and persisting `{model, stop_reason, text, tool_calls?, usage}` — `text` always present so `${{ …output.text }}` never misses. Provider `*llm.Error`s wrap into `exec.ClassifiedError` honoring the provider's ADR-006 class (429 → transient, 4xx → permanent); context errors pass through unclassified (engine judges timeout/cancelled). Usage rides `exec.Output.Usage` onto the new nullable `step_attempts.usage` JSONB (migration 0012) inside the success completion tx, surfaced as `attempts[].usage` in `GET /v1/runs/{id}`. Submit-time validation now also checks message role ∈ {user, assistant} + non-empty content. `Builtins`/`CoreBuiltins` take a `*llm.Registry`; `cmd/worker` builds it from `cfg.LLM` (Anthropic/OpenAI/Mock), `cmd/api` passes nil (never executes). The canonical `fanout.json` moved its two llm models to `mock/sim-1` so it stays fully offline-runnable on compose/CI/smoke (its tool/retrieve steps are already stubs). 8.5's in-test executor shim is deleted; its e2e now drives the production executor.)*
**Done when:**
- [x] E2E: two chained `llm` steps (mock) pass data via templating on compose
- [x] Usage persisted per attempt; visible in run status API
- [x] Provider transient errors retry per M5 policy (integration test with injected 429)

#### 8.7 — Tool SPI & built-in tools ✅
**Depends on:** 8.1, 5.5
Tool interface (name, JSON-Schema args, `Invoke(ctx, args) (result, error)`, capability flags) + `tool` step executor (args via templating, schema-validated before invoke). Built-ins: `http_request` (method/URL/headers/body, **URL allowlist**, timeout, automatic `Idempotency-Key` header from 5.5 for non-GET), `json_transform` (gojq expression). *(As built: new leaf package `internal/tools` (imports `plugin`+`dag`+stdlib+gojq+jsonschema/v6, never exec/engine) — `Tool` = `Manifest() plugin.Manifest` + `Invoke(ctx, Invocation) (json.RawMessage, error)` (the literal `(ctx, args)` grew to an `Invocation` struct carrying the 5.5 idempotency key + logger, the `StepContext` precedent); every tool declares its args JSON Schema in the manifest, generated from the tool's Go arg struct with `dag.StepConfigSchema`'s reflector settings. `tools.Registry` is the typed facade over `plugin.Registry` (kind tool) that additionally **compiles each args schema once at registration** (santhosh-tekuri/jsonschema v6; uncompilable → boot error) and exposes `ValidateArgs` — the framework gate the executor calls **before** `Invoke`, so "bad args → permanent, no call" is a framework guarantee every tool inherits (violation → typed `*ArgsValidationError`, unknown tool → `*UnknownToolError`). `exec.ToolExecutor` replaces `StubToolExecutor` in place (version `0.1.0-stub` → `1.0.0`, flags unchanged): decode rendered `ToolConfig` → lookup → validate → one `Invoke` → persist the tool result **verbatim** (no envelope; downstream reads `${{ steps.x.output.<field> }}`); `*tools.Error` class honored via `ClassifiedError`, ctx errors unwrapped. `http_request` (side_effectful): one call guarded by a host **allowlist** (empty = deny-all safe default; blocked → typed `*HostNotAllowedError` permanent, provably pre-connection; `CheckRedirect` re-validates every redirect hop), timeout + response-size cap, automatic `Idempotency-Key` on non-GET from 5.5 (wins over any user header); 429/5xx→transient, other non-2xx→permanent, self-imposed timeout→transient vs engine-ctx passthrough. `json_transform` (cacheable — first pure built-in tool): gojq under `ctx`, one-value/many→array. Wiring: `tools.NewBuiltins(HTTPOptions)` the one shared constructor; `config.ToolsConfig` (`AGENTLOOM_TOOLS_HTTP_ALLOWLIST`/`_TIMEOUT`/`_MAX_RESPONSE_BYTES`); `cmd/worker` builds+wires it, `cmd/api` folds tool manifests into `GET /v1/plugins`; `exec.Builtins`/`CoreBuiltins` gained a `*tools.Registry` param (8.6 llm precedent). Canonical `fanout.json`'s executed tool step moved `http_request`→offline `json_transform` (empty compose allowlist would block http) so the M8 exit workflow stays offline; corpus fixtures keep realistic `http_request`. Tests: full `internal/tools` offline suites (registry identity/dup/schema-compile/args-validation matrix; http_request success/JSON-body/idempotency-key-on-POST-absent-on-GET/status-classes/size-cap/timeout/ctx-passthrough/allowlist-block-with-tripwire-zero-requests/redirect-revalidation/secret-hygiene; json_transform paths/multi-emit/errors/ctx), `exec` toolexec suite (unknown/invalid-args-never-invokes/class+ctx passthrough/verbatim/key propagation), headline engine integration (idempotency-key identical across a 500→200 retry with history `[transient, succeeded]`; allowlist-blocked step dead-letters source `permanent`), plugins-listing integration for kind tool with args schemas, config suite. ADR-009 gained the tool SPI as-built section + updated flag table; ADR-006 the tool error taxonomy.)*
**Done when:**
- [x] Args validated against tool schema pre-invocation (bad args → permanent failure, no call)
- [x] `http_request` proves idempotency-key reuse across retries against httptest server
- [x] Allowlist blocks non-approved hosts (SSRF guard) with a typed error

#### 8.8 — Retrieval SPI & reference backend ✅
**Depends on:** 8.1
`Retriever` interface (`Ingest(docs)`, `Query(q, k) []ScoredDoc`) + `retrieve` step executor writing results to step output for downstream templating/context. Reference implementation: Postgres full-text (tsvector) — zero new infra; pgvector/external vector stores documented as follow-on plugins (backlog). RAG-lite fixture: retrieve → llm (mock) answer with citations. *(As built: new leaf package `internal/retrieval` (imports `plugin`+`dag`+stdlib, never `exec`/engine/`store`) — `Retriever` = `Manifest()` + `Ingest(ctx, []Doc)` + `Query(ctx, q, k) []ScoredDoc`, `Doc` `{id, content, metadata}` (id the idempotent upsert key), `ScoredDoc` embedding `Doc` + `score`; the typed `retrieval.Registry` facade over `plugin.Registry` (kind retriever) — plain register/get/manifests, **no** per-retriever config schema by decision (the retrieve step's config shape is uniform, its schema lives on the executor); `*retrieval.Error{Class}` + `*UnknownRetrieverError` mirroring `tools`, structural secret hygiene, ctx errors never wrapped. The reference backend is a **subpackage** `internal/retrieval/pgfts` (the only importer of `store`, keeping the SPI a leaf and giving `docs/plugins.md` its worked example): Postgres full-text over migration 0013's `retrieval_docs` (`id`/`content`/`metadata`/timestamps + a **functional GIN index** on `to_tsvector('english', content)` so the sqlc row type stays scalar), `Query` ranking by `ts_rank` desc via `websearch_to_tsquery` (ANDs terms, never errors on arbitrary input ⇒ empty/no-match returns an empty slice not a failure), `Upsert` idempotent re-ingest, `cacheable`-only manifest; store grew `RetrievalDocRepo` (Upsert/Query/Count) on `Querier`/`repos` + `queries/retrieval.sql`. `exec.RetrieveExecutor` replaces `StubRetrieveExecutor` in place (`0.1.0-stub` → `1.0.0`, the last dev stub gone; flag unchanged cacheable): decodes the 8.2-rendered `RetrieveConfig`, `top_k` default 5 / cap 100 / negative permanent, unknown retriever & empty rendered query permanent, one `Query`, output `{retriever, query, top_k, results}` with `results` always an array (never null so `${{ …output.results }}` never misses), `*retrieval.Error` class honored via `ClassifiedError`, ctx errors unwrapped; dag validation gained a `top_k >= 0` check (`config_field_invalid`). `exec.Builtins`/`CoreBuiltins` grew a third `*retrieval.Registry` param (8.6/8.7 precedent, nil valid → permanent at lookup); `cmd/worker` always builds `retrieval.NewRegistry(pgfts.New(st))` (no key/toggle — needs only the shared Postgres), `cmd/api` folds its manifest into `GET /v1/plugins` (OpenAPI already enumerated the retriever kind and made `config_schema` optional — no spec change). New canonical `examples/definitions/rag_lite.json` (corpus-pinned) — a `pg_fulltext` retrieve step feeding ranked results into a mock `llm` step via `${{ steps.search.output.results }}`, executed end-to-end against a seeded corpus in the engine integration suite (answer text cites the seeded doc ids); `fanout.json`'s retrieve step is now the real executor against an empty corpus (offline-green). Tests: retrieval registry matrix, `exec` retrieveexec suite (defaults/cap/negative/empty-query/nil-registry/unknown/class+ctx passthrough/output shape/empty-array), pgfts integration (ingest+rank, top-k bound, no-match/empty, re-ingest upsert, metadata round-trip, empty-field rejection), engine RAG-lite e2e + unknown-retriever dead-letter, api plugins-listing for kind retriever; migrate round-trip bumped to 13. `docs/plugins.md` is the new SPI guide with the "writing a retriever plugin" walkthrough; ADR-009 gained the retrieval as-built section + flag table (retrieve → 1.0.0, `pg_fulltext` row, last "stub" gone), ADR-006 the retrieval error taxonomy.)*
**Done when:**
- [x] Reference retriever ingests and ranks against seeded corpus (integration test)
- [x] `retrieve` step output shape documented and consumed by an `llm` step in the fixture
- [x] SPI docs include a "writing a retriever plugin" walkthrough stub

---

## Milestone 9 — Distributed rate limiting & response caching

**Goal.** Fleet-wide governance of shared external resources: Redis token buckets (requests/min *and* tokens/min) enforced across all worker processes with delayed-requeue backpressure (throttled steps don't burn attempts or block workers), plus a response cache with deliberate key design for non-deterministic outputs and an explicit invalidation strategy.

**Role.** Two mandated distributed-systems features, implemented as executor middleware so every current and future step type inherits them. Cache-hit accounting deliberately records counterfactual spend for M10's "saved" metric.

**Architecture docs:** ADR-010 (rate limiting & backpressure), ADR-011 (cache key design & invalidation).

**Exit criteria:** N workers against a limit of R req/min observe ≤R calls at the provider (mock-verified); throttled steps requeue with delay and complete; identical deterministic steps hit cache; invalidation busts by prefix.

#### 9.1 — ADR-010 & resource limit configuration ✅
**Depends on:** 6.3, 8.3
**ADR-010:** named resources (`anthropic:<model>`, `openai:<model>`, custom tool resources), dual buckets per resource (requests + estimated tokens), backpressure semantics — deny → attempt outcome `throttled` (not a failure), delayed requeue at `retry_after` + jitter, never busy-wait a worker slot; fairness note (per-run throttle cap to prevent one run starving the fleet — documented, implemented if load tests demand). Config loading for resource limits. *(As built: `docs/adr/010-rate-limiting-and-backpressure.md` (Accepted) pins named resources keyed by the **resolved** provider (`mock/sim-1` → resource `mock:sim-1`) plus `tool:<name>`; resolution is exact → provider wildcard `<provider>:*` → **not-found = unlimited** (the unknown-resource policy — limits are protective opt-in, fail-closed would brick the fleet on a new model name, mirroring 6.4's fail-open); dual per-resource buckets (requests cost 1, tokens cost = pre-call estimate) acquired **all-or-nothing in one atomic two-key Lua script** — 6.3's explicitly-deferred script, now mandatory because M9 denials are steady-state and 6.4's no-refund skew would leak a request token per token-denial; the **`throttled` backpressure contract** — a denial is a *second administrative outcome* alongside `lost`, outside the ADR-006 taxonomy, never budget-counted (`CountCountedFailures` already counts `transient`/`timeout` only → zero query change), reusing 5.2's `running → retrying` CAS + `next_attempt_at` claim guard + overdue-retrying reconciler scan **with no new store primitive or reconciler duty**, re-dispatched through the delayed ZSET under new reason `throttle` (added by 9.2); **requeue math** `clamp(retry_after, floor 500ms, cap 5m) + U[0, 20% × clamped]` — additive-partial jitter deliberately, since `retry_after` is a real refill deadline (full jitter would wake siblings before tokens exist); **wait-vs-never** — `ErrCostExceedsCapacity` / `RetryAfterNever` denials perm-fail to the DLQ (the 6.3 contract edges consumed); the **fairness stance** documented (global buckets can let one fan-out starve a run; remedy = per-run throttle cap → park with a new `resource_starved` reason, deferred to M19 load tests since the hard provider limit is always respected); plus the **9.2 executor hook** (`ResourceClaimer`) and the estimator/reconciliation forward-pointers designed in-ADR. The config half shipped: leaf package `internal/limits` (stdlib only — imports neither `ratelimit` nor engine) with `Rate{PerMinute, Burst}` (+ `RefillPerSec`/`Capacity` mapping helpers for 9.2), `Resource`, `Set` with `Lookup`/`Names`/`Len`, `Parse` (strict `DisallowUnknownFields` decode + trailing-content check + all-errors-joined validation: name shape/whitespace/trailing-`:*`-only-wildcard/uniqueness, ≥1 of requests|tokens, strictly-positive-finite `per_minute` (no rate-zero quotas in v1, exactly as 6.4 forbids rate-zero API buckets), non-negative `burst`), and `Load(inline, file)` (inline XOR file XOR empty=unlimited; file IO lives here so config stays env-pure); `config.ResourcesConfig` (`AGENTLOOM_RESOURCES` / `AGENTLOOM_RESOURCES_FILE`, mutual exclusion rejected at `config.Load`); `cmd/worker` loads + logs the set at boot (held for 9.2's middleware — `cmd/api` untouched, it never executes). ADR-006 cross-updated: `throttled` in the outcome vocabulary note, taxonomy rows 16–17, retry-budget exclusion, Enforcement-points M9 line. `.env.example` documents both sources. Tests: full `internal/limits` unit matrix (valid golden, wildcard/exact/unlimited resolution, rate helpers, every validation-error path incl. all-at-once, inline/file/both/missing/empty `Load`, nil-`Set` safety) + config parse/mutual-exclusion cases. No migration and no runtime behavior change in 9.1 — the `throttled` CHECK, the two-key script, the middleware, and metrics all land in 9.2/9.3.)*
**Done when:**
- [x] ADR covers dual buckets, requeue math, and fairness stance
- [x] Resource config parsed/validated with tests; unknown-resource policy decided
- [x] `throttled` outcome added to taxonomy (ADR-006 cross-updated)

#### 9.2 — Limiter middleware for executors ✅
**Depends on:** 9.1
Executor middleware acquiring (request-bucket 1, token-bucket estimated cost) before provider calls; on deny: record `throttled`, schedule delayed redelivery, release the worker (no attempt consumed). Rough token estimator now (chars/4 + declared `max_tokens`); refined by M12's counters. Metrics: throttle count, queue-wait time by resource. *(As built: the two-key atomic acquire `ratelimit.AcquireDual` (6.3's deferred second Lua script — refills both buckets against one Redis clock, grants only if both hold their cost, debits both or neither, `retry_after = max` of the denying buckets, denied-dimension code requests/tokens/both) with the drift property — a token denial never debits the request ledger — proven by an all-or-nothing concurrency stress; the leaf adapter `internal/ratelimit/resource` maps a resolved resource name + token estimate onto the buckets (exact → wildcard → unlimited-skips-Redis, dual vs single vs meter-nothing), surfacing `ErrCostExceedsCapacity`/`RetryAfterNever` for the perm-fail edges; the `exec.ResourceClaimer` hook (ADR-010's interface verbatim) implemented on the llm executor (`<provider>:<model>` by the resolved provider, estimate chars/4 + `max_tokens`) and the tool executor (`tool:<name>`, requests-only); the engine `rateLimit` middleware in `execute()` — bind → acquire → route: granted/unlimited/claim-error/Redis-error(fail-open) proceed, `ErrCostExceedsCapacity`/`RetryAfterNever` perm-fail to the DLQ (source permanent), a genuine denial routes `completeThrottle`; `completeThrottle` computes the requeue delay via pure `throttleDelay` (clamp(retry_after, floor, cap) + additive jitter — deliberately not full jitter, since retry_after is a real refill deadline), records `throttled` via `store.ThrottleStep` (5.2's `running → retrying` CAS reused verbatim — claim cleared, `next_attempt_at` stamped, `step_throttled` event, **no** steps_failed bump, **not** a counted failure so the retry budget is untouched), and post-commit schedules the delayed re-dispatch (reason `throttle`, no `EnqueuedAt` → ZADD dedup); a throttle on a cancelling run settles as cancelled; migration 0014 adds `throttled` to the outcome CHECK; the reconciler's overdue-retrying scan heals a lost throttle schedule with zero new duty; worker config `AGENTLOOM_RESOURCES_KEY_PREFIX` + `_THROTTLE_{FLOOR,CAP,JITTER_FRAC}`; obs subsystem `ratelimit` (`throttled_total{resource,bucket}`, `throttle_wait_seconds{resource}`, `fail_opens_total`), `resource` on the ADR-008 allowlist; openapi outcome enum + ADR-005 reason vocab + ADR-010 metrics section amended. Headline `TestFleetRespectsResourceLimit`: 4 workers sharing one real Redis bucket, 24 runs, the token-bucket cumulative bound (calls ≤ burst + refill×elapsed) asserted against every provider call and all runs completing; plus fake-clock throttle-defer-and-complete (history `[throttled, succeeded]`, budget 0, one event), the two perm-fail paths → DLQ, fail-open, and cancelling-run settlement.)*
**Done when:**
- [x] Multi-worker integration: 4 workers vs R=10/min limit → mock provider sees ≤10±ε calls/min
- [x] Throttled steps eventually complete; attempt counter unchanged by throttles
- [x] Worker slot released immediately on throttle (throughput of other runs unaffected — asserted)

#### 9.3 — Token-cost reconciliation ✅
**Depends on:** 9.2
Post-call reconciliation: debit/refund the token bucket by (actual usage − estimate) so sustained error in the estimator can't let the fleet exceed provider token budgets. Property test: long-run conservation of bucket accounting. *(As built: the correction is a **third Lua script** `ratelimit.Adjust(ctx, Bucket, delta)` beside the single-key `Acquire` and two-key `AcquireDual` — it refills to the Redis clock exactly as they do, then applies `tokens -= delta` with the asymmetry that makes reconciliation sound: a **positive delta (under-estimate) is an unclamped debit** that may drive the balance negative — the *unchanged* acquire scripts already deny while `cost > tokens` and grow `retry_after`, so the debt throttles later acquires until refill repays it — while a **negative delta (over-estimate) is a refund clamped at capacity**; the TTL rule composes with a negative balance for free (time-to-full grows, so the debt survives an expiry that would otherwise reset the bucket to full). The leaf adapter grew `resource.Reconcile(ctx, name, est, actual)` — re-resolves exact→wildcard→unlimited and `Adjust`s **only the token bucket** (the requests cost of 1 is exact), a no-op touching no Redis for an unlimited/requests-only resource or a zero delta — and `Decision.TokensMetered` (true only for a dual or tokens-only-single acquire) so the engine reconciles exactly what it debited. Engine middleware: `rateLimit` returns a `reconcileBinding` on a granted, token-metered acquire; after the executor returns, `execute` reconciles iff `execErr == nil && out.Usage != nil` — an errored call carries no usage so its estimate stays debited (deliberately conservative), a success racing a timeout/cancellation still reconciles (the tokens were spent), the shutdown-abandon path skips it (no completion). Reconciliation is **fail-open** like the acquire (a Redis error logs + counts, never affects the step). New instruments under the `ratelimit` subsystem: `engine_ratelimit_estimate_error_tokens{resource}` (signed histogram, the first `_tokens`-unit histogram — ADR-008's unit table + `TestInstrumentConformance` widened) and `engine_ratelimit_reconcile_failures_total`, labeled by the resolved config-entry name. Tests: `ratelimit` Adjust matrix + the **conservation property test** (`TestReconcileConservationProp` — random `AcquireAt`/`AdjustAt` interleavings match a pure-Go model's exact float64 balance at every step); `resource` no-op/debit/refund with the requests ledger proven untouched; `engine` wiring (exact est/actual passed, histogram observed, non-metered grant never reconciled, fail-open) + the headline `TestFleetActualTokensRespectResourceLimit` — 4 workers, a biased-3×-low estimator, cumulative **actual** tokens held within `burst + refill×elapsed + in-flight slack` (a bound violated 3× over without reconciliation). No migration, config, or store change. ADR-010 gained the "Token-cost reconciliation (as built, 9.3)" section, ADR-008 the `_tokens` unit + ratelimit inventory rows.)*
**Done when:**
- [x] Over- and under-estimates reconciled on the bucket (unit + property tests)
- [x] Sustained load with biased estimator stays within configured tokens/min (integration)
- [x] Reconciliation metrics exposed (estimate error histogram)

#### 9.4 — ADR-011 & cache key builder ✅
**Depends on:** 8.6
**ADR-011:** cache key = SHA-256 over canonical serialization (sorted keys, normalized numbers) of {schema_version, plugin name+version, model, sampling params, rendered messages/inputs, tool schemas} — *never* assume identical inputs ⇒ identical outputs: **default policy** caches only `temperature==0` LLM calls and `cacheable` pure tools; anything else is opt-in via step-level `cache: {mode: off|read_write|read_only, ttl, scope}`. Invalidation strategy: TTL, version-bump (plugin/prompt-template version in key), and admin bust-by-prefix. Storage: Redis with TTL + value size cap. *(As built: `docs/adr/011-response-cache.md`; the `cache` step-envelope field in `internal/dag` — `CacheMode`/`CacheScope` enums, `CachePolicy{mode,ttl,scope}`, strict decode + `cache_field_required`/`cache_field_invalid` validation with `MaxCacheTTL` 30d, regenerated JSON Schema, kitchen-sink construct pin; new leaf package `internal/cache` — `Key(KeyInput)` = hex SHA-256 over length-prefix-framed components (KeySchemaVersion, definition schema_version, scope+run id, executor + concrete-plugin identity, per-type request body), JSON components canonicalized via round-trip-through-`any`, `temperature` nil-distinct-from-0, `RedisKey` namespaced by concrete plugin for 9.6 bust-by-prefix; `Decide` encoding the two-layer policy — hard eligibility gate (`Cacheable && !SideEffectful`) then determinism-driven default, step-mode override within eligibility. Deferred to 9.5/9.6: the Redis store, the middleware, `config.CacheConfig`, migration materialization, metrics, and the bust/stats ops surface — no migration/config/runtime change in 9.4.)*
**Done when:**
- [x] Key-stability golden tests (reordered JSON keys, float forms → same key; any semantic change → new key)
- [x] Policy matrix (step type × determinism × capability flags) documented and encoded
- [x] ADR records why write-through Redis (not Postgres) and the size-cap fallback (skip caching oversized)

#### 9.5 — Cache store & middleware ✅
**Depends on:** 9.4, 9.2
Read-through/write-through middleware ahead of the rate limiter (hits skip limiter and provider entirely); value = output + usage snapshot; attempt records `cache_hit=true` with zero incremental cost + counterfactual "would-have-cost" usage. Metrics: hit/miss/bypass/store by plugin. *(As built: migration 0015 materializes `run_steps.cache_policy` (nullable JSONB) at instantiation like `retry_policy`/`timeout`, so the middleware reads the effective policy off the claimed row and never reparses the snapshot; the Redis store `internal/cache/redisstore` is a thin byte-oriented KV over `cache.RedisKey` owning the prefix/namespacing, the value size cap (`cache.ErrValueTooLarge`, the sentinel relocated into the `cache` leaf so the engine matches it without importing the store), and the TTL on write — it never knows an entry's shape (the engine marshals `{output, usage}` and hands opaque bytes, keeping `cache` a leaf, the pgfts precedent); the executor hook `exec.CacheBinder` (mirroring `ResourceClaimer`) is implemented on the llm/tool/retrieve executors, projecting the resolved request onto a `CacheBinding` (the two plugin identities, the **concrete** plugin's capability flags — the tool binding reports the *invoked tool's* flags, not the tool executor's, so `json_transform` is cacheable while `http_request` is not — the determinism signal, and the request body), a binder error routing "skip caching, let Execute classify"; the engine middleware `engine/cache.go` runs `cacheRead` in `execute()` **ahead of the rate limiter** — a hit completes from the stored result with no limiter acquire and no provider call (asserted: a granting recording limiter observes zero acquires on the hit), a miss threads a write binding to `cacheWrite` after a successful execution (write-before-commit, safe because the entry is a pure function of the resolved request); `SchemaVersion` is `dag.CurrentSchemaVersion` (only v1 exists); every error path is fail-safe (no binder / declined policy / unbuildable key / corrupt policy / corrupt entry / Redis error → execute uncached); `cache_hit` rides in the attempt's `usage` JSONB (no second migration — 0012 left `usage` open), `exec.Usage.CacheHit` marking the counterfactual counts, the API's `usage` pass-through and the OpenAPI schema needing only a doc touch-up (still lint 100/100); metrics are a new `cache` subsystem (ADR-008 amended) — `hits`/`misses`/`bypass`/`stores{plugin}` + unlabeled `fail_opens`; config `AGENTLOOM_CACHE_{ENABLED,KEY_PREFIX,DEFAULT_TTL,MAX_VALUE_BYTES}` (enabled default, 24h TTL ≤ 30d ceiling, 1 MiB cap), `cmd/worker` builds the redisstore over the shared coordination Redis and wires `engine.WithResponseCache`, `cmd/api` untouched (ADR-002 intact). Tests: redisstore integration (round-trip/TTL/oversize-skip/namespacing), exec binder unit matrix (resolved model + nil-vs-0 temperature, tool-level flags, retrieve non-determinism, error paths), engine pure-helper unit tests + the headline integration suite (two temperature=0 runs → 1 provider call + 1 limiter acquire + `cache_hit` counterfactual usage on the second; temperature>0 default bypass → 2 calls; `read_write` opt-in → hit; `mode: off` → 2 calls); ADR-011 gained the "Cache store & middleware (as built, 9.5)" section. Deferred to 9.6: bust-by-prefix + stats endpoints, `ctl cache`, the ops runbook.)*
**Done when:**
- [x] E2E: identical `temperature=0` mock-LLM steps → second is a hit (no provider call, no limiter acquire)
- [x] `temperature>0` bypasses by default; `read_write` opt-in overrides (both asserted)
- [x] Hit recorded on the attempt with counterfactual usage for M10

#### 9.6 — Cache invalidation & ops surface ✅
**Depends on:** 9.5, 6.5
Admin API: bust by prefix/scope (SCAN-batched, non-blocking, audited event), cache stats endpoint (hit rates by plugin); `ctl cache bust|stats`. Invalidation behavior documented in the ops runbook. *(As built, on the 9.5 store + `cache.RedisKey` namespacing with **no migration, no engine change, no new config var**: the `internal/cache` leaf grew `BustMatch`/`BustPattern` (glob `<prefix>:v*[:<kind>[:<name>]]:*`, metacharacters escaped, matching `v*` so a bust also sweeps entries stranded behind an earlier `KeySchemaVersion`, and structurally excluding the `stats:` namespace) plus the stats-key helpers (`StatsRedisKey`/`StatsPattern`/`ParseStatsKey`/`NewPluginStats`/`ParseCounter`); `redisstore` gained **durable per-plugin counters** — `Get`/`Set` best-effort-increment `hits`/`misses`/`stores` in a `<prefix>:stats:<kind>:<name>` hash (one pipelined `HINCRBY`+`PEXPIRE`, 30-day self-cleaning TTL, swallowed on error so a counter never gates a step; they mirror the engine's Prometheus `engine_cache_*` counters on the normal path — the one divergence a corrupt stored value counts a store-hit vs an engine fail-open), `Bust(BustMatch)` (`SCAN COUNT 512` + batched non-blocking `UNLINK`, point-in-time, returns the deleted count), and `Stats()` (`SCAN` the stats namespace, dedup, `HGETALL`, sorted by kind then name); the API added the `CacheOps` seam (`*redisstore.Store`; the api package imports only the `cache` leaf, never go-redis) + `WithCacheOps`, and the two **admin-scoped** routes `POST /v1/cache/bust` (namespace selector `{plugin_kind?, plugin_name?}` validated to the three cacheable kinds — a name-without-kind or non-cacheable kind is a 400; a structured **audit line** carries the actor `key_id`, namespace, and count) and `GET /v1/cache/stats` (per-plugin `{hits, misses, stores, hit_rate}`), both **503 `cache_unavailable`** (new error code) when unwired — route→scope and route→class tables, the auth matrix, and OpenAPI (still lint 100/100) all extended; `cmd/api` now builds one Redis client shared by rate limiting and the cache ops surface (fail-soft, ADR-002 intact — the API never reads a cached *result*), wiring `WithCacheOps` when caching is enabled; `ctl cache bust|stats` mirror the endpoints. Tests: `internal/cache` pattern/stats-key unit matrix (incl. a glob matcher proving stats keys are un-bustable); `redisstore` integration for the counters, the three bust granularities leaving non-matching entries + all stats intact, and a **3k-key-per-namespace under-load** test (concurrent Get/Set throughout a bust, zero errors on the live ops, exact deleted count, the other namespace intact); api behavioral suite (namespace mapping, audit-log key_id, stats projection, 400 matrix, 503) + the auth-matrix rows; the headline engine-layer e2e `TestCacheStatsReconcileAndBust` (two temperature=0 runs → the stats endpoint's numbers equal the gathered `engine_cache_*` counters exactly, then an admin bust forces the third identical run to miss and re-call the provider, counters surviving the bust); `ctl` unit tests. Docs: ADR-011 "Invalidation & ops surface (as built, 9.6)" + ADR-007 route rows, new `docs/ops-runbook.md` (TTL / version-bump / admin-bust decision guide), `docs/api.md` walkthroughs.)*
**Done when:**
- [x] Bust removes matching keys without blocking Redis (verified under load)
- [x] Admin-scope enforced; action audit-logged with actor key ID
- [x] Stats endpoint matches Prometheus counters in an integration test

---

## Milestone 10 — Cost tracking & budget enforcement

**Goal.** Differentiator #3: a real-time cost ledger (per attempt → per step → per run) from a versioned pricing catalog, budgets at step and run level, enforcement actions (proceed / downgrade via model fallback chains / park with `budget_exceeded` / fail), and cost events + metrics that later drive the dashboard's live meter.

**Role.** Cost awareness must sit *inside* the scheduler path (checked at claim time), not be a reporting afterthought — that's what makes it a scheduling feature rather than a dashboard widget.

**Architecture docs:** ADR-012 (cost model: attribution, estimation, budget semantics).

**Exit criteria:** a mock-priced workflow shows accurate cumulative cost in the API; a budgeted run downgrades models at the configured threshold and parks exactly at its cap; raising the budget resumes it.

#### 10.1 — ADR-012 & pricing catalog ✅
**Depends on:** 8.6
**ADR-012:** attribution rules (LLM usage × price; priced tools; cache hits = $0 actual + counterfactual saved; validation/judge and summarization overhead attributed to their step, flagged as overhead), estimation approach for pre-flight checks, budget semantics (hard vs soft), unknown-model policy (configurable: conservative default estimate + warning event, or fail-closed). Versioned pricing catalog: embedded defaults + file/env override, effective-dated entries ($/1M input & output tokens per model). *(As built: `docs/adr/012-cost-model.md`; the contract-half leaf `internal/cost` (stdlib only, imports no other agentloom package — the cache/limits precedent) — money is integer **nano-USD** (`int64`, 1 USD = 1e9, half-away-from-zero rounding) so 10.2's run-aggregate-equals-exact-ledger-sum holds without float drift; the pricing catalog keys off the **ADR-010 resource name** (`<resolved-provider>:<model>`, `<provider>:*`, `tool:<name>` — the same string the limiter uses, so `mock/sim-1` → `mock:sim-1` for both features), `Parse` doing strict decode + all-errors-joined validation (schema_version==1, name rules + model/tool namespace split, non-negative finite rates, required `effective_from`, duplicate `(name, date)` rejected), `Load(inline, file)` merging an operator override (`AGENTLOOM_PRICING[_FILE]`, mutually exclusive) **onto embedded `defaults.json`** by `Merge` (whole-list-per-name replacement + fallback override, copy-before-overlay so the shared `Default()` is never mutated), `Lookup`/`PriceModel`/`ToolPrice` with effective-date selection (newest entry ≤ query time; scheduled reprice = a second entry, old runs keep their rate), exact→`<provider>:*` wildcard→not-found resolution, and the unknown-model policy; `price.go` the pure arithmetic (`Cost`/`Estimate` upper-bound/`ToolCost`); `event.go` the `cost_unknown_model` payload contract; `Source` recording rate provenance for the 10.2 ledger. `config.CostConfig` carries the override source + `UnknownModelPolicy` string (`estimate`|`fail`, default estimate), config staying a leaf (validates the string, doesn't import cost — the limits precedent). `cmd/worker` boot-loads-and-logs (malformed override fails boot); `cmd/api` untouched. Unknown-model policy is split by design: **fail-closed governs pre-flight only** (10.3 blocks an unpriced model before spend, permanent), **post-call ledger pricing always succeeds** (fallback rate + warning, since the money is already spent) — the caller passes the policy. **No migration/store/engine/executor change in 10.1** — the `cost_ledger`, attempt-completion pricing, claim-time budget check, downgrade chains, the physical event append, and cost metrics are 10.2–10.5.)*
**Done when:**
- [x] Catalog loads/validates; effective-date selection tested
- [x] Unknown-model policy implemented per ADR with warning event
- [x] Attribution rules enumerated in ADR with worked examples

#### 10.2 — Cost ledger & aggregation ✅
**Depends on:** 10.1
`cost_ledger` rows per attempt (usage, rate snapshot, computed cost, overhead flag) written in the attempt-completion transaction; run-level aggregates (`spent_usd`, by-model/by-step breakdowns) updated atomically in the same tx; `GET /v1/runs/{id}` includes cost summary; `GET /v1/runs/{id}/cost` full breakdown. *(As built: `exec.Output.Resource` is the new channel by which the metered executors report the ADR-012/ADR-010 resource name — the llm executor `<provider>:<served-model>` (priced at the model that actually served, so a dated variant hits the provider wildcard and 10.4's downgrade prices the real model), the tool executor `tool:<name>`, empty = not cost-bearing; the engine's cache middleware carries it across a hit via a new `cost_resource` field on the stored cache entry (`omitempty`, so a pre-10.2 entry decodes to `""` and just ledgers no saved figure). Pricing is a pure pre-transaction function (`engine/cost.go` `priceAttempt`, no DB reads) returning a `store.AttemptCostArgs` or nil (nothing to ledger: pricing disabled, no resource, an unpriced/free tool, or a model attempt with no usage); it always passes `PolicyEstimate` — post-call pricing never fails a succeeded attempt, so an unknown model is priced at the catalog fallback with a `cost_unknown_model` warning event, **except on a cache hit** (spent nothing; the miss already warned). `complete.go` computes the row pre-tx and `store.ApplyAttemptCost` writes it *after* `SucceedStep` landed (a fenced zombie completion never ledgers) under the run lock the CAS already holds. Migration 0016: `cost_ledger` keyed `(run_id, step_id, attempt, entry)` — `entry` discriminates the charge kind (`attempt` now; ADR-012 rule 4's `judge`/`compaction` overhead rows fill the same-attempt slot in M11/M12 with no schema change) — with `resource`, `usage`, `rate` snapshot, `rate_source`, `cache_hit`, `overhead`, `cost_nano_usd`, `saved_nano_usd`; plus scalar `runs.spent_nano_usd` / `saved_nano_usd` (columns carry the nano unit per ADR-008's units-in-name convention, not the roadmap's shorthand `spent_usd`), bumped in the same tx so the aggregate is an exact integer `spent == SUM(cost_nano_usd)`. **By-step and by-model breakdowns are read-time `GROUP BY` over the ledger, not materialized** — the rows commit in the same tx as the aggregate, so a read-time group is exactly consistent with zero write contention. Store: `store.ApplyAttemptCost` (transition-style, `ErrNoTx`-guarded, inside the completion tx — insert row + bump aggregate + append warning) and `store.CostRepo` (`Ledger()` — list + the two breakdowns + the property test's independent sum); `store.EventCostUnknownModel` joins the vocabulary. API: `GET /v1/runs/{id}` carries a `cost` summary on the run view, `GET /v1/runs/{id}/cost` returns summary + `by_step` + `by_resource` (per-model/per-tool, token sums) + full per-attempt `entries`; money is integer nano-USD on the wire with `*_usd` strings rendered by exact integer division (never float); OpenAPI still lints 100/100. Attribution corners: cache hit = $0 row + `cache_hit` + counterfactual `saved`; priced tool = flat row; **unpriced tool = no row** (free, rule 3); no-usage/non-cost-bearing = nothing; `overhead` always false in 10.2 (pre-wired). `cmd/worker` wires `engine.WithPricing(cfg)` from the boot-loaded catalog; `cmd/api` untouched (it only reads ledger rows). Tests: the concurrent-fan-out exact-sum property test, the two-run cache-hit saved test, the unknown-model fallback+event test, the `priceAttempt` unit matrix, the `ApplyAttemptCost` store test (aggregate/event/ErrNoTx), and the API `/cost` per-step/per-model contract test. **No new config var, no engine hook interface, no metric (10.5), no budget check (10.3).)*
**Done when:**
- [x] Property test: concurrent attempt completions → aggregate equals sum of ledger exactly
- [x] Cache hits ledger $0 with counterfactual field populated
- [x] Cost API returns per-step and per-model breakdowns (contract test)

#### 10.3 — Budget enforcement at claim time ✅
**Depends on:** 10.2, 5.6
Budgets: run `budget_usd`, step `max_usd`/`max_tokens`. Claim path evaluates projected spend (spent + pre-flight estimate); post-attempt re-evaluates actuals. Actions per policy: `proceed`, `downgrade` (10.4), `park` (reason `budget_exceeded`, resumable), `fail`. `PATCH /v1/runs/{id}/budget` raises budget; unpark resumes. *(As built: the contract — run-level `budget_usd` (`*float64`, nil = unbudgeted) + `on_budget_exceeded` (`park`|`fail`) and step-envelope `budget: {max_usd, max_tokens}` in `internal/dag`, codes `budget_field_required`/`budget_field_invalid`, `max_tokens` restricted to llm steps; migration 0017 materializes `runs.budget_nano_usd` (nullable) / `runs.on_budget_exceeded` and `run_steps.budget_policy`, and extends the `step_attempts.outcome` CHECK with the administrative outcome **`budget_exceeded`** (uncounted, like `lost`/`throttled`). The estimate rides a new optional `exec.CostEstimator` (input/output token split; llm `<provider>:<model>`, tool `tool:<name>`). The middleware `engine/budget.go`'s `budgetCheck` runs **after cacheRead, before rateLimit** (a hit is $0; parking after a limiter acquire would strand debited tokens) — pure `budgetDecide` prices the estimate (upper-bound `cost.Estimate` under `WithUnknownModelPolicy`) and routes: step `max_tokens` over cap → permanent (oversized request never sent), unknown model under fail-closed → permanent, step `max_usd` (cumulative ledger + estimate) → permanent, run budget (`spent + estimate`, from the claim-time `ClaimOrigin` spend under the run lock) → park or fail. Park is atomic (`store.BudgetParkStep`: release `running → ready`, attempt `budget_exceeded`, `budget_exceeded` event, park run — only re-parking when still running, so fan-out siblings release without a duplicate `run_parked`; the released step is `ready`, so unpark re-dispatches it with zero reconciler dependency); fail reuses `completeFailure(permanent)` → DLQ + `on_failure`. Resume: `engine.Control.SetBudget` + `PATCH /v1/runs/{id}/budget` (submit; `run_budget_updated` event; terminal → 409) + unpark; `CostSummaryView` grows budget fields; `ctl budget <run-id> <usd>`; `cmd/worker` maps `cfg.Cost.UnknownModelPolicy` via new `cost.ParseUnknownModelPolicy`. Headline integration: a $0.005 mock chain parks at exactly the step whose projection exceeds the cap (`spent ≤ budget < spent + one estimate`), then raise-to-$1 + unpark → success with all steps ledgered; plus fail-policy DLQ, oversized-`max_tokens` zero-provider-calls, the decision unit matrix, and the PATCH contract. OpenAPI 100/100; no metrics (10.5) or downgrade chains (10.4) yet.)*
**Done when:**
- [x] Run parks within one step of its cap under mock pricing (test tolerance defined)
- [x] `budget_exceeded` event emitted with projection details; API raise → unpark → completes
- [x] Step-level `max_tokens` enforced pre-flight (oversized request never sent)

#### 10.4 — Model downgrade chains ✅
**Depends on:** 10.3
Step config `model_fallbacks: [...]`: budget-threshold triggers (e.g., >80% of run budget → next cheaper model) evaluated at claim; the actually-used model recorded on the attempt and priced correctly; downgrade emits an event; interacts correctly with cache keys (different model ⇒ different key — asserted). *(As built: the contract is an ordered `model_fallbacks: [{model, at_budget_fraction?}]` chain on the **llm step config** in `internal/dag` (`ModelFallback`, code `config_field_invalid` — models non-empty + distinct from the primary and each other, `at_budget_fraction` in (0,1) and requiring a run `budget_usd`, thresholds non-decreasing along the cheapening chain, and a chain with no budget to trigger against rejected), regenerated JSON Schema + kitchen-sink coverage (both copies). No migration — the used model is already durable on 10.2's `cost_ledger.resource` and the attempt output. Two triggers evaluated at claim: **soft** (once run spend reaches a fallback's `at_budget_fraction`, route proactively to that tier — even if the primary fits) and **hard** (the primary's projected `spend + estimate` would exceed a budget → route to the least-aggressive fitting tier, avoiding a park); a tier is chosen only if priceable **and** its projection fits every budget, so the engine never downgrades to a model it would immediately park on, and an exhausted chain falls through to 10.3's ordinary park/fail on the primary. New optional executor hook `exec.ModelDowngrader` (`ModelFallbacks` reports current+chain; `WithModel` rewrites only the config's `model` field via a raw-message re-key so the fallback config differs from the primary by exactly the model) — implemented on the llm executor; because the cache key, resource limiter, cost estimate, and provider call all derive from that model, one rewrite re-targets the whole pipeline (**different model ⇒ different cache key**, asserted at the exec layer in `TestDowngradeChangesCacheKey`). The decision lives in `engine/budget.go`'s restructured `budgetCheck`/`budgetDecide` (max_tokens checked first as model-independent; the step's cumulative cost read once and threaded into both the downgrade fits-check and the step `max_usd` cap) plus a new pure `chooseDowngrade` (`engine/downgrade.go`, unit-matrix-tested: threshold/projection/rescue/exhaustion); on a downgrade the middleware returns the re-targeted config, records a **`model_downgraded`** event (`store.RecordModelDowngrade`, fenced on the caller's claim in its own short `step.downgrade` tx — no state transition, so no CAS; a fenced caller abandons like any other fenced write) carrying from/to models + resources + trigger + the spend/budget projection, and the claim path re-keys the response cache on the fallback model (a fallback cache hit still short-circuits the provider call) before proceeding. Headline integration (`downgrade_integration_test.go`, a catalog pricing `mock:expensive` 50× `mock:cheap`): an expensive-model chain crosses its 0.5 threshold mid-run, gen0/gen1 ledger `mock:expensive` and gen2–gen4 ledger `mock:cheap` (the served model priced at its own rate, output model reflects the swap), three `model_downgraded` threshold events, exact run-aggregate = ledger sum; plus a projection-trigger test (budget too tight for the primary → immediate downgrade, `budget_projection` limit run), and an exhausted-chain test (even the cheap tier over budget → park with no downgrade event, then raise-budget + unpark → completes on the primary). No new migration/config/metric — downgrade metrics are 10.5.)*
**Done when:**
- [x] E2E: expensive→cheap downgrade fires at threshold; ledger prices actual model
- [x] Downgrade event carries from/to models + trigger reason
- [x] Exhausted fallback chain falls through to the configured budget action

#### 10.5 — Cost metrics & events ✅
**Depends on:** 10.2, 7.2
Prometheus: cost counters by model/plugin (bounded labels), budget-park counter, saved-by-cache counter. `cost_updated` events appended per attempt completion (feeds the M18 live meter via M16). Grafana cost panel added. *(As built: a new ADR-008 `cost` subsystem — six bounded-label counters `engine_cost_{spent,saved}_usd_total{resource}` (money in the base **USD** unit, `float64(nano)/1e9`; the ledger keeps the exact integer nano-USD), `engine_cost_{input,output}_tokens_total{resource}`, `engine_cost_budget_exceeded_total{limit,action}`, `engine_cost_downgrades_total{trigger}` — `resource` the pricing-catalog name shared with the ledger/limiter, so cardinality stays within budget (new `limit`/`action`/`trigger` allowlist rows, three values each). Spend/saved/tokens are recorded **post-commit** in `completeSuccess` from the priced `store.AttemptCostArgs` (`engine.recordCost`), so a fenced/rolled-back completion counts nothing — a cache hit records only saved, a productive call records spend + billed tokens; the budget counter increments at the claim-time decision (park inside `completeBudgetPark`, fail at the two `completeFailure` routes, only on a committed completion — each fan-out sibling counts once, mirroring the `budget_exceeded` event); the downgrade counter increments once per recorded `model_downgraded`. New `engine.Metrics` methods `CostSpent/CostSaved/CostTokens/BudgetExceeded/ModelDowngraded` (+ `nopMetrics`), instruments + registration + recorders in `internal/obs/metrics`, conformance tables extended (`allowedSubsystems += cost`, `allowedLabels += limit/action/trigger`, `exercise` touches them). The **`cost_updated`** event (`store.EventCostUpdated` = `cost.EventTypeCostUpdated`) is appended by `ApplyAttemptCost` **inside** the completion transaction — under the run lock, same monotonic seq as the aggregate bump — once per cost-bearing attempt (same guard as the ledger row; cache hits included since saved moves). `AddRunCost` grew a `RETURNING spent_nano_usd, saved_nano_usd, budget_nano_usd` (its only SQL change, now `:one`), so `store.CostUpdatedEvent` carries the attempt's charge **and** the run's running totals + budget after the bump — the M18 meter reads the total straight off the stream, stateless. Sharing the lock+seq with the bump makes a run's `cost_updated` totals **non-decreasing in seq order by construction** (asserted in the store `TestApplyAttemptCost…` and the new engine `cost_metrics_integration_test.go`, cross-checked against the run aggregate — the M18.4 consistency check pre-paid); the `cost_unknown_model` warning now follows `cost_updated` (higher seq). `ApplyAttemptCost` now returns `(store.CostApplied, error)`. Grafana: the Engine board gained a **Cost** row (spend rate + saved-by-cache rate by resource, tokens/s, budget-actions/downgrades) with the anti-drift audit green; a dev-scale `BudgetParkRateSpike` example alert (+ promtool test, `make obs-lint` green); `make smoke-dashboards` drives a temperature=0 mock pair (miss + cache-hit savings) so the spend/saved panels are genuinely non-empty (budget/downgrade panel allowlisted — no budgeted smoke runs) and `make smoke-metrics` asserts the spend/saved/token counters positive. **No migration, no config var, no store primitive beyond the `RETURNING`.** The event-feed read API + pub/sub are M16 (ADR-018); the meter UI is M18.4. Closes M10.)*
**Done when:**
- [x] Metrics visible under chaos-suite load; cardinality within ADR-008 budget
- [x] `cost_updated` events in the run event feed with monotonic run totals
- [x] Grafana panel shows spend rate + saved-by-cache

---

## Milestone 11 — Output validation & semantic retries

**Goal.** Differentiator #1: a validator chain on step outputs (JSON Schema, CEL/regex, LLM-judge), deterministic JSON repair and provider-native structured-output modes to prevent failures at the source, and the semantic retry engine — on validation failure, retry with a critique-augmented prompt (what was produced, what was wrong), under its own attempt counters and budgets, distinct from mechanical transport retries.

**Role.** This is the sharpest contrast with Temporal-style engines: the retry input *changes* based on the failure. The `validation_failed` outcome reserved in M5 activates here; interplay with cost (retries are metered) and caching (invalid outputs never cached) was pre-wired in M9/M10.

**Architecture docs:** ADR-013 (validation & semantic retry semantics).

**Exit criteria:** a mock provider scripted to emit malformed→malformed→valid output yields a run that succeeds on semantic attempt 3, with verdicts and augmented prompts visible in attempt history; judge validator works with cost attributed as overhead.

#### 11.1 — ADR-013 & validator SPI ✅
**Depends on:** 8.1, 5.1
**ADR-013:** validator chain config on steps, verdict model (`pass|fail`, issues[] with codes/paths/messages, optional score), `validation_failed` outcome semantics vs transport failures (separate counters, separate policies), feedback-prompt construction rules, interplay: failed outputs never cached; semantic attempts are cost-metered and budget-constrained; judge outputs are never themselves validated (no recursion). Validator SPI registered as a plugin kind; verdicts persisted per attempt. *(As built: `docs/adr/013-output-validation-and-semantic-retries.md` (Accepted) + the leaf package `internal/validate` — the fifth ADR-009 kind-owning package, the `internal/tools` structure exactly: the `Validator` interface (`Manifest() + Validate(ctx, Input) (Verdict, error)`), the persisted `Verdict{schema_version, status, score?, issues[], results[]}` / `Issue{validator, code, path, message}` model, the typed `validate.Registry` facade over `plugin.Registry` (kind validator) that **compiles each config JSON Schema once at registration** and gates config pre-flight (`ValidateConfig`), typed `*validate.Error{Class}` / `*UnknownValidatorError` / `*ConfigValidationError` (a validator error is a *transport* failure of the stage, distinct from a fail verdict), and the `Chain` runner (cheap-first ordering — all cheap validators run for a full critique, cost-bearing ones only if the cheap ones pass; RFC 6901 target selection with `/text` default for llm-family; a missing target is a fail verdict, not a panic). `NewBuiltins()` is **empty in 11.1** (11.2 fills it). **Contract:** `internal/dag`'s `Step.Validation` envelope field + `ValidationPolicy`/`ValidatorSpec` (strict decode rejecting 11.4's reserved keys; codes `validation_field_required`/`validation_field_invalid` — ≥1 validator, name shape, JSON-pointer syntax, `MaxValidators` 16), regenerated JSON Schema, both kitchen sinks carrying a validation chain (construct pin extended), decode + structural fixtures. **Store:** migration 0018 — `run_steps.validation_policy` (materialized at instantiation like cache_policy), `step_attempts.verdict` (the 0012 usage precedent), widened outcome + dead_letters-class CHECKs adding `validation_failed`; `finishAttempt` threads a verdict param through all 8 call sites; `SucceedStepArgs.Verdict`, `DeadLetterStepArgs.{Usage,Verdict}`, `AttemptOutcomeValidationFailed`. **Engine:** `engine/validate.go` — `resolveChain` pre-flight (before cacheRead/spend; unresolvable → permanent), the validate stage in `execute()` **after execute, before cacheWrite** (invalid outputs never cached — ADR-011), `completeSuccess` grew a verdict param (persisted on the attempt), `completeValidationFailure` (records `validation_failed` with usage + verdict, meters the spent cost via the reused priceAttempt/ApplyAttemptCost, dead-letters source `retries_exhausted`, runs the `on_failure` disposition), and cache-hit **re-validation** (a stored-but-now-invalid output is re-executed, not served); `engine.WithValidators` seam. **API:** `AttemptView.Verdict` + OpenAPI `Verdict`/`VerdictIssue`/`ValidatorResult` components + `AttemptOutcome` enum gains `validation_failed` (and the previously-missing `budget_exceeded`), still 100/100; `GET /v1/plugins` lists the validator kind. `validation_failed` stays **rejected in `retry_on`** — ADR-013 gives validation its own semantic policy (11.4) rather than ADR-006's forecast "unlock", so the transport and semantic budgets are disjoint (`CountCountedFailures` unchanged). ADR-006/009/011/012 cross-amended. **Not yet:** built-in validators (11.2), the semantic-retry loop (11.4 — a failing verdict dead-letters on the first attempt in 11.1), llm_judge (11.5), quality metrics (11.6).)*
**Done when:**
- [x] SPI + verdict schema implemented; chain config validated in definitions
- [x] ADR decision table: transport failure vs validation failure vs throttle — routing for each
- [x] Verdict persistence visible in run status API

#### 11.2 — Deterministic validators ✅
**Depends on:** 11.1
Built-ins: `json_schema` (compiled once per definition; detailed path-level issues), `regex`/`contains`, `cel` (predicate over parsed output), `numeric_range`. All pure, fast, and unit-tested against a corpus of good/bad outputs. *(As built: the five deterministic validators in `internal/validate` — one file each, all `cacheable`-only at `1.0.0`, registered by `NewBuiltins()` (both deployables and `GET /v1/plugins` pick them up with zero wiring change). `json_schema` compiles the author schema via `santhosh-tekuri/jsonschema/v6` and reports one issue per violating instance location with the RFC 6901 pointer in `Path` (a string target — the `/text` default — is re-parsed as JSON; a non-JSON string is an `invalid_json` fail verdict, never a panic); `regex` (RE2, `negate`) and `contains` (`case_insensitive`, `negate`) stringify a non-string target as compact JSON (grep-like); `cel` evaluates a boolean predicate over a distinct env (`value`/`output`/`step_type`, `parse_json` opt-in) with compile-time bool enforcement but runtime eval errors as `cel_eval_error` fail verdicts; `numeric_range` accepts a numeric string and rejects NaN/±Inf. **New SPI hook `ConfigCompiler`** (`CompileConfig(config) error`): the registry calls it from `ValidateConfig` after the schema check, so an unparseable regex / non-boolean CEL / uncompilable schema / bound-less-or-inverted range is a permanent `*ConfigValidationError` at claim **before any spend**, and the call warms a `compileCache` keyed on config bytes so each artifact compiles **once per distinct config per process** — reused across every attempt/retry/takeover/run (benchmarked: warm `json_schema` ~20× cold). Table tests per validator (incl. malformed JSON → structured issues, never a panic), an in-package compile-once assertion, warm/cold benchmarks, api plugin-listing coverage for kind `validator` (config schemas exposed, UI-ready), and engine integration on the real built-ins (a `contains` pass and a `json_schema` malformed-output `invalid_json` fail, verdicts persisted). No migration, no config var, no metric, no engine/dag change. ADR-013/009 amended, `docs/plugins.md` + `docs/expressions.md` updated.)*
**Done when:**
- [x] Table tests per validator incl. malformed JSON → structured issue list (not a panic)
- [x] Compiled artifacts reused across attempts (no per-attempt recompilation — benchmarked)
- [x] Config schemas exposed via plugin registry (UI-ready)

#### 11.3 — JSON repair & structured-output modes ✅
**Depends on:** 11.2, 8.3
Cheap deterministic fixes *before* declaring failure: code-fence stripping, trailing-comma/unquoted-key conservative repair, first-JSON-object extraction. Plus provider-native structured output as an `llm` step option (Anthropic forced tool-use JSON; OpenAI `response_format`) to reduce failures at the source. Repairs recorded on the attempt (output provenance: `raw` vs `repaired`). *(As built: the `output_format` block on the llm step config (`type: json | json_schema`, optional `schema`, `mode: auto | repair_only`) in `internal/dag` (validated under `config_field_invalid`/`config_field_required`, llm-only, schema compilability deferred to claim pre-flight; regenerated JSON Schema, both kitchen sinks + construct pin, invalid fixture). The stdlib-only leaf `internal/jsonrepair` — `Repair(text)` runs conservative cumulative passes (strip_code_fence, extract_first_json, trailing_commas, unquoted_keys, never touching string contents, short-circuiting on first valid parse) → `raw | repaired | unrepairable` with the passes named; fixture-driven corpus with an exact asserted repair rate (14/17) + fuzz. Provider-native structured output on the llm interface (`ChatRequest.ResponseFormat`, `ChatResponse.Structured`): Anthropic forced synthetic tool (`agentloom_structured_output`, `tool_choice` forced, input lifted onto `Structured`, kept out of user-visible tool calls); OpenAI `response_format` (`json_object` / strict named `json_schema`); mock native `{"echo": <text>}` echo + scripted `Structured` — golden request + response fixtures both directions per provider, gated live smokes. The llm executor (`internal/exec/llmexec.go`) builds `ResponseFormat` for `mode: auto`, and post-call shapes the output: native → `json`/canonical `text`, else `jsonrepair` → `raw`/`repaired` (json + canonical text) or `unrepairable` (raw text kept, no json); persists a new `json` output field (so `${{ steps.x.output.json.field }}` flows structured data) and `exec.Output.Repair` provenance. An **implicit `json_schema` validator** is prepended to the step's chain at `engine.resolveChain` (author schema for json_schema, empty `{}` parseability for json), so an unrepairable output is a proper `invalid_json` fail verdict feeding 11.4 rather than a silent success — reusing all of 11.1/11.2 with zero `internal/validate` changes. Provenance persisted per attempt on **migration 0019** `step_attempts.repair` (nullable JSONB, the usage/verdict precedent — no new outcome/class), threaded through `finishAttempt`/`SucceedStepArgs`/`DeadLetterStepArgs`, carried across a cache hit (`cacheEntry.repair`), and surfaced as `attempts[].repair` (`RepairProvenance` OpenAPI component, still 100/100). `output_format` is a trailing cache-key component appended only when present (no `KeySchemaVersion` bump — existing entries stay valid; a `repair_only` format re-keys since it changes the output without changing the request). Canonical `examples/definitions/structured_extract.json` (corpus-pinned), engine integration suite (repaired → success + downstream `.json` flow; prose → validation_failed with `invalid_json` + unrepairable provenance; native echo → native provenance), executor unit suite (native/repaired/unrepairable/repair_only + output_format-changes-cache-key). No new config var, no new metric (repair rate is 11.6). ADR-013 §"JSON repair & structured-output modes (as built, 11.3)", ADR-011 cache-key amendment, `docs/plugins.md` updated.)*
**Done when:**
- [x] Messy-output corpus: repair rate measured and asserted (fixture-driven)
- [x] Structured-output flag round-trips through both real providers (fixture tests) and mock
- [x] Repair provenance visible in attempt record

#### 11.4 — Semantic retry engine ✅
**Depends on:** 11.1, 10.3
On failed verdict: outcome `validation_failed`; consult semantic policy (`max_semantic_attempts`, feedback template — default injects prior output + verdict issues + revision instruction into the next prompt); rebuild the prompt, run a new attempt (cache-bypassed by construction — augmented prompt ⇒ new key; also explicitly never cache invalid outputs); exhaustion → step failure → DLQ with all verdicts attached. Semantic and transport counters independent and both visible in status. *(As built: `ValidationPolicy` gained `max_attempts` (1..`MaxSemanticAttempts`=10; absent=1=11.1 behavior) + `feedback` (`FeedbackPolicy{template, max_output_chars}`, llm-family-only, requires max_attempts≥2) in `internal/dag`; a **deliberately narrow** feedback template (`internal/dag/feedback.go`) admitting exactly `${{ feedback.prior_output|issues|attempt|max_attempts }}`, one scanner shared by submit-time `CheckFeedbackTemplate` and retry-time `RenderFeedback`; a `validation` block with no validators is now admissible on an llm step with `output_format` (the implicit validator is the chain). Migration 0020 adds `run_steps.feedback` (the pending critique the claim CAS copies onto the next attempt, cleared on succeed/DLQ) + `step_attempts.feedback` (what each attempt was given) — the `exec.Repair`/JSONB precedent, no new outcome/class; the record is `exec.Feedback{schema_version, semantic_attempt, max_attempts, prior_attempt, text}`. Injection is `exec.FeedbackInjector.WithFeedback` on the llm executor (raw-message re-key: prompt-suffix / trailing user message), called in `execute()` **after resolveChain, before cacheRead**, so the augmented request re-keys the cache/estimate/limiter/provider by construction (a semantic retry is cache-bypassed — `TestFeedbackChangesCacheKey`). `completeValidationFailure` became a router: in-tx `CountValidationFailures` (validation_failed past the DLQ baseline, the exact mirror of `CountCountedFailures` — disjoint semantic budget, both requeue-re-armable) → `n<max` ⇒ `store.SemanticRetryStep` (running→retrying reusing the 5.2 CAS + a new `SemanticRetryRunStep` query stamping `feedback`, attempt closed validation_failed with usage/verdict/repair, cost ledgered) + an **outbox** re-dispatch (reason `semantic_retry`, `next_attempt_at=now` — no backoff; `reconcile_retry` heals a crash), new event `step_semantic_retry_scheduled`, post-commit `engine_step_retries_total{class=validation_failed}` + nudge; else the terminal DLQ with `semantic_attempts` + `verdict_history` on the record. Budget interplay is free: the re-attempt is an ordinary claim, so an over-budget projection parks the run (halting the loop, parked attempt `budget_exceeded`, feedback intact) and SetBudget+Unpark resumes. API: `AttemptView.feedback` + `StepView.{transport_failures,validation_failures}` (disjoint counters, derived from attempts) + OpenAPI `Feedback` component (still 100/100). Canonical `examples/definitions/semantic_retry.json`. Engine integration (fail→fail→pass on 3 semantic attempts with diffable stored feedback + 2 events; exhaustion→DLQ with 2-entry verdict_history + requeue re-arm; budget-park halts then resumes), api integration (feedback + counters on the wire), dag/exec unit suites. One migration (0020), no new config var, no new metric (depth histograms are 11.6). ADR-013 §"Semantic retry engine (as built, 11.4)"; ADR-005/006/011 cross-amended; `docs/plugins.md` updated.)*
**Done when:**
- [x] Mock scripted fail→fail→pass: succeeds with 3 semantic attempts; each augmented prompt stored and diffable
- [x] Budget interplay: semantic retries halt when the run parks on budget (integration)
- [x] DLQ entry for exhausted semantic retries carries the full verdict history

#### 11.5 — LLM-judge validator ✅
**Depends on:** 11.4
`llm_judge` validator: rubric prompt template, configurable (cheap) judge model with fallback chain, score threshold, judge usage cost-attributed to the step as overhead (10.1 rules), judge verdict + rationale stored. Guardrail: judges are terminal (never validated, never semantically retried). *(As built: the first **cost-bearing** built-in validator, `internal/validate/llm_judge.go` (`cacheable + cost_bearing` at `1.0.0`), filling the last `NewBuiltins` slot — which grew a `*llm.Registry` parameter (the `exec.Builtins` precedent) so the judge routes its model through the same provider registry the llm executor uses; `cmd/worker`/`cmd/api`/tests updated, `GET /v1/plugins` lists `llm_judge` with its config schema. Config: `model` (routed like an llm step) + ordered `fallback_models` (tried on provider errors) + `rubric` (static text) + `threshold` (pass iff `score >= threshold`) + `on_error` (`fail` default | `skip`) + optional `max_tokens`/`temperature`/`timeout`/`max_output_chars`. The `CompileConfig` pre-flight (11.2's `ConfigCompiler`) makes every content error — blank rubric, out-of-range threshold, duplicate/blank fallback, bad `on_error`/`timeout`, and a `model` (or fallback) that does not route (nil registry = keyless build routes nothing) — a permanent config error at claim, before the productive step spends. `Validate` builds a fixed content-free system prompt + rubric/candidate-output user message + a `ResponseFormat` (native structured output for real providers; `jsonrepair` fallback reused from 11.3) at temperature 0, calls the model chain under a per-call `context.WithTimeout` (a provider error or the judge's own timeout falls through to the next model; a caller-context cancel passes through **unwrapped** so the engine keeps the timeout/cancelled judgment), and parses a required `{score∈[0,1], rationale}`. Pass → a verdict carrying the score + a `Results[0]` with `rationale`+`usage`; below-threshold → a `rubric_below_threshold` fail whose message carries the rationale (so 11.4's `formatVerdictIssues` folds it into feedback with no change). New optional `ValidatorResult.{rationale,usage,error}` + `ValidatorUsage` (resource + served model + tokens) + `validate.Error.Usage`, all additive on the existing `verdict` JSONB — **no migration, no new config var, no new metric, no new outcome/class**; the `Chain` runner lifts them from a validator's single-result verdict. `on_error`: `fail` → a classified `*validate.Error` (provider error = transient, malformed answer = permanent) routed through `completeFailure`; `skip` → a pass verdict whose sole result is `status: error`, so an unavailable judge never blocks a workflow. **Cost as overhead (ADR-012 rule 4, exercised):** the pure `engine.priceOverheads` reads judge usage off the verdict results and applies `overhead: true` ledger rows under a `judge:<chain index>` entry (the same-attempt PK slot 0016 reserved — no schema change) inside the same completion tx on every verdict-carrying route (`completeSuccess` incl. cache-hit re-judge, `completeValidationFailure` semantic-retry + terminal DLQ, `completeFailure` malformed-under-fail `judge:e`), each priced under `PolicyEstimate`, bumping `runs.spent_nano_usd`, emitting `cost_updated`, and recording `engine_cost_spent_usd_total{resource=<judge model>}` — so judge spend counts against the run budget on the next claim and a step `max_usd` cap; the cost API surfaces `entries[].overhead` + a new per-step `overhead_nano_usd` roll-up (`AggregateCostByStep`, sqlc-regenerated) + OpenAPI `ValidatorResult`/`ValidatorUsage`/`CostByStepView` (still 100/100). **Pre-existing gap closed:** `completeFailure` now takes the executor `exec.Output` (mechanical-failure callers pass `exec.Output{}` — no-op there since an errored call carries no usage), so a step whose executor **succeeded and billed** but whose validation chain then errored (a judge under `on_error: fail`) now records the productive attempt's usage (`RetryStepArgs` gained `Usage`; `DeadLetterStepArgs` had one) and ledgers its productive cost on both the retry and DLQ branches — and the judge's own billed call rides its usage on `*validate.Error.Usage` and is metered too, so **every** judge spend is metered regardless of outcome. Judges are terminal (not a step, chain never recurses). Canonical `examples/definitions/llm_judge.json` (corpus-pinned). Tests: a full `internal/validate/llm_judge_test.go` unit matrix (pass/fail/boundary, structured + repaired answers, malformed under fail/skip, provider error under fail/skip, fallback chain, ctx-cancel passthrough, request-shape golden, the `CompileConfig` matrix), engine integration (fail→feedback→pass e2e with 2 productive + 2 overhead ledger rows and run-aggregate = ledger sum; on_error:fail meters productive+judge spend then DLQs; on_error:skip degrades to a pass with an `error` result and no overhead), and an api cost-overhead e2e. Two accepted limitations documented (ADR-013/010): judge calls bypass the M9 fleet limiter, and judge overhead is metered after the fact not pre-projected. ADR-013 §"LLM-judge validator (as built, 11.5)"; ADR-012 rule 4, ADR-009 flag table, ADR-010 amended; `docs/plugins.md` updated.)*
**Done when:**
- [x] Mock e2e: judge fails low-quality output → semantic retry → judge passes revision
- [x] Judge cost appears as overhead in the cost breakdown
- [x] Judge failure (provider error) handled per policy (skip-with-warning vs fail — configurable, tested)

#### 11.6 — Quality signals & metrics ✅
**Depends on:** 11.4, 7.2
Surface validation health: validation failure rate by step type/model/validator, semantic-retry depth histogram, repair rate, judge score distribution (bounded buckets). Status API exposes per-step verdict summaries. Grafana "output quality" panel. *(As built: five instruments under a new ADR-008 `validate` subsystem on the 7.2 registry — `engine_validate_verdicts_total{step_type,resource,status}` (pass/fail by step & resolved resource — the failure rate by step/model), `engine_validate_validator_results_total{validator,status}` (per-validator pass/fail/skipped/error), `engine_validate_semantic_depth_attempts{outcome}` (histogram 1..10, one per terminated loop under succeeded/validation_failed), `engine_validate_repairs_total{status}` (native/raw/repaired/unrepairable per productive output_format attempt), `engine_validate_judge_score_ratio{validator}` (histogram 0.1..1.0 — the judge score distribution); `resource` bounded to the pricing catalog + `none`, `validator` to the validator catalog. All recorded **post-commit** (the 10.5 cost-metric discipline — a fenced completion counts nothing) via `recordVerdictQuality`/`recordRepairQuality` in `completeSuccess` (which grew a `semanticAttempt` param — both callers pass it), `completeValidationFailure` (verdict+results+scores+repair on both semantic-retry and terminal routes; depth only at the terminal DLQ), and `completeFailure` (repair only — the 11.5 judge-under-`on_error:fail` shaped output). Two documented counting decisions: a cache-hit re-validation *fail* is a bypass+re-execute (fresh verdict counted, not the stale one), and a validator *transport* error is already visible via retries/dead-letters so gets no separate counter. Status API: `StepView.validation` (`ValidationSummaryView`+`ValidatorSummaryView`) — `summarizeVerdicts` rolls `attempts[].verdict` (ascending, last-wins, corrupt row skipped never a 500) into pass/fail counts + latest verdict + per-validator breakdown, present only for a validated step; OpenAPI `ValidationSummary`/`ValidatorSummary` (still 100/100), api now imports the `internal/validate` leaf in production. Grafana Engine **Output quality** row (verdicts, failure ratio by resource, validator results, semantic-depth p50/p95, repair share, judge score p50/p90) with the anti-drift audit green; dev-scale `ValidationFailureRatioHigh` alert (min-verdict-rate guarded) + promtool tests; `make smoke-metrics`/`smoke-dashboards` drive a mixed-quality offline workload (contains-pass, contains-fail→DLQ under a 3-attempt budget, output_format native echo, repair_only prose→unrepairable) asserting the series non-empty (judge-score panel allowlisted — the unscripted mock emits no parseable judge verdict; covered by `TestQualityMetricsJudgeScore` instead). No migration, no config var, no store change, no new outcome/class. ADR-013 §"Quality signals & metrics (as built, 11.6)"; ADR-008 amended. **M11 complete.**)*
**Done when:**
- [x] Metrics emitted per conventions; panel renders under a scripted mixed-quality load
- [x] Run status shows verdict summary per step (contract test)
- [x] ADR-013 updated with any semantics learned during implementation

---

## Milestone 12 — Context & memory management

**Goal.** Differentiator #4: token accounting (per-model counters), a run-scoped **blackboard** store (shared memory with versions, tags, pinning — also the substrate for M14 agent handoffs), declarative context assembly for steps, and automatic compaction when assembled context exceeds budgets — deterministic strategies plus LLM summarization, every application audited.

**Role.** Long multi-step workflows die on context windows without this. Compaction lives in the pre-execution path of the LLM executor, so *any* workflow benefits without bespoke code; provider-window guardrails make "context overflow 400s" structurally impossible.

**Architecture docs:** ADR-014 (context model: sources, budgets, compaction strategies, determinism & audit).

**Exit criteria:** a 20-step conversational fixture stays under a tight context budget via compaction (summaries visible on the blackboard, revisions audited) and completes with zero provider context-overflow errors.

#### 12.1 — ADR-014 & token counting ✅
**Depends on:** 8.3
**ADR-014:** context sources and precedence, pinning, per-step context budgets (default: model window − `max_tokens` − headroom), strategy pipeline semantics, determinism/audit requirements. `internal/tokens`: `Counter` per model family (tiktoken for OpenAI; Anthropic estimation with calibration factor, optional count-tokens API use flagged; chars/4 fallback), counts cached on stored artifacts. *(As built: the leaf package `internal/tokens` (imports `internal/llm` for the unified `ChatRequest`/`Message`/`Block`/`ToolDef` it counts, plus stdlib and tiktoken; no other agentloom package, so exec/engine and the coming contextmgr can depend on it without a cycle) — the `Counter` interface (`ID()`/`Count(text)`/`CountRequest(llm.ChatRequest)`, pure, no clock/network/log on the count path) and four families: **openai** (exact BPE via `github.com/pkoukk/tiktoken-go` with the `tiktoken-go-loader` `go:embed`'d offline rank tables set once via `SetBpeLoader` — no runtime download, proven by an `http.DefaultTransport` tripwire; encoding chosen by model prefix — `o200k_base` for gpt-4o/4.1/4.5/gpt-5/o1/o3/o4, `cl100k_base` for legacy gpt-4/gpt-3.5, defaulting o200k), **anthropic** (an estimate — Claude publishes no tokenizer — counting with the o200k BPE × a pinned `AnthropicCalibrationFactor`, rounded; the gated recorder re-derives the factor and a test guards >0.5% drift), **mock** (mirrors `internal/llm`'s mock provider input estimator `len(flattenPrompt)/4 + 1` EXACTLY, so a mock-driven request's counted total equals the mock's reported `Usage.InputTokens` — the property that lets M12's mock exit fixture run offline in CI with real accuracy assertions), and **fallback** (chars/4, ceil, ≥1). `CountRequest` frames the whole request (system as a framed message, per-message role framing, text/tool_use-input-JSON/tool_result-content blocks, per-tool name+description+schema+constant, structured-output schema, reply priming) — counting bare text would drift a multi-turn conversation past ±5%. `Registry.Select(provider, model)` (the resolved provider from `llm.Registry.Resolve` — routing is not re-implemented) returns the counter + a `Selection` (family/fallback), logging the chars/4 fallback **once per (provider, model) per process** (a `sync.Map` of `sync.Once`), never per call. Every counter carries a stable `ID()` fingerprint (`openai/o200k_base@1`, `anthropic/estimate@1;factor=1.15`, `mock/estimate@1`, `fallback/chars4@1`) so a stored `(token_count, counter_id)` is valid only against the counter that produced it (12.2/12.3 recompute on mismatch). Optional Anthropic `count_tokens` API use is a documented, off-by-default, not-yet-implemented seam — the default path is pure and offline. Two new deps (tiktoken-go + its offline loader, ~6.7 MB embedded BPE tables). **No migration, no config var, no engine/executor change** — 12.6 swaps the M9.2 chars/4 estimator for these counters. ADR-014 §Decision + worked compaction examples; ADR-012 cross-noted (12.6 adds `context_window` to the catalog).)*
**Done when:**
- [x] Accuracy tests vs recorded real counts within ±5% for both providers' fixtures *(OpenAI: green offline — tiktoken is OpenAI's real tokenizer, six framed fixtures within ±5% + hand-verified exact cases. Anthropic: needs one live `count_tokens`/Chat recording pass to populate the three `pending` fixtures and confirm the calibration factor — the gated `TestRecordTokenCorpus -record` does this; the accuracy assertion skips-with-notice until then.)*
- [x] Counter selection by model routing tested; fallback path logged once (not per call)
- [x] ADR reviewed with worked compaction examples

#### 12.2 — Blackboard store ✅
**Depends on:** 2.4
`blackboard_entries`: run-scoped key/value (JSONB or text), version history, `token_count`, tags (incl. `pinned`), author step, timestamps. `StepContext` read/write API; optional CAS on version for concurrent writers; step config sugar for declarative reads/writes. Retained post-run for audit (pruning in M21). *(As built: the leaf `internal/blackboard` (stdlib only — the `Entry`/`PutArgs`/`Board` types, key/tag/value rules, shared `CanonicalValue`/`ResolvePointer`) + the Postgres subpackage `internal/blackboard/pgboard` (the only importer of `internal/store`). Migration 0021 `blackboard_entries` is append-only per key (`PRIMARY KEY (run_id, key, version)`, GIN-indexed `tags`, `token_count`+`token_counter` stored together) + `run_steps.blackboard_policy`. `store.PutBlackboardEntry` is transition-style (run-lock → optional claim fence → CAS → insert → `blackboard_updated` event); a CAS conflict is `*BlackboardVersionConflict`→transient, a fence/bad-input is permanent. `StepContext.Blackboard` (programmatic reads/writes, attributed+fenced) bound in `claim.go`; entries token-counted via the new `exec.TokenCounterProvider` hook (llm executor resolves the model tokenizer, else chars/4 fallback). Declarative `blackboard.write` envelope block (`from` RFC-6901 pointer into the step's own output, `pinned` sugar) applied in the **completion transaction** after the success CAS. `blackboard_write` test executor (test-only set). `GET /v1/runs/{id}/blackboard` (read scope, heads/history, key/tag filters, keyset pages) + `ctl blackboard`. Canonical `examples/definitions/blackboard.json` (offline mock). **Declarative reads into a prompt are 12.3's context sources** — 12.2 ships writes + the programmatic read/write API; the `blackboard.<key>` template root is deferred with them. One migration, no new config var, no new metric. ADR-014 §"Blackboard store (as built, 12.2)"; ADR-004/003/006/007 cross-amended.)*
**Done when:**
- [x] Store integration tests: versioning, tag queries, CAS conflict path
- [x] Two parallel steps writing distinct keys + same key (CAS) behave per ADR
- [x] Blackboard contents exposed via `GET /v1/runs/{id}/blackboard` (read scope)

#### 12.3 — Context assembly ✅
**Depends on:** 12.2, 12.1, 8.6
Steps declare `context` spec: ordered sources (explicit step outputs, blackboard selectors by key/tag, retrieval results), per-source caps, pinned entries always included, assembly into the provider message list with a computed pre-flight token total. Deterministic given store state. *(As built: `context` is a step-envelope block (`dag.Step.Context`, llm-family only, materialized onto `run_steps.context_policy` — migration 0022 — like `blackboard_policy`); the leaf `internal/contextmgr` `Assemble(spec, counter, Sources)` resolves each source in declaration order (precedence = message order), renders it into a stable `<context name=… kind=…>…</context>` block, truncates a per-source `max_tokens` cap on a rune boundary with a `…[truncated]…` marker (binary search over the final string — BPE counts are not additive), never truncates a `pinned` source, and counts everything with the step's counter — deterministic given store state. Missing-source policy is `on_missing: error` (default, permanent before any provider call) vs `skip` (omit + record on the audit event); an unwired/unknown backend is a permanent config error regardless of policy; a transport read failure redelivers. Two optional `LLMExecutor` hooks — `ContextInjector.WithContext` (prepend a leading user message / prompt prefix, the `WithFeedback` re-key mirror, so the assembled context is a cache-key input by construction) and `PreflightCounter.PreflightTokens` (count the whole framed request, returning an int so the engine never imports `llm`). The engine stage (`engine/context.go`) sits after feedback injection and before the cache read, builds the readers (one batched step-output read, `sc.Blackboard`, `engine.WithRetrievers` registry), and appends a `context_assembled` event in its own fenced tx (the `RecordModelDowngrade` precedent — counter fingerprint, per-source disposition included/truncated/skipped, context + pre-flight tokens). On the mock, the recorded `preflight_tokens` equals the attempt's reported input tokens exactly. `classifyUpstreamRef` extracted and shared with the 8.2 template lint. No templating inside the block (a dynamic query flows through a `retrieve` step + `step_output` source). Canonical `examples/definitions/context_assembly.json`; kitchen-sink construct pin (all four kinds + pinned + cap + skip); one migration (0022), no new config var, no new metric — the `context_utilization` histogram is 12.6. ADR-014 §"Context assembly (as built, 12.3)"; ADR-003/004/006/011 cross-amended.)*
**Done when:**
- [x] Golden tests: same store state ⇒ byte-identical assembly *(contextmgr golden + determinism unit tests, and the engine `TestContextAssemblyDeterministic` — two runs on the same store state produce byte-identical echoed output and equal context tokens.)*
- [x] Missing-source policy (error vs skip-with-event) implemented per ADR and tested *(default `error` → permanent DLQ before any provider call, `skip` → omitted + a `skipped` source on the `context_assembled` event; the per-kind matrix is covered in `contextmgr` and the engine `TestContextMissingSourceErrorDeadLetters`.)*
- [x] Assembled token total within counter accuracy of provider-reported prompt tokens (fixture check) *(exact on the mock: the recorded `preflight_tokens` equals the draft attempt's reported `Usage.InputTokens` — the mock counter mirrors the mock provider's estimator — asserted in `TestContextAssembly`.)*

#### 12.4 — Deterministic compaction strategies ✅
**Depends on:** 12.3
When assembly exceeds the step budget, apply the configured strategy pipeline: `drop_lowest_priority`, `truncate_oldest` (middle-out truncation with markers), `sliding_window` (last-N messages). Each application writes a `context_revision` event (what was dropped/kept, tokens before/after). Hard guarantee: post-pipeline ≤ budget or typed failure. *(As built: `budget_tokens` + an ordered `compaction` pipeline + a per-source `priority` on the same `context` envelope block (`dag.ContextSpec`, materialized on the existing `run_steps.context_policy` — **no new migration**); the three strategies in `internal/contextmgr/compact.go` (`summarize` reserved-and-rejected until 12.5). The budget is measured over the **whole framed request** (the engine's `PreflightCounter`, the same arithmetic 12.6 guards), not the preamble alone, and re-measured each pass because BPE counts are not additive. The compaction unit is a source (1:1 with a rendered `<context>` block), so `contextmgr.Render` reproduces 12.3's byte-identical preamble and the goldens survive. Pinned sources are untouchable across every strategy; on failure `Compact` returns a typed `*OverBudgetError` (with a `PinnedOnly` flag) the engine turns into a **permanent** step failure before any provider call (ADR-006 row 21) — overflow is never sent. Each strategy that runs appends a `context_revision` event (`store.ContextRevisionEvent`) in the same fenced tx as the extended `context_assembled` event (raw → revision* → assembled in seq order), queryable per step attempt. Canonical `examples/definitions/context_compaction.json` (offline mock, five turns + a budgeted summary). ADR-014 §"Deterministic compaction (as built, 12.4)" + worked examples; ADR-003/004/006 cross-amended. No new config var, no new metric — `context_utilization` is 12.6.)*
**Done when:**
- [x] Property test: for arbitrary oversized assemblies, pipeline output ≤ budget or typed error *(`internal/contextmgr` rapid `TestCompactProperty`: random entries/pins/priorities × random pipelines → `FinalTokens ≤ budget` or `*OverBudgetError`.)*
- [x] Pinned entries never dropped (asserted across strategies) *(same property test asserts every pinned entry survives byte-identical; per-strategy unit tables + `TestContextCompaction` assert the pinned source stays `included`.)*
- [x] Revisions queryable per step attempt (audit API or events — contract test) *(`context_revision` events carry `step_id`+`attempt`; `TestContextCompaction` reads them back per attempt, asserting order, non-increasing tokens, and a final ≤-budget result.)*

#### 12.5 — Summarization compaction ✅
**Depends on:** 12.4, 10.2
`summarize` strategy: evicted spans summarized by a cheap model into blackboard summary entries (chained summaries supported), cost attributed as overhead, results cached (deterministic prompt at `temperature=0`), pinned exemption honored. Failure of the summarizer falls back to the next deterministic strategy (never blocks the step). *(As built: `summarize` is the fourth compaction strategy, un-reserved on the `context` envelope block (`dag.CompactionStrategy` grew `model` (required, routed like a step's model), `key` (default `context_summary`), `max_tokens` (default 256), `timeout` (default 60s, ≤ 10m) — no migration, materialized on `run_steps.context_policy`). New `internal/contextmgr/summarize.go`: the `Summarizer` interface + the concrete `LLMSummarizer` (one temperature-0 `Chat` under a per-call timeout, caller-ctx cancel passed through unwrapped) + the deterministic `SummaryChatRequest` (fixed system prompt, span as a leading user turn, fixed instruction trailing — the mock echoes the short instruction, so the offline fixture shrinks without scripting). In `compact.go` the strategy gathers the oldest non-pinned live entries oldest-first until their token sum reaches `over + max_tokens`, calls the summarizer once over the rendered span, writes a versioned `SummaryValue` to the blackboard (`parent_version` = prior head → the key's version history **is** the chain), drops the span (disposition `summarized`), and splices a synthetic `summary`-kind entry into the assembly at the span's oldest slot (so it re-renders in place and may be folded again — chaining, both within a pipeline and across steps that read the running summary via a `blackboard` source). A summary commits only if genuinely smaller than its span (progress guard). Pinned exemption is honored; the property test now includes summarize (still `FinalTokens ≤ budget` or `*OverBudgetError`, pins byte-identical). Determinism is restored operationally: the fixed temp-0 prompt is cached under ADR-011 — the engine's `cachedSummarizer` (`engine/summarize.go`) keys the deterministic request under a dedicated `context_summarizer` namespace at `SummarizerVersion`, global scope, fail-safe on every cache error, reusing the `engine_cache_*` metrics under a bounded `summarizer` label; a repeated compaction of the same span is a cache hit ($0 saved overhead row). Cost is **overhead ledgered before the call**: `store.CompactionEntry(i)` = `compaction:<i>` (the 0016-reserved slot), priced by `priceSummaryOverheads` (cache-hit → $0 + saved), applied **inside the fenced `RecordContextAssembled` tx** (and the new `RecordContextRevisions` for the over-budget path) — under the claim fence, before the step's own provider call — so summarizer spend is metered even if the step later fails/retries (the 11.5 judge discipline; also closes 12.4's "failed-attempt revisions unpersisted" gap). **A summarizer failure never blocks the step** (ADR-006 row 21 note): model unavailable / provider error / timeout / non-shrinking summary → fall back to the next deterministic strategy + a warning on the `context_revision` (`error` field); only a caller-ctx cancel is fatal (redeliver). `ContextRevisionEvent` grew `summaries[]`/`error`; `ContextAssembledEvent` grew `summaries`; the assembled `sources` gained the `summarized` disposition + a synthetic `summary` source. `cmd/worker` wires `engine.WithSummarizer(contextmgr.NewLLMSummarizer(providers))`. Canonical `examples/definitions/context_summarization.json` (offline mock, sixteen turns + four rollups → four chained `context_summary` versions). Both kitchen sinks + the construct pin gained a summarize strategy; `context_bad.json` grew five summarize validation cases. Tests: `internal/contextmgr` summarize unit matrix (shrink/chain/fallback/ctx-cancel + `SummaryChatRequest` golden) + the extended property test; engine integration `TestContextSummarization` (four chained versions, under budget, overhead ledgered), `TestContextSummarizationCacheHit` (second run's `compaction:*` rows are $0 saved cache hits), `TestContextSummarizerFallsBack` (unroutable model → deterministic fallback + warning revision, no summary, no overhead). **No migration, no new config var, no new metric** (`context_utilization` is 12.6). ADR-014 §"Summarization compaction (as built, 12.5)" + worked example; ADR-012 rule 4, ADR-011, ADR-006 row 21 cross-amended.)*
**Done when:**
- [x] 20-step conversational fixture (mock) stays under budget; summaries visible and chained *(the 20-step `context_summarization.json` — sixteen turns + four rollups — runs offline; `TestContextSummarization` asserts every rollup's framed request ≤ budget and four chained `context_summary` versions with `parent_version` links.)*
- [x] Summarizer cost flagged overhead; cache hit on repeat compaction (asserted) *(each summarization is a `compaction:<i>` `overhead: true` ledger row priced at `mock:cheap`; `TestContextSummarizationCacheHit` runs the identical conversation twice against one shared cache and asserts the second run's `compaction:*` rows are `cache_hit` $0 saved rows.)*
- [x] Summarizer failure falls back deterministically with a warning event *(`TestContextSummarizerFallsBack`: an unroutable summarizer model → the `summarize` revision is an unchanged fallback carrying an `error`, the next deterministic strategy runs, the step completes under budget, and no summary/overhead is written.)*

#### 12.6 — Provider window guardrails ✅
**Depends on:** 12.4, 9.2
Registry of model context windows (in the pricing/model catalog); pre-flight hard check (assembled + `max_tokens` ≤ window) → auto-compact, or typed failure if compaction disabled/insufficient; `context_utilization` histogram metric; refines M9.2's token estimator with real counts.
**Done when:**
- [x] Oversize scenario auto-compacts; with compaction disabled → typed error, no provider call *(`context_window` on the ADR-012 catalog defaults a context-bearing step's budget from the window — `budget_source: window` on the `context_assembled` event — so `context_window.json` (no explicit `budget_tokens`) auto-compacts to fit `mock/small`'s 1024 window; `TestContextWindowExceededDeadLetters` shows a context-less oversize step fail permanently before any provider call.)*
- [x] E2E suite shows zero provider context-overflow errors by construction *(the `guardWindow` engine stage — after budget/downgrade, before the limiter — hard-fails `preflight + max_tokens > window` before any call; `TestNoProviderContextOverflowByConstruction` runs a window-enforcing mock and proves the engine's typed `context_window_exceeded` fires first, never the provider's overflow.)*
- [x] Utilization histogram visible in Grafana; estimator error metric improves (before/after captured) *(new `context` subsystem `engine_context_utilization_ratio` + `engine_context_window_rejections_total`, a Grafana Engine **Context** row, and the estimator swapped from `chars/4` to the real `internal/tokens` counter — `TestEstimatorErrorImproves` captures aggregate abs error `chars/4=2` vs `counter=0` on the mock; `engine_ratelimit_estimate_error_tokens` tightens to zero.)*

---

## Milestone 13 — Dynamic DAG: planner steps & runtime expansion

**Goal.** Differentiator #2: workflows whose graphs grow at runtime. A `planner` step's validated output injects new steps/edges into the *running* graph — atomically with the planner's completion, fully durable, crash-safe at every boundary, and guarded by hard caps. Plus `map` fan-out (runtime-sized parallel instances) built on the same expansion primitive.

**Role.** The hardest correctness milestone — the reason M2 chose per-run graph copies and M4 chose the outbox. Planner output is just another validated LLM output, so M11's semantic retries automatically repair malformed plans: the differentiators compound.

**Architecture docs:** ADR-015 (dynamic graph semantics: expansion operation, validation & caps, versioning, recovery invariants).

**Exit criteria:** planner fixture expands the graph mid-run and completes; kill-at-every-boundary chaos matrix green; caps halt runaway planners; run graph API returns versioned provenance.

#### 13.1 — ADR-015 & expansion operation spec ✅
**Depends on:** 2.3, 11.1
**ADR-015:** `PlanOutput` schema (new nodes with full configs, new edges splicing at named anchor points, join handling), expansion validation (whole-graph revalidation under M1 rules incl. cycle check, plus caps: `max_added_steps` per expansion and per run, `max_depth`, `max_expansions`), `graph_version` increments, rejected plans = `validation_failed` (semantic-retry the planner), and the crash matrix: expansion applies **only** inside the planner's completion CAS transaction — a crashed pre-commit attempt re-executes (possibly via cache) and only one attempt can ever commit; zombies fenced. UI implications (layout of injected nodes) noted for M18.
**Done when:**
- [x] Crash matrix proves no lost/duplicated expansions for every boundary *(ADR-015 §"Crash matrix" tabulates E1–E7 across the 13.5 boundaries — pre-claim, mid-LLM, pre-commit, post-commit-pre-ACK, post-commit-pre-dispatch, zombie, Redis-loss — each reducing to "did the completion transaction commit?", proven by argument from the existing ADR-005 W1/W2/W4/P1/P2 + 4.5 fencing invariants; automated kill-at-every-boundary runs land in 13.5.)*
- [x] `PlanOutput` JSON Schema published (drives planner validators and UI) *(`dag.PlanOutput`/`DecodePlanOutput` + `docs/schema/plan-output.v1.json`, generated by `make generate` from the same structs and drift-checked in CI beside the definition schema, reusing its step-config `$defs` so an injected step's config cannot drift from an authored step's.)*
- [x] Cap semantics and rejected-plan flow documented with examples *(ADR-015 §"Caps" + §"Rejection routing"; the caps ship as `dag.ExpansionPolicy`/`ExpansionCaps` (top-level `expansion` block, defaults 32/10k/100/4) with submit-time bounds validation, and the pure `dag.ValidateExpansion` splits plan-attributable rejections → `validation_failed` (semantic retry) from run-guard exhaustion → permanent `expansion_cap_exceeded`, with a full unit matrix.)*

#### 13.2 — Graph mutation in the store ✅
**Depends on:** 13.1, 2.6
`ExpandRun(tx, runID, delta)`: insert `run_steps`/`run_edges` with `graph_version++`, revalidate graph invariants and caps in-tx, recompute `remaining_deps` for spliced frontier, write outbox rows for newly-ready injected steps, append `graph_expanded` event with the delta. Concurrent expansions (parallel planners) serialize via row locking on the run.
**Done when:**
- [x] Concurrent-expansion test: two planners commit sequentially; final graph valid; versions ordered *(`TestExpandRunConcurrent`: two goroutines expand one run at once, serialize on the run lock, come out `graph_version` 2 then 3 with `steps_total` the sum and the two `graph_expanded` events stepping the versions linearly in seq order; `TestExpandRunIDCollisionRejected` proves the loser of a same-id race is rejected.)*
- [x] Cap violation aborts the tx atomically (planner attempt fails with typed error) *(`ExpandRun` returns a typed `*store.ExpansionRejectedError` — `CapExceeded()` true for a run-guard exhaustion — and the caller's error return rolls the whole transaction back; `TestExpandRunCapExceeded` (max_expansions) and `TestExpandRunDepthPropagates` (max_depth) assert the injected rows never landed and `graph_version` did not move, and `TestExpandRunRejectionRollsBackCompletion` composes `SucceedStep` + a rejected `ExpandRun` in one tx and shows both roll back — the origin stays `running`, no `graph_expanded` event.)*
- [x] Injected-step readiness correct across splice patterns (before/after/parallel-to anchors) *(`TestExpandRunAfterSplice` — `origin → new` inserted `pending`/1-dep, readied by the emulated fan-out; `TestExpandRunBeforeSplice` — `new → existing-pending` inserts a zero-indegree `new` `ready`+outboxed and widens the anchor's `remaining_deps`; `TestExpandRunParallelSplice` — `existing-active → new → existing-join` inserts `new` `pending`/1 and grows the join's `remaining_deps`; `assertCountersMatchEdges` confirms the two ADR-004 bookkeeping representations agree.)*

#### 13.3 — Planner step executor ✅
**Depends on:** 13.2, 11.4
`planner` step: an LLM call whose output must parse/validate as `PlanOutput` (M11 validators; semantic retries repair bad plans); completion transaction = persist output + CAS complete + `ExpandRun` + outbox, atomically. Planner output also stored as normal step output (audit/UI provenance).
**Done when:**
- [x] Mock planner e2e: plan → injected steps execute → downstream join continues → run succeeds *(`exec.PlannerExecutor` is the llm executor re-targeted — `llmConfigView` projects `PlannerConfig` onto an `LLMConfig` carrying an implicit plan `output_format`, so every llm hook (routing, cache, limiter, cost, feedback, context, window guard) is reused; `completeSuccess` composes `store.ExpandRun` after the fenced `SucceedStep` and before the out-edge fan-out (moved under the run lock for every completion). `TestPlannerExpandsAndCompletes`: a plan splices two workers after the planner and into a pre-existing join, they execute, the join continues, the run succeeds at `graph_version` 2 / `steps_total` 5, one `graph_expanded` event, injected rows carrying `depth`/`origin_step`/`origin_kind` provenance.)*
- [x] Malformed plan → semantic retry with verdict feedback → valid plan (scripted mock) *(a planner carries an implicit `json_schema` validator over the published `plan-output.v1.json` (JSON-shape gate) and `dag.ValidateExpansion` inside `ExpandRun` (graph gate); a plan-attributable rejection returns `*store.ExpansionRejectedError`, routed through `completeValidationFailure` as a synthesized `expansion`-validator fail verdict → 11.4's semantic-retry loop re-prompts with the issues as feedback. `TestPlannerMalformedPlanSemanticRetries`: an id-collision plan → attempt 1 `validation_failed` (verdict carries the expansion issue) → feedback-augmented attempt 2 valid → run succeeds, `graph_version` moved exactly once. A run-guard cap exhaustion (`CapExceeded`) instead fails permanent — `TestPlannerCapExceededPermanent` dead-letters `expansion_cap_exceeded`, no retry, graph unchanged.)*
- [x] Zombie planner attempt (reclaimed mid-flight) cannot double-expand (fencing test) *(`TestZombiePlannerCannotDoubleExpand`: worker A stalls past its lease, worker B takes over and expands (`graph_version` 2), A resumes and its completion — `ExpandRun` included — is fenced on the lost claim at `SucceedStep` and abandoned without ACK (both claim ids logged); the graph stays at version 2, one `graph_expanded`, planner history `[lost, succeeded]`. `TestPlannerExpansionAtomicOnFailpoint` proves E3: an abort right after `ExpandRun` leaves no injected rows and `graph_version` unmoved.)*

#### 13.4 — Dynamic map fan-out ✅
**Depends on:** 13.2
`map` step: over a runtime list (templated from an upstream output), instantiate N instances of a named sub-template + a gather join collecting ordered results; implemented as an engine-generated expansion (no LLM involved); N capped by config.
**Done when:**
- [x] E2E: retrieve list of 20 → map llm-per-item (mock) → gather returns ordered array *(a `map` step over a runtime list instantiates one instance of its `body` sub-template (the definition `templates` library) per item plus a generated `gather` join, applied through `store.ExpandRun` — the planner's primitive with an engine-generated delta. `dag.GenerateMapExpansion` mints `<body_step>#k` instances (item/`item_index`/internal-step refs rewritten per instance), after-splices `map → entry#k`, wires `sink#k → gather`, and gives the gather an ordered `["${{ steps.<sink>#k.output }}", …]` config so it emits results in list order. `TestMapFansOutAndGathers`: a 20-element list → 20 `analyze#k` llm instances (mock) → `process#gather` emits the ordered 20-element array; run succeeds at `graph_version` 2 / `steps_total` 23, one `graph_expanded` (origin process/map), instance rows carrying `depth` 1 / `origin_step` process / `origin_kind` map.)*
- [x] N > cap rejected with typed error at expansion time *(two caps: `MapConfig.max_items` (executor, before any expansion — `TestMapMaxItemsPermanent`) and the run per-expansion step cap (`ValidateExpansion` inside `ExpandRun` — `TestMapCapExceededPermanent`). Both dead-letter the map **permanently** — an engine-generated delta has no model to re-prompt, so `routeMapRejection` never semantic-retries: `CapExceeded` → `expansion_cap_exceeded`, else `map_expansion_invalid`. The graph never moves and no `graph_expanded` fires.)*
- [x] Item failures honor the map's failure policy — **fail-fast** *(an instance whose llm call fails permanently dead-letters, and under the default `fail_fast` the run fails before the gather gathers — `TestMapItemFailureFailFast`. **collect-errors split into 13.4b below** — it needs the gather to fire on all-terminal, not all-succeeded, which is new engine readiness.)*

#### 13.4b — Map collect-errors policy ✅
**Depends on:** 13.4
`map` failure policy `collect_errors`: an item failure is collected as an error slot in the gathered ordered array and the run still succeeds — the alternative to 13.4's fail-fast. Needs new readiness semantics (the gather fires when all instances are **terminal** — succeeded OR dead-lettered — not all-succeeded) plus error-slot collection and a `MapConfig.on_item_failure` field. *(Carved out of 13.4 to keep that ticket a focused session; ADR-015 §"Map fan-out (as built, 13.4)" records the split.)*
**Done when:**
- [x] `on_item_failure: collect_errors` gathers succeeded outputs + error markers for failed items, run succeeds (tested) *(a failed instance is settled to the new terminal status `collected` — migration 0024, distinct from `dead_lettered` for honesty — with an engine-synthesized error marker `{"map_item_failed": true, "class": …}` stored as its output; `renderConfig` was widened to include `collected` outputs so the mode-agnostic gather's strict `${{ steps.<sink>#k.output }}` ref resolves to the marker. `TestMapCollectErrorsSucceeds`: `[ok1, boom, ok3]` → `analyze#1` collects → the gather emits `[ok1-output, {marker}, ok3-output]` in order and the run **succeeds** at `steps_collected` 1 / `steps_failed` 0, no DLQ row; the tolerated attempt still records its real `permanent` class.)*
- [x] Gather readiness fires on all-instances-terminal, not all-succeeded (tested) *(`CollectFailStep` bumps `steps_collected` not `steps_failed`, and `completeFailure`'s terminal branch — for a collect-errors instance, detected from the origin map's config — fires the instance's unconditioned `sink#k → gather` edge via the ordinary success-style fan-out, so the gather's normal all-mode counter readies it once every instance is terminal. `SucceedRun`/`FailRunRollup`/`CancelRunRollup` all-terminal sums widened with `+ steps_collected`, so success still requires `steps_failed = 0` — `collected` never blocks it. `TestMapCollectAllFail`: every item fails → all `collected` → the gather emits an all-error array → the run still succeeds.)*
- [x] fail-fast (13.4) and collect-errors selectable per map, both covered *(`MapConfig.on_item_failure` (`fail_fast` default | `collect_errors`); `TestMapItemFailureFailFast` (13.4) proves fail-fast still fails the run, `TestMapCollect*` prove collect-errors. collect_errors is validation-restricted to single-step bodies (a multi-step body is `config_field_invalid` — a mid-body failure would strand the sink; deferred).)*

#### 13.5 — Expansion chaos & recovery matrix ✅
**Depends on:** 13.3, 5.8
Targeted chaos: kill workers at each 13.1 boundary (pre-claim, mid-LLM, pre-commit, post-commit-pre-dispatch) across repeated runs; assert graph consistency (single expansion, valid invariants) and eventual completion. Fuzz the expansion validator with generated deltas.
**Done when:**
- [x] Kill-at-boundary matrix automated and green in CI (short mode) *(a test-gated engine crash seam (`engine/crashpoint.go`, `AGENTLOOM_WORKER_CRASH_POINT=<stage>:<step_id>`) hard-exits a real `cmd/worker` at each boundary — `pre_claim` (E1), `pre_completion` (E2), `after_expand` (E3), `post_commit` (E5) — the faithful SIGKILL a parent-process kill can only land at a random instruction; the two in-transaction boundaries (E3/E5) were previously unreachable from outside the process, the `ConsumerConfig.PhaseHook`/`completeFailpoint` precedent. `TestExpansionKillAtBoundaryMatrix` (`test/crash`) runs the offline planner per boundary × repeated runs: one armed worker crashes (asserted exit 137), an unarmed survivor fleet recovers via reclaim + the transactional outbox (reconciler out at 10m), and the run completes. Inert without the env and gated to test-executor mode — no production behavior change, no migration, no config var, no metric. E6 (zombie double-expand) stays the in-process 13.3 fencing test.)*
- [x] Fuzzer finds no validator panics/acceptance-of-invalid over corpus run *(`FuzzValidateExpansion` (`internal/dag`, DB-free leaf) feeds bytes through `DecodePlanOutput` — the production gate — then asserts (1) `ValidateExpansion` never panics and (2) no acceptance-of-invalid: when OK, an independent oracle (`NewGraph`+`TopoOrder` over the merged graph, sharing none of the validator's code path) confirms injected ids are unique/collision-free, every edge endpoint resolves, and the merged normal-edge graph is acyclic. 1M+ executions clean; seed corpus spans accept/reject shapes. `make test-expansion-chaos` runs the matrix + a bounded `-fuzz`.)*
- [x] Post-chaos quiescence: `graph_version` history linear, no orphan steps *(every boundary asserts exactly one committed expansion — `graph_version` 2, `steps_total` 5, one `graph_expanded` event stepping 1→2 with no gaps/duplicates (`requireGraphVersionLinear`) — and every step (injected included) terminal `succeeded` with `step_succeeded` firing exactly once (no double-execution/double-expansion), plus outbox-empty + `WaitQuiescent`.)*

#### 13.6 — Run-graph introspection API ✅
**Depends on:** 13.2, 6.5
`GET /v1/runs/{id}/graph`: current versioned graph with provenance (origin: definition | planner-step-X | map-step-Y, added-at version/time) and per-version deltas — the contract the dashboard uses to animate expansions (M18.2). Fixtures exported for frontend tests.
**Done when:**
- [x] Graph endpoint returns full provenance; delta list reconstructs any version *(`GET /v1/runs/{id}/graph` (read scope/class) returns `nodes`/`edges` — each with `origin` (definition | planner | map | loop, plus the injecting `step`), `depth`, the `graph_version` it was introduced at, and `added_at` — plus `expansions`, the ordered per-version delta feed. Both reconstruction paths hold: nodes/edges carry `graph_version` so a client keeps rows with `graph_version ≤ N` to reconstruct version N; `expansions` replays base-graph + ordered deltas. Nodes/edges come from the row provenance columns (13.2); deltas from the `graph_expanded` events via the new bounded `ListEventsByType` store read. No migration, no new config var, no new metric.)*
- [x] Contract test + committed fixture for frontend consumption *(`TestRunGraphContract` asserts the pure `buildRunGraphResponse` projection against the committed golden `internal/api/testdata/run_graph_fixture.json` — the exported fixture the M17/M18 frontend tests consume; regenerate with `UPDATE_GOLDEN=1`. OpenAPI `getRunGraph` + the `RunGraphResponse`/`GraphNodeView`/`GraphEdgeView`/`GraphOriginView`/`GraphExpansionView`/`GraphEdgeRef` schemas (spec lints 100/100; `TestOpenAPIRouteCoverage`/`TestOpenAPIOperationContracts` green).)*
- [x] `graph_expanded` events and API versions agree (consistency test) *(`TestRunGraphVersionConsistency` asserts `graph_version = 1 + len(expansions) = run.graph_version`, every node's introducing version is 1 or belongs to an expansion, the row-derived and event-derived added sets coincide per version, and the delta feed is linear (`from_version[i] = to_version[i-1]`). The `TestRunGraphIntrospection` integration test drives `planner.json` to completion and re-checks the same invariants on a real expansion — injected workers carry `origin.kind=planner`/`depth=1`/`graph_version=2` — plus the 404-on-missing-run case.)*

---

## Milestone 14 — Multi-agent orchestration

**Goal.** Differentiator #5: workflows modeled as multiple agents with distinct roles (researcher, critic, writer) handing off via the blackboard, including revision loops — critic rejects the writer's output and sends it back with feedback, bounded and durable. Loops execute by **unrolling**: each iteration is a fresh set of step instances created through M13's expansion machinery, so the run graph stays acyclic and every iteration is durably checkpointed.

**Role.** This composes nearly everything before it — agent steps are LLM steps (M8) with validators (M11), context specs (M12), budgets (M10), and loop unrolling (M13). The flagship example ships here.

**Architecture docs:** ADR-016 (agent model, handoff contract, loop unrolling & termination).

**Exit criteria:** the research→write→critique loop fixture runs to convergence under scripted mock rejections, iteration cap enforced, thread visible on the blackboard; guards halt a runaway loop.

#### 14.1 — ADR-016 & agent definitions ✅
**Depends on:** 8.6, 12.3, 1.1
**ADR-016** + schema: `agents:` section in the workflow definition (role name, system prompt, model + fallbacks, allowed toolset, default validators, default context spec); `agent` step type references an agent and merges step-level overrides over agent defaults. Validation: unknown agent refs, tool allowlist enforcement.
**Done when:**
- [x] Agent defaults merge deterministically (override precedence tested)
- [x] `agent` step executes as a fully-configured LLM step (e2e with mock)
- [x] Tool calls outside the agent's allowlist rejected with typed error

#### 14.2 — Handoff conventions & message thread ✅
**Depends on:** 14.1, 12.2
Standardized blackboard usage: agent outputs auto-appended to a `thread` (author, role, iteration, timestamp); per-agent context presets ("conversation view": thread + role-filtered entries + pinned facts); explicit handoff payloads (structured output of one agent as pinned input to the next).
**Done when:**
- [x] Two-agent relay e2e: researcher findings automatically visible to writer via thread preset
- [x] Thread entries carry author/role/iteration metadata (asserted)
- [x] Context preset respects compaction (long thread compacts, pinned handoff survives — test)

#### 14.3 — Loop-edge runtime (unrolling) ✅
**Depends on:** 14.2, 13.2
Execute M1's marked loop edges: when the loop-source step (critic) completes and its CEL condition signals "revise" with iteration < `max_iterations`, the engine expands iteration k+1 — cloning the loop-body segment as new step instances (`{node_id}#k+1`), placing the critic's feedback on the blackboard for the new iteration's context; condition false or cap reached → the loop-exit edge proceeds. All via `ExpandRun` (atomic, durable, crash-safe for free).
**Done when:**
- [x] Writer⇄critic e2e: mock critic rejects twice then accepts → 3 writer instances, feedback threaded into each revision prompt
- [x] Cap reached → exit path taken with `loop_exhausted` event (policy: proceed vs fail — both tested)
- [x] Kill mid-iteration → resume completes the same iteration (no duplicate iteration)

#### 14.4 — Run guards & termination policies ✅
**Depends on:** 14.3, 5.6
Run-level guards evaluated at expansion and claim time: `max_total_steps`, `max_expansions` (13.1), `max_wall_clock` (5.6), budget (10.3) — plus loop-specific no-progress detection (optional: identical output hash across consecutive iterations → force exit with event). Every halt is a typed park/fail with an explanatory event.
**Done when:**
- [x] Runaway-loop fixture halted by each guard class in isolation (test per guard) *(`guard_integration_test.go`: a runaway writer⇄critic loop halted by `max_total_steps` and `max_expansions` in isolation → the loop source dead-letters `expansion_cap_exceeded`, run fails, `guard_tripped` fired; `max_wall_clock` → the deadline crosses mid-loop (manually driven on a fake clock), the next claim's `guardDeadline` cancels the run reason `deadline_exceeded`; the budget guard keeps its 10.3 `budget_exceeded` path. Wall-clock is now enforced at claim time (new `engine/guard.go` stage), not only by the reconciler sweep.)*
- [x] No-progress detector fires on scripted identical outputs; disabled by default (opt-in) *(loop-edge `no_progress {step?, path?, policy?}`, nil by default. `TestLoopNoProgressProceedExits`/`FailDeadLetters`: identical consecutive drafts → `loop_no_progress` event at iteration 1 → proceed routes the exit (run succeeds) or fail dead-letters the loop source; `TestLoopNoProgressDisabledByDefault`: the same identical-output loop with no guard runs to `loop_exhausted`, never firing a no-progress event. Purely additive — an unresolvable pointer/unreadable prior instance skips the check, never a new failure.)*
- [x] Guard events explain which limit, current value, and configured cap *(`guard_tripped` (expansion caps + wall clock) carries `{guard, current, cap, unit, action}`, rendered from `dag.ExpansionVerdict.Breaches []CapBreach{Limit, Current, Cap}` for the caps and elapsed-vs-deadline seconds for the wall clock; `loop_no_progress` carries the compared step/pointer/hash + policy/action; the budget and iteration guards keep their richer `budget_exceeded` / `loop_exhausted` events. No migration, no new config var, no new metric.)*

#### 14.5 — Flagship example: research → write → critique ✅
**Depends on:** 14.3, 8.8, 11.4
`examples/definitions/research-critic-writer.json`: researcher (retrieve + llm) → writer → critic loop (validators + judge) with budgets, compaction, and fallback models; runnable in mock mode (CI) and live mode (env-gated). Narrative doc walking through the event history, cost breakdown, and loop iterations.
**Done when:**
- [x] Runs green in CI (mock); live mode documented and smoke-tested once *(`examples/definitions/research-critic-writer.json` — a `search` (retrieve) → `researcher` → `writer` → `critic` loop → `editor` → `publish` pipeline composing agent roles (14.1), the handoff thread (14.2), loop unrolling (14.3), guards (14.4), `llm_judge`+semantic-retry feedback (11.4/11.5), `budget_usd`+`model_fallbacks` (10.3/10.4), and a `context` preset with `summarize` compaction (12.4/12.5) — no new engine code. `TestFlagshipResearchCriticWriter` (`internal/engine/flagship_integration_test.go`, offline scripted mock) runs it to convergence: the loop unrolls twice (`draft`/`draft#1`/`draft#2` at `graph_version` 3, loop provenance/constant depth), the judge fails the first draft then a semantic retry passes it (`draft` history `[validation_failed, succeeded]`), feedback threads into each revision, the judge's calls are ledgered as `judge:*` overhead, and the run aggregate == the exact ledger sum. Live mode is `TestFlagshipLive` — `LIVE_LLM_TESTS=1` + an Anthropic key rewrites the `mock/*` ids to Sonnet/Haiku and asserts a published article; skipped in CI, the `internal/llm` live-smoke precedent. The env-scripted mock (`AGENTLOOM_LLM_MOCK_SCRIPT[_FILE]`, `llm.ParseMockScript`) is what makes the loop iterate on the compose stack — the stock echo mock never returns a `revise` verdict.)*
- [x] `docs/examples/research-critic-writer.md` narrates events, costs, iterations with real output *(the narrative walks the run through the watch tree, the 8-entry handoff thread (author/role/iteration), the two `graph_expanded` expansions, the cost breakdown with judge overhead (`spent_usd 0.010972`, `judge:*` rows), and the event highlights — all captured from `make demo-research` (`scripts/demo-research.sh`: boots compose with the scripted mock, seeds the corpus, submits, watches, prints thread/graph/cost/events + a passing receipt). Includes the mechanism map, the guards, and the live-mode instructions. The exit-target-reference quirk (a post-loop step reads the final draft from the blackboard head, not a template) is documented here and in ADR-016 §14.5.)*
- [x] Used as the seed fixture for M15/M18 (approval + dashboard demos) *(the `publish` step echoes the editor's final article — the side-effect payload M15.5's human-approval gate slots in front of; the loop's `graph_expanded` deltas and `cost_updated` stream are what M18 animates. Called out in the doc's "This example is a seed fixture" section and ROADMAP 15.5/21.1's `14.5` dependency.)*

---

## Milestone 15 — Human-in-the-loop

**Goal.** Differentiator #6: a `human_approval` step that parks its branch **without holding a lease or worker slot**, surfaces the proposed action for approve / reject / **edit**, resumes on decision through the normal dispatch path, times out via the delayed queue, and leaves a complete audit trail. Dashboard UI for this lands in M18; the API contract is fully usable headlessly (and by `ctl`).

**Role.** The safety valve for side-effectful agent workflows. Built on primitives that already exist — park/resume (M5.6), delayed delivery (M3.5), scoped auth (M6) — so this milestone is mostly careful semantics, not new machinery.

**Architecture docs:** ADR-017 (HITL semantics: parking, decision model, edit constraints, timeouts, authz, audit).

**Exit criteria:** flagship example gains an approval gate before its side-effectful step; fleet keeps executing other work while parked; approve-with-edit resumes with the edited payload; timeout policy fires correctly; every decision attributable to a key.

#### 15.1 — ADR-017 & approval step schema ✅
**Depends on:** 5.6, 6.1
**ADR-017** + schema: `human_approval` config — title/description templates rendering the proposed action, payload reference (typically the upstream step's output), allowed decisions, optional JSON Schema constraining edits, `timeout` + `on_timeout` (`reject` | `approve` | `park`), and reject routing (fail branch vs dedicated reject edge). Decision-precedence and race rules (human vs timeout) specified.
**Done when:**
- [x] Schema + validation for all config combinations (incl. reject edge presence rules)
- [x] Race semantics (decision vs timeout) specified with a state diagram
- [x] Authz model: `approve` scope required; self-approval stance documented

#### 15.2 — Park without lease ✅
**Depends on:** 15.1, 4.3
Executor path: render payload → write `approvals` row (pending) + step → `awaiting_human` in one tx → **ACK the queue message** (no PEL residency, heartbeat stops, worker slot freed). Reconciler treats `awaiting_human` as healthy-parked. Crash between commit and ACK is benign (redelivery sees `awaiting_human` → ACK-and-drop).
**Done when:**
- [x] While parked: PEL contains nothing for the step; fleet continues other runs (throughput asserted)
- [x] Crash-before-ACK scenario converges via ACK-and-drop (integration)
- [x] Pending approval visible in run status + events

#### 15.3 — Decision API ✅
**Depends on:** 15.2, 6.5
`GET /v1/approvals?status=pending` (filterable, paginated) and `POST /v1/approvals/{id}:decide` — `{decision: approve|reject, edited_payload?, comment?}`; edits validated against the step's edit schema; CAS `pending → decided` (concurrent decide → 409); approve → step succeeds with (edited) payload as its output → outbox successors; reject → per-config routing. Actor key ID + timestamp + comment recorded.
**Done when:**
- [x] Full decision matrix integration-tested (approve, approve+edit, reject→fail, reject→edge)
- [x] Invalid edit rejected 422 with schema errors; double-decide → 409
- [x] Audit: decision record immutable and exposed in run status + events

#### 15.4 — Approval timeouts ✅
**Depends on:** 15.3, 3.5
On park, schedule an expiry envelope via the delayed queue; on firing, apply `on_timeout` through the same CAS as human decisions (single winner under race), emit events, and clean up the schedule on early decision.
**Done when:**
- [x] Fake-clock test: timeout fires → policy applied; early decision cancels the expiry (no double transition)
- [x] Race test: near-simultaneous decide + expiry → exactly one wins, event history coherent
- [x] Timeout policy `park` leaves the run resumable via unpark (tested)

#### 15.5 — Notification webhook & example update ✅
**Depends on:** 15.3, 8.7, 14.5
Optional notification plugin: on new pending approval, POST a signed (HMAC) payload to a configured webhook URL — delivered effectively-once via the side-effect journal + retries. Update the flagship example: approval gate (with edit schema) before its side-effectful publish step; CI auto-approves via API.
**Done when:**
- [x] httptest webhook receives exactly one valid HMAC-signed notification despite injected retries
- [x] Flagship example pauses at the gate; CI decides via API and completes
- [x] Webhook failures never affect run correctness (fire-and-journal, capped retries, warning event)

---

# Phase C — Realtime & UI

---

## Milestone 16 — Realtime events & WebSocket streaming

**Goal.** The live nervous system for the dashboard: a normalized, per-run-ordered event feed (monotonic `seq` in Postgres — the durable truth), best-effort Redis pub/sub fan-out for low latency, an authenticated WebSocket endpoint with snapshot → backfill → live-tail semantics and seq-based resume, plus a filtered multi-run firehose and a small typed TS client.

**Role.** Decouples "engine emits facts" from "UI renders them": the WS protocol is defined against durable seq numbers, so dropped connections and missed pub/sub messages recover deterministically. Everything M18 renders arrives through this layer.

**Architecture docs:** ADR-018 (event taxonomy, delivery semantics, WS protocol & auth).

**Exit criteria:** a WS client receives snapshot + ordered events for a run across reconnects with zero gaps/dupes (by seq) while two workers execute it; 100 concurrent clients sustained in test.

#### 16.1 — ADR-018 & event schema normalization ✅
**Depends on:** 2.3, 15.3
**ADR-018:** typed event envelope (per-run monotonic `seq`, `type`, `ts`, versioned payload) covering run/step status changes, attempts, `cost_updated`, `graph_expanded`, approval lifecycle, guard/park events; delivery = at-least-once with client dedupe by seq; step logs stay out of the main feed (separate channel). Migrate/normalize existing `events` writers to the envelope.
**Done when:**
- [x] Event catalog documented with payload schemas (generated types basis)
- [x] All engine writers emit normalized envelopes (grep/CI check: no ad-hoc event writes)
- [x] Seq monotonic per run under concurrent writers (integration assertion)

*(As built: ADR-018 + the new leaf package `internal/event` — `Type` vocabulary + one `Payload` struct per event type (`EventType()`; step-scoped ones add `EventStepID()`), the `Envelope` (per-run `seq`, `type`, `ts`, lifted `step_id`, versioned payload; `SchemaVersion` stamped at projection, no DB column), the ordered `Catalog` + `Lookup`/`Decode`/`DecodeEnvelope`, and `GenerateSchema()` → the committed `docs/schema/events.v1.json` (emitted via `internal/dag/gen -events-out`, CI-drift-checked with the definition/PlanOutput schemas). The store re-exports the engine/api-referenced payloads as aliases (`store.CostUpdatedEvent = event.CostUpdated`) and derives its `Event*` string constants from the vocabulary; its **two typed `appendEvent` helpers are the only sanctioned event writers** — both derive the type from an `event.Payload`, and `TestNoAdHocEventWrites` (go/ast) fails any other physical `AppendEvent`/`Events().Append` call site. `store.EventEnvelope(row)` is the read adapter (decode + lift step id). The attempt field is normalized to `attempt` across the five step-lifecycle payloads that used `attempt_no` (safe on an append-only feed). Step logs stay on their M7.4 REST channel. **No migration, no new config var, no new metric.** Tests: `internal/event` catalog-completeness/self-consistency/decode-round-trip/envelope-projection + schema drift & content; store `TestNoAdHocEventWrites` + `TestEventConstantsMatchVocabulary` + integration `TestEventSeqMonotonicUnderConcurrentWriters` (8×25 concurrent appends → seqs exactly 1..200) + `TestEventBackfillFromLastSeq`; engine integration `TestEventFeedNormalized` (a real run's every stored row decodes typed, seq gap-free, step-scoped events carry a step id). Green under `-race`; golangci-lint clean (a scoped revive exclusion for the trivial interface-marker methods + the re-export aliases). ADR-004/architecture cross-amended. **Next: 16.2** (live publish path — Redis pub/sub).)*

#### 16.2 — Live publish path (Redis pub/sub) ✅
**Depends on:** 16.1
After-commit best-effort publish to `run:{id}` channel (and a firehose channel) from workers/API; durable truth remains Postgres. Publisher helper with metrics (publish latency, failures). Consumers detect seq gaps → fall back to DB backfill.
**Done when:**
- [x] Subscriber sees events <100ms after commit (local integration budget)
- [x] Simulated pub/sub loss (paused subscriber) recovers via gap-detected backfill with no dupes
- [x] Publish failures never affect the engine transaction (async, logged, metered)

*(As built: the publish path hangs off **one seam at the store's transaction boundary** — the new `store.EventSink` (`EventsCommitted(ctx, []event.Envelope)`), fed after a committed `WithTx` from a per-tx buffer both `appendEvent` helpers fill with the **projected envelope** (built from the payload + `RETURNING` `created_at`, no re-decode). So all ~52 event-append sites — engine completion/fan-out, `engine.Control`'s API-side lifecycle ops, and API-side instantiation — publish with **zero call-site change** and no engine awareness of pub/sub; `WithEventSink` is a variadic `store.Option` (existing callers unaffected). New leaf **`internal/event/pubsub`** (imports `internal/event` + go-redis, never `store` — the `cache/redisstore` precedent): channel naming (`<prefix>:run:<uuid>` + `<prefix>:firehose`), the async **`Publisher`** (satisfies `store.EventSink`; non-blocking enqueue into a bounded buffer, single drain goroutine PUBLISHes per envelope under a per-batch timeout, drop-on-overflow + publish-failure both metered, `Close(ctx)` drains), the **`Subscription`** (blocks until SUBSCRIBE confirmed, pumps `ParseEnvelope`'d envelopes, drops unparseable), and the pure single-run **`Tailer`** (dedupe by seq, gap → paged DB backfill to head → re-evaluate live — the snapshot/backfill/tail assembly 16.3/16.5 reuse). New `event.ParseEnvelope([]byte)` (wire = the envelope JSON, so publish and backfill agree). Config `AGENTLOOM_EVENTS_PUBSUB_ENABLED` (default true) + `_CHANNEL_PREFIX`/`_PUBLISH_BUFFER`/`_PUBLISH_TIMEOUT`; both deployables build the publisher over the shared coordination Redis client and wire it as the store sink (API stays fail-soft — ADR-002 holds, a latency hint is not a dispatch). New ADR-008 `events` subsystem — `engine_events_published_total{channel}`, `_publish_failures_total`, `_publish_dropped_total`, `_publish_latency_seconds` — embedded in both `WorkerMetrics` and `APIMetrics`. **No migration, no new store table.** Tests: `event` parse round-trip; store `TestEventSink*` integration (after-commit batch == appended rows, rollback/no-append deliver nothing, concurrent-writer union = seqs 1..N); pubsub unit (Tailer table, publisher fake/overflow/latency/closed-client) + integration (publish→both channels round-trip); engine integration `TestLivePublishReachesSubscriber` (DoD-1: live latency + snapshot/backfill/tail completeness under 2 workers), `TestPubSubLossRecoversViaBackfill` (DoD-2: drop every 3rd live message → Tailer heals to exactly 1..N), `TestPublishFailureNeverAffectsEngine` (DoD-3: publisher over an unreachable Redis → run succeeds, feed gap-free, failures metered). Green under `-race`; golangci-lint clean; `make generate` diff-clean. ADR-018 §"Live publish path (as built, 16.2)"; ADR-008 amended. **Next: 16.3** (run WebSocket endpoint).)*

#### 16.3 — Run WebSocket endpoint ✅
**Depends on:** 16.2, 6.2
`GET /v1/runs/{id}/ws`: auth via short-lived signed ticket minted at `POST /v1/runs/{id}/ws-ticket` (avoids long-lived keys in browser URLs; bearer-subprotocol documented as alternative). Protocol: server sends run snapshot → backfills events from client's `last_seq` → live-tails; heartbeat ping/pong; slow-client policy (bounded buffer, close on overflow with a resumable code).
**Done when:**
- [x] Protocol integration test: connect → snapshot → kill connection mid-stream → reconnect with `last_seq` → no gaps/dupes
- [x] Expired/invalid tickets rejected; ticket scoped to one run + read scope
- [x] Slow-client overflow closes with documented code; client can resume

*(As built: `GET /v1/runs/{id}/ws` composes the 16.2 pubsub leaf (`Subscriber` + `Tailer`) with the store's new `EventRepo.EventsAfter` (the `pubsub.Backfiller` shape — `st.Events()` plugs straight into the Tailer, replacing the 16.2 engine test's local adapter). **No migration, no new metric**; one config block (`AGENTLOOM_API_WS_TICKET_{SECRET,TTL}`), one dep (`github.com/coder/websocket`). **Auth** is a short-lived HMAC-signed opaque ticket minted at `POST /v1/runs/{id}/ws-ticket` (`read` scope, `read` class) scoped to one run + read; the WS handshake's `requireReadOrTicket` accepts the ticket (query param, the browser path) OR a `read` bearer (non-browser), every failure a uniform 401 — so `/ws` is exempt from the requireScope auth-matrix walk and covered by `TestWSTicket*`. Empty `AGENTLOOM_API_WS_TICKET_SECRET` ⇒ cmd/api mints a random per-process secret (boot warning: not valid across replicas). **Protocol** (JSON text frames): `snapshot` (the `GET /v1/runs/{id}` body, via a shared `loadRunResponse`) → `event` frames backfilled from `?last_seq` → `caught_up` → live `event` frames; clients dedupe/order by `(run_id, seq)` and resume with `last_seq`. Subscribe-before-backfill closes the live/backfill window; a subscribe failure or a nil subscriber degrades to DB **polling**; a slow **resync** `Catchup` while live heals the 16.2 lost-final-publish residual. **Slow-client** frames ride a bounded buffer drained by one writer goroutine; a client that keeps it full for `SlowClientTimeout` is closed **4001** and resumes — the signal is the buffer enqueue timeout, not a ctx cancel (coder/websocket closes the conn on a cancelled Write, which would eat the close frame), so the driver stops feeding and lets the writer drain before sending 4001. `statusRecorder.Unwrap()` lets the hijack reach the `http.Hijacker`; the request log skips the latency histogram for the 101 upgrade. `WithWebSocket(WSOptions)` seam; `WSSubscriber`/`WSEventStream` are narrow api interfaces `*pubsub.Subscriber`/`*pubsub.Subscription` satisfy via a thin cmd/api adapter (api never imports go-redis); routes unmounted-secret answer 503 `stream_unavailable`. OpenAPI two ops (`mintRunWSTicket` 200, `streamRunEvents` 101 + WS frame schemas, vacuum 100/100 with a justified `operation-success-response` opt-out for the 101 upgrade). Tests: `ws_test.go` (ticket round-trip/expiry/wrong-run/tamper/version matrix, frame encodings, parseLastSeq); `ws_integration_test.go` DoD-1 `TestWSSnapshotBackfillResume` (kill mid-stream → resume → union == seqs 1..N), DoD-2 `TestWSTicketAuth` (403/404/valid ticket + bearer/rejections), DoD-3 `TestWSSlowClientCloses` (flood + stall → 4001 → reconnect), `TestWSPollFallback` (no subscriber → poll delivers full feed). Green under `-race`; golangci-lint clean (incl. integration tag); `make generate` diff-clean. ADR-018 §"Run WebSocket endpoint (as built, 16.3)"; ADR-007 route rows; `docs/api.md` WS walkthrough. **Next: 16.4** (multi-run firehose).)*

#### 16.4 — Multi-run firehose endpoint ✅
**Depends on:** 16.3
`GET /v1/events/ws` with server-side filters (run IDs, event types, definition) for the dashboard's run list; subscription management messages; connection registry with per-connection metrics; same ticket auth.
**Done when:**
- [x] Filtered subscriptions deliver only matching events (test matrix)
- [x] 100 concurrent WS clients tailing a chaos-suite load: no server degradation beyond budget (measured)
- [x] Connection count/backpressure metrics exported

*(As built: `GET /v1/events/ws` generalizes the 16.3 run WS to many runs on one connection — a process-wide, refcounted `firehoseHub` holds at most one Redis `SubscribeFirehose` and fans each committed envelope out to every connected client's bounded inbox (drop-on-full → `ws_hub_dropped_total`, healed by backfill); each connection tails the runs its subscriptions want through per-run `pubsub.Tailer`s and **backfills from head on discovery** so the complete per-run feed survives cross-publisher (API `run_created` vs worker step events) reorder. The connection transport is the shared `wsLink` the 16.3 run WS was refactored onto (bounded buffer + sole writer + 4001 slow-close). **Protocol:** `subscribe {id, filter, cursors}` / `unsubscribe {id}` control frames → `subscribed` ack → cursor backfill → `caught_up {id, cursors}` → live `event` frames tagged with the matched `subscriptions`; a malformed/over-limit control message is an in-band `error` frame (`bad_message`/`filter_invalid`/`subscription_limit`/`unknown_subscription`) that leaves the connection open. **Filters** `run_ids`/`types`/`definition_id`/`definition_name` are ANDed; run→definition resolution rides a shared bounded cache filled from the new additive `run_created.definition_id` payload field. **Auth** is the same ticket with a new `aud` claim (`firehose` vs `run`, cross-rejected), minted at `POST /v1/events/ws-ticket`; both routes are absent from the requireScope auth matrix and answer 503 `stream_unavailable` when WS is off. **Metrics** (ADR-008 `api`, `kind` ∈ {run,firehose}): `engine_api_ws_{connections,subscriptions,frames_sent_total,slow_closes_total,hub_dropped_total,send_queue_fill_ratio}` — the 16.3 run WS retrofitted onto the same set. **No migration, no new config var, no new store table**; one additive event field, one OpenAPI `Events` tag + two ops (vacuum 100/100), four new error codes. **Tests:** unit (ticket audience, filter compile/match, cursor parse, run-meta, frame encodings); integration `TestFirehose{FilteredDelivery (DoD-1),CursorResume,Auth,ControlErrors,SlowClientCloses,PollFallback,Metrics}`; load `TestFirehoseHundredClients` (DoD-2, `make test-firehose-load`: 100 clients, commit→receipt p95 ~15ms, REST p95 1.3→3.1ms, zero slow-closes). Green under `-race`; golangci-lint clean; `make generate` diff-clean. ADR-018 §"Multi-run firehose (as built, 16.4)"; ADR-007/008 route+metric rows; `docs/api.md`/`docs/observability.md` updated. **Next: 16.5** (typed TS event client).)*

#### 16.5 — Typed TS event client ✅
**Depends on:** 16.3
`web/lib/engine-client`: TS types generated from the event/envelope JSON Schemas (CI drift check against Go), a small client implementing ticket auth, snapshot/backfill/tail, seq dedupe, resume, and reconnection backoff. Usable from Node for headless tailing (example script) — the exact client M17/18 build on.
**Done when:**
- [x] Type drift between Go schemas and TS types fails CI
- [x] Node example script tails a live compose run correctly through a forced reconnect
- [x] Client unit tests cover resume, dedupe, and backoff logic

*(As built: `web/lib/engine-client` (`@agentloom/engine-client`) — the first package in the new `web/` **pnpm workspace** (Corepack-managed, `packageManager`-pinned; M17 adds the Next.js app), a **pure library with no React/UI imports and no runtime dependency** (global `WebSocket` + `fetch`, Node >= 22). (1) **Generated event types + two-layer drift check.** `src/generated/events.ts` is emitted from the committed `docs/schema/events.v1.json` by `scripts/gen-events-types.ts` — a **dependency-free deterministic emitter** over the exact invopop vocabulary (byte-stable output is what a drift diff needs; a codegen lib's formatting could flake it). It renders one interface/alias per `$defs` entry + a discriminated layer (`EVENT_TYPES` const, `EventType` union, `EventPayloadMap`, `EventEnvelope = {[K in EventType]: base & {type:K; payload:Map[K]}}[EventType]`) so `switch (env.type)` narrows `env.payload`. The one override: the reflected `UUID` def is `[16]byte`, but google/uuid marshals a **string** on the wire → `UUID = string`. Drift: Go structs → `events.v1.json` (existing Go-CI diff) → `events.ts` (new `web` CI job regenerates + `git diff --exit-code`), so a Go payload change unreflected in the TS fails CI on one side or the other. (2) **Ticket auth, two modes:** `{ apiKey }` mints tickets itself with a `read` bearer (server-side; the key stays server-side); `{ mintTicket }` lets the caller supply them (browser → server-side proxy, the M17 path). A fresh ticket is minted on **every (re)connect** (TTL ~60s); a `read` bearer on the upgrade is the documented non-browser alternative via a custom `webSocketFactory`. (3) **Deterministic recovery:** `RunStream` runs snapshot → backfill from `last_seq` → live tail; reconnects with `last_seq` = highest seq seen (snapshot re-emitted; server re-backfills the exact tail → union gap-free/dup-free by seq); **4001 resumes immediately**, every other close backs off (exponential + full jitter, reset on `caught_up`). `FirehoseStream` manages subscriptions client-side and **re-issues each with its tracked per-run cursors on reconnect** (bounded to `MaxCursorRuns`, non-terminal-first), deduping each run across the re-backfill; in-band `error` frames are surfaced and leave the connection open. (4) **Wire frames hand-mirrored** from the OpenAPI `WS*Frame` schemas (M16.5 predates M17.1's generated REST client; the vocabulary that actually drifts is the generated part). Every transport seam (socket, timers, `fetch`) is injectable → **26 vitest unit tests** over a fake transport + fake clock (backoff, cursor dedupe/gap, run resume/dedupe/4001/user-close/mint-failure-backoff/mintTicket/terminal-close, firehose filtered-delivery/cross-run-dedupe/cursor-resubscribe/in-band-error/unsubscribe/cursor-bound, generated-types sanity). (5) **`examples/tail-run.ts`** + **`scripts/ws-tail-smoke.sh` (`make smoke-ws-tail`, DoD-2):** boots app, submits a ~12s sleep chain, tails via the Node example through a mid-run `docker compose restart api`, asserts received seqs == `1..max(events.seq)` with ≥1 reconnect. **New `web` CI job** (setup-node + Corepack pnpm: install/generate-drift/typecheck/test/build). **No Go change, no migration, no config var, no metric.** ADR-018 §"Typed TS client (as built, 16.5)"; `docs/api.md` client subsection; `web/README.md` + `web/lib/engine-client/README.md`. **M16 complete. Next: M17.1** (web app scaffold & typed API client).)*

---

## Milestone 17 — Frontend: visual DAG builder

**Goal.** The node-based builder: Next.js + React Flow with a node palette (LLM, tool, retrieve, branch, map, planner, agent, approval, join), config panels **generated from the backend's plugin JSON Schemas**, client-side graph validation mirroring backend rules (cycles forbidden except marked loop edges), and a deliberately isolated serialization module mapping canvas state ⇄ the workflow definition JSON — the architectural boundary, property-tested against the backend's own fixtures.

**Role.** Arrives only now, per plan: the headless engine (API + JSON workflows) is fully functional and proven. The builder is a *view* over the same definition contract that has existed since M1 — nothing engine-side changes to accommodate it.

**Architecture docs:** ADR-019 (frontend architecture & the serialization boundary).

**Exit criteria:** import flagship example → edit visually → validate → save as definition → submit run against the compose backend (mock provider) — entirely through the UI; serialization round-trip is lossless over the full fixture corpus.

#### 17.1 — Web app scaffold & typed API client ✅
**Depends on:** 6.6, 0.2
`web/`: Next.js (App Router, TS strict), Tailwind + shadcn/ui, `openapi-typescript` client generated from `api/openapi.yaml` (CI drift check), auth wiring (server-side proxy holds the API key via env; local dev key entry documented), vitest + Playwright harness, `pnpm` workspace, frontend CI job (lint, typecheck, unit, build).
**Done when:**
- [x] App lists definitions/runs from the compose backend through the typed client *(`/definitions` server-rendered via the direct client, `/runs` client-rendered through the same-origin proxy — both over `@agentloom/api-client`; verified live against the compose stack in the Playwright smoke)*
- [x] API key never shipped to the browser in proxy mode (verified in build output) *(`scripts/check-bundle-no-key.sh` builds with a sentinel key and asserts it + `AGENTLOOM_API_KEY` are absent from `.next/static`; `web` CI job step + the e2e's "no browser request carries Authorization" assertion)*
- [x] Frontend CI job green; Playwright smoke test runs against compose *(`web` job widened — lint + typecheck + unit + build + build:verify + two-side generated-types drift check; new `web-e2e` job runs `scripts/web-e2e-smoke.sh` against the compose app profile)*

*(As built: **two new workspace packages plus the Next.js app**, no Go change, no migration, no config var, no metric — 17.1 is entirely under `web/` plus CI/Makefile/docs. **(1) `web/lib/api-client` (`@agentloom/api-client`)** — the typed REST client. `src/generated/openapi.ts` is emitted from `api/openapi.yaml` by `scripts/gen-openapi-types.ts` via `openapi-typescript` (v7; verified to resolve the spec's two external `$ref`s to `docs/schema/{workflow-definition,events}.v1.json`, inline them, compile strict, and emit byte-deterministically across runs — so a CI diff is stable). The runtime is a thin `openapi-fetch` wrapper: `createApiClient({ baseUrl, apiKey?, fetch? })` attaches `Authorization: Bearer <key>` via a request middleware only when a key is given, plus a `problem()` helper narrowing the `{error:{code,message,issues}}` envelope, and ergonomic re-exports (`RunView`/`DefinitionView`/`ErrorCode`/…). **Closes the 16.5 residual** ("frame types hand-mirrored until M17.1") with a compile-time `test/frame-parity.test.ts` asserting engine-client's hand-written `WS*Frame` payloads are structurally compatible with the generated `WS*Frame` schemas (omitting the `type` discriminant — openapi-typescript synthesizes a schema-name literal where the wire value is lowercase — and the independently-generated `event`/`run` payloads), so engine-client stays dependency-free while the drift is now type-checked; `FirehoseCaughtUpFrame` was added to engine-client's exports for it. Drift chain: Go structs → `openapi.yaml` (route/op coverage tests) → `openapi.ts` (new CI diff over `lib/*/src/generated`). **(2) `web/app` (`@agentloom/app`)** — Next.js 15 App Router, React 19, TS strict, Tailwind v4 + shadcn-style primitives (badge/button/card/table under `src/components/ui`, the `cn` util + `components.json` so `npx shadcn add` works later), ESLint 9 flat config (with a seeded `no-restricted-imports` boundary rule on `src/lib/graphdef/**` for 17.2). **Proxy-only auth:** a catch-all Node-runtime route handler `src/app/api/agentloom/[...path]/route.ts` injects the server-held bearer, allowlists `v1/**`+`healthz` (else 404, no open relay), never copies a client-supplied `Authorization` (the proxy sets its own), forwards method/query/body + passes back status/body/`X-RateLimit-*`/`Retry-After`, and 503s with an envelope when no key is configured — the API has no CORS, so this same-origin proxy is the only browser path. `src/lib/api/{server,browser}.ts` are the two client factories (server holds the key, browser targets `/api/agentloom`); `src/lib/config.ts` is `server-only`. Pages: `/` (backend health / setup card), `/definitions` (server-rendered latest-per-name table — direct client), `/runs` (client-rendered table with status filter + keyset "Load more" — proxy client), so both auth paths run in production code. Config reuses `AGENTLOOM_API_URL`/`AGENTLOOM_API_KEY` (the `ctl` names; never `NEXT_PUBLIC_`); `.env.example` + the home/definitions setup cards document local key entry. **(3) Tests:** api-client vitest (bearer injection, query serialization, success/error narrowing, `problem`, generated-types sanity, frame parity) — 9; app vitest over jsdom (proxy handler: bearer injection, method/path/query/body/idempotency-key forwarding, client-Authorization never forwarded, allowlist 404, missing-key 503; `RunsPage` render/empty/error over a mocked client) — 8; engine-client's 26 unchanged. **(4) Bundle no-key proof** (`scripts/check-bundle-no-key.sh`, `pnpm build:verify`, CI step): builds with a **runtime-generated** sentinel key (so the repo's `sk_`-literal grep stays clean) and asserts neither the value nor `AGENTLOOM_API_KEY` appears in `.next/static`. **(5) Playwright** (`e2e/smoke.spec.ts` + `playwright.config.ts` running `next start` via corepack): seeds a definition + run through the backend, then asserts `/`/`/definitions`/`/runs` render the seeded rows through the app **and that no browser request carries an `Authorization` header** — verified green against the live compose stack. `scripts/web-e2e-smoke.sh` (`make web-e2e`) boots the app profile with an ephemeral root key. **(6) Wiring:** `pnpm-workspace.yaml` gained `app`; root scripts became `-r --if-present` (+ `dev`/`e2e`/`build:verify`); engine-client gained a `lint` script; Makefile `web-lint`/`web-build`/`web-dev`/`web-e2e`; the `web` CI job widened (lint + build:verify + two-side drift) and a new `web-e2e` job added. **Decisions:** `openapi-fetch` over a hand-rolled client (zero drift with the generated `paths`, ~2 KB); proxy-mode-only (the API has no CORS; a "direct browser" mode is deliberately not offered); one app for both M17 builder and M18 dashboard; reuse the `ctl` env names; `next-env.d.ts` committed as the canonical 2 lines (tsc tolerates the missing `.next/types` reference, so standalone typecheck works before a build); a compile-time frame-parity test rather than rewiring the pure engine-client. **Accepted residuals:** `next lint` swapped for the ESLint CLI (deprecated in Next 16); `next-env.d.ts` is rewritten to 3 lines by a local build (harmless — CI checks only `lib/*/src/generated`). `docs/api.md` "Typed REST client" subsection; `web/README.md` + `web/app/README.md` + `web/lib/api-client/README.md`. **Next: 17.2** (ADR-019 & the serialization module).)*

#### 17.2 — ADR-019 & the serialization module ✅
**Depends on:** 17.1, 1.6
**ADR-019:** canvas state model, `toFlow(definition)` / `toDefinition(nodes, edges)` mapping rules, `ui` block ownership (positions/layout hints live in the definition's `ui` block; engine ignores, builder round-trips), unknown-field passthrough (forward compatibility), validation parity strategy. Implement as a pure TS module (`web/lib/graphdef`) with types generated from the definition JSON Schema.
**Done when:**
- [x] Round-trip property tests: definition → flow → definition is lossless over the **backend fixture corpus** (M1.6 files consumed directly) *(`test/roundtrip.test.ts` runs `toDefinition(toFlow(f))` deep-equal over `examples/definitions` + `internal/dag/testdata/{valid,invalid_structural}` — 293 assertions — plus flow-idempotence and determinism; the two duplicate-id fixtures assert `toFlow` throws `GraphdefError(duplicate_step_id)`, and every object-shaped `invalid/` fixture round-trips or throws a `GraphdefError`, never crashes.)*
- [x] Unknown future fields survive round-trip untouched *(`test/passthrough.test.ts` — a hand-written definition with unknown keys at every object level + a junk-laden `ui` block, and a seeded-PRNG property test injecting random `x_future_*` keys and layout perturbations across the whole corpus (>300 cases), all still lossless.)*
- [x] Module has zero React/UI imports (boundary enforced by lint rule) *(`eslint.config.mjs` `no-restricted-imports` bans react/react-dom/next/`@xyflow/*`/`server-only`; `test/boundary.test.ts` scans every source specifier and asserts `package.json` declares no UI dependency and no runtime dependency at all.)*

*(As built: **new workspace package `web/lib/graphdef` (`@agentloom/graphdef`)** — the pure serialization boundary, no Go change, no migration, no config var, no metric; entirely under `web/` plus the ADR, one CI drift-path widening, and docs. **(1) ADR-019** fixes the frontend package layering (`api-client`/`engine-client`/`graphdef` pure libs; `app` the only React consumer) and the whole serialization contract the later M17/M18 tickets consume: the `Flow` canvas state model, the `toFlow`/`toDefinition` mapping rules, `ui`-block ownership (`ui.nodes.<id>.position` lifted onto nodes, everything else preserved), the copy-then-overwrite unknown-field passthrough rule, the deterministic synthesized edge id (`from->to` / `from->to#n`), and the forward-stated canonical-export (17.6) and validation-parity (17.5) strategies. **(2) Generated types** — `src/generated/definition.ts` is emitted from `docs/schema/workflow-definition.v1.json` by `scripts/gen-definition-types.ts`, a dependency-free deterministic walker (the engine-client emitter extended for the `Step` `oneOf` → a discriminated union `Step = {[K in StepType]: StepBase & {type:K; config?: StepConfigMap[K]}}[StepType]` so `switch (step.type)` narrows `step.config`, plus `additionalProperties:{$ref}` maps and the `true`-any schema). We generate our own types rather than reuse api-client's `Definition` (openapi-typescript collapses `config` to `Record<string,never>`). Drift: Go structs → `workflow-definition.v1.json` (`make generate`, Go-CI-diffed) → `definition.ts` (the `web` job's `git diff --exit-code` path widened to `lib/graphdef/src/generated`). **(3) The mapping** — `toFlow(unknown, opts?)` shape-checks (object; well-formed `steps`/`edges`; object `ui`) and rejects a duplicate step id with a typed `GraphdefError{code,path}` (backend path grammar `steps[3].id`), lifts each step's `ui.nodes[id].position` (else a deterministic default grid, `positioned:false`), and carries configs/envelope blocks/unknown fields/residual `ui`/orphan `ui.nodes` entries verbatim; it deliberately does *not* validate configs/endpoints/cycles/CEL (that is 17.5) — a dangling edge or invalid config rides faithfully into the `Flow`. `toDefinition(flow)` is the exact JSON-value inverse. `FlowNode`/`FlowEdge` are structurally React-Flow-compatible with **no `@xyflow/*` dependency**. **(4) Tests** (vitest, node env, 319): round-trip/idempotence/determinism over 82 corpus fixtures (293 assertions) + duplicate-id rejection + codec-invalid-never-crashes; explicit + seeded-PRNG passthrough; toFlow specifics (position lift/synthesis/custom `defaultPosition`, orphan/non-object/empty `ui`, edge ids, dangling endpoint, shape-error paths); generated-types sanity (`STEP_TYPES` == the schema enum, compile-time Step narrowing); the boundary scan. **Decisions:** package at `web/lib/graphdef` (roadmap-aligned; React is structurally unresolvable inside it, not just lint-banned); own deterministic emitter over the JSON Schema (not api-client's types, not `json-schema-to-typescript`); `FlowNode`/`FlowEdge` own shapes over an `@xyflow` dependency; `toFlow` throws only on shape violations + duplicate ids (validation is 17.5); losslessness is JSON-**value**-level on the browser side (byte-for-byte canonical export is specified in ADR-019 but implemented in 17.6); `position` is a builder-owned closed `{x,y}` leaf (the property mutator leaves it alone). **Accepted residuals:** value-level (not byte-level) `ui` preservation on the JS side; templates/agents are document-level libraries, not canvas nodes, in M17 (a template sub-canvas is deferred); the app's seeded `src/lib/graphdef/**` eslint block was repointed at `src/lib/pure/**` now that graphdef is its own package with its own boundary config. `docs/adr/README.md` row 019; `web/README.md` + `web/lib/graphdef/README.md`. **Next: 17.3** (canvas, palette & node components).)*

#### 17.3 — Canvas, palette & node components ✅
**Depends on:** 17.2
React Flow canvas: palette with all node types, drag-to-create, typed connection handles (edge rules by port), custom node components (status-neutral styling reused by M18), minimap/zoom/fit, undo/redo (zustand temporal store), multi-select/delete, keyboard a11y basics.
**Done when:**
- [x] Playwright: build a 10-node graph with every node type via mouse/keyboard *(`e2e/builder.spec.ts` adds all ten core types (eight by click, two by keyboard) and asserts the node count)*
- [x] Undo/redo across create/move/connect/delete (unit + e2e) *(store vitest suite covers all four operations incl. drag-coalescing to one entry; the e2e covers delete + move undo/redo)*
- [x] Node components render deterministic snapshots (visual regression baseline) *(committed DOM snapshots of `StepNodeView` for every `StepType` + selected + skinned)*

*(As built: **the React Flow canvas, entirely under `web/app`** — no Go change, no migration, no config var, no metric, and `graphdef` untouched (the canvas holds React Flow's own `Node`/`Edge` with the graphdef step/edge object in `data`, read through typed accessors, since RF v12 constrains `data` to `Record<string,unknown>`). New deps: `@xyflow/react`, `zustand`, `zundo`, plus the `@agentloom/graphdef` workspace dep. **(1) Pure canvas helpers** (`src/lib/pure/builder`, the app's no-React eslint boundary): `catalog.ts` (per-`StepType` label/group/one-line config summary, exhaustive over `StepType`), `ports.ts` (the connection-handle contract), `steps.ts` (id allocation + a bare `{id,type,config:{}}` step). **(2) The handle contract** — every node exposes one `in` target + `out`/`loop` sources; `human_approval` adds `approve`/`reject`. It is the single source of truth both ways: `deriveHandles(edge)` (render an existing edge from the right port) and `edgeSeedForHandle(handle)` (seed a new edge's fields — `loop` → a placeholder marked loop edge, decision handles → a `decision` marker). `isValidConnection` enforces only the port rules (source+target, target on `in`, no self-connection, no exact duplicate `(source,sourceHandle,target)`); cycles/ancestry/decision-consistency are 17.5's, so an invalid-but-well-shaped graph rides faithfully into the `Flow` (matching `toFlow`). **(3) The store** (`src/lib/builder/store.ts`) is zustand under zundo's `temporal` middleware: the tracked slice is the serializable canvas; RF runtime churn (selection/measurement/viewport) is kept out of history by `partialize`+structural `equality`; a **drag coalesces into one undo entry** (history paused for the drag, the pre-drag snapshot pushed once on stop — a naive pause/resume drops the move from history, so the manual push is what makes a move undoable). Edge ids are the deterministic `graphdef.edgeId(from,to,n)` at the lowest free ordinal. The `adapter.ts` beside the store seeds RF arrays from a `Flow` (deriving handles + render type) and strips runtime fields back off so `toDefinition` sees exactly the ADR-019 shapes. **(4) Components** — `StepNode` splits into a status-neutral, React-Flow-free `StepNodeView` (with a `skin` slot the **M18 dashboard reuses** for run-status skins) and the RF wrapper adding the typed handles; two edge renderers (`step`/`loop`) with routing labels; a `Palette` (grouped, drag-to-create + click/Enter add — the reliable keyboard/e2e path); a `Canvas` (minimap/zoom/fit, palette-drop at the cursor, drag batching, `isValidConnection`); an `Inspector` placeholder with a live definition preview (replaced by 17.4). **(5) Routing** — the list pages move into a `(site)` route group (centred column), the builder gets a `(builder)` group (full-bleed); URLs unchanged. A webpack `resolve.extensionAlias` in `next.config.ts` resolves graphdef's runtime `.js` specifiers onto its `.ts` source (api-client's `.js` imports are type-only/erased, so this is the first place it bites). **(6) Tests** — vitest over the pure helpers + store (create/connect/delete undo-redo, drag-coalescing, selection/viewport untracked) + committed DOM snapshots of `StepNodeView` for every `StepType`; a Playwright `builder.spec` (10-node graph via mouse+keyboard, a reject-port decision edge asserting `decision:"reject"`, delete/move undo-redo). **Decisions:** zundo for the temporal store; click/keyboard add primary, HTML5 DnD secondary; DOM snapshots over screenshots (cross-OS stable); the loop port seeds a placeholder edge now (fields edited in 17.6); no cycle/ancestry rules until 17.5. ADR-019 §"Canvas, palette & node components (as built, 17.3)". **Next: 17.4** (schema-driven config panels — forms generated from `GET /v1/plugins` JSON Schemas).)*

#### 17.4 — Schema-driven config panels
**Depends on:** 17.3, 8.1
Selecting a node opens a config panel rendered from the plugin's JSON Schema (`GET /v1/plugins`): custom renderers for core field types (string/number/enum/secret-ref/model-picker), JSON editor fallback for the rest, inline validation, and a prompt editor with template-variable autocomplete populated from *upstream* nodes only (uses the graph's reachability — wrong-direction references impossible to author).
**Done when:**
- [ ] Every built-in plugin's config editable via generated forms (no hardcoded per-plugin forms)
- [ ] Invalid config marks the node and blocks submit; errors match backend codes on round-trip
- [ ] Autocomplete offers exactly the upstream steps' output paths (test with branching graph)

#### 17.5 — Client-side graph validation
**Depends on:** 17.4
Mirror backend rules in the browser: cycles rejected **unless the edge is explicitly a loop edge**, dangling handles, unreachable nodes, join config sanity, missing required config, budget sanity warnings. Problems panel with click-to-focus; errors block submit, warnings don't. Parity enforced by running the same fixture corpus through both validators and comparing verdicts.
**Done when:**
- [ ] Parity test: backend valid/invalid fixture corpus → identical accept/reject verdicts client-side
- [ ] Loop-edge authoring flow validates (marked loop OK; unmarked cycle rejected with path highlight)
- [ ] Problems panel focuses the offending node/edge on click (e2e)

#### 17.6 — Import/export, save & submit flows
**Depends on:** 17.5
Import JSON (validated, errors surfaced), export canonical JSON, save/version definitions via API, submit runs with a params modal, loop-edge inspector (mark edge as loop, set condition + `max_iterations`), "open in builder" from a stored definition.
**Done when:**
- [ ] E2E: import flagship example → edit a prompt → save new version → submit → run ID returned (mock backend)
- [ ] Export equals canonical backend serialization byte-for-byte (fixture assertion)
- [ ] Unsaved-changes guard and version-conflict (stale save) handling tested

---

## Milestone 18 — Frontend: live execution dashboard

**Goal.** The demo centerpiece: reuse the builder's DAG visualization to watch runs live over WebSocket — per-step status transitions (running/complete/failed/retrying/throttled/awaiting-human), dynamically injected planner steps animating into the graph, a live running-cost meter with budget state, a per-step inspector (attempts, logs, outputs, validator verdicts, semantic-retry prompt diffs), the HITL approval inbox, and DLQ/ops controls.

**Role.** Makes every invisible guarantee visible: the crash-recovery demo, semantic-retry prompt diffs, cost downgrades, and graph expansions all become watchable — which is precisely the project's demo surface.

**Architecture docs:** none new (ADR-018/019 govern).

**Exit criteria:** scripted demo — submit flagship example from the builder, watch it execute live, kill a worker (statuses visibly recover), approve the HITL gate with an edit, watch cost meter and a budget park/resume, inspect a semantic-retry diff — all without a page refresh.

#### 18.1 — Run list & run-detail scaffolding
**Depends on:** 16.4, 16.5, 17.1
Runs table with live status chips (firehose WS), filters (status/definition/time), keyset pagination; run-detail layout (graph pane, inspector pane, event timeline strip); snapshot load + WS resume wiring via the 16.5 client.
**Done when:**
- [ ] Playwright: submitted run appears and updates in the list without refresh
- [ ] Reconnect mid-run resumes with no visual gaps (seq-resume exercised through the UI)
- [ ] Timeline strip renders the normalized event feed with type filtering

#### 18.2 — Live DAG view with dynamic expansion
**Depends on:** 18.1, 13.6, 17.3
Reuse builder node components with status skins (pending/ready/running/succeeded/failed/retrying/throttled/awaiting_human/dead_lettered/cancelled/skipped), animated active edges, elkjs auto-layout honoring `ui` position hints with stable incremental layout; `graph_expanded` events animate injected nodes in (provenance-badged); loop iterations grouped/collapsible.
**Done when:**
- [ ] Every status skin driven by live events (scripted fixture walks all states)
- [ ] Planner expansion animates in with provenance badge; layout remains stable (no full reshuffle)
- [ ] Crash demo visible: kill a worker → node flips retrying → running on another worker (scripted e2e against compose)

#### 18.3 — Step inspector
**Depends on:** 18.2, 7.4, 11.4
Inspector tabs per selected node: **Overview** (timings, attempts timeline, model used incl. downgrades, idempotency key, claim/worker history), **Output** (JSON viewer), **Logs** (per-attempt via 7.4 API, level filter, follow mode), **Validation** (verdicts, issues, and a diff view of semantic-retry prompt augmentations), **Cost** (per-attempt breakdown incl. overhead + cache savings).
**Done when:**
- [ ] All tabs render from both fixtures and a live run
- [ ] Semantic-retry diff view shows prompt deltas between attempts (the killer demo — e2e asserted)
- [ ] Reclaimed step shows both workers in the claim history

#### 18.4 — Live cost meter & budget UX
**Depends on:** 18.3, 10.5
Run-header cost ticker driven by `cost_updated` events; budget progress bar with threshold coloring; downgrade and park events surfaced as banners; budget-raise action (PATCH + confirm) with park→resume reflected live; saved-by-cache indicator.
**Done when:**
- [ ] E2E: budgeted run climbs the meter, parks at cap, user raises budget, resumes — all live
- [ ] Downgrade banner names from/to models and trigger
- [ ] Meter total matches `GET /v1/runs/{id}/cost` at completion (consistency assertion)

#### 18.5 — HITL approval inbox & decision UI
**Depends on:** 18.3, 15.3
Approvals inbox (live via events, filters, aging indicator); decision modal: rendered proposed action, payload viewer, schema-validated JSON edit, approve/reject with comment; optimistic UI with 409 conflict recovery; "waiting on you" affordance on the DAG node linking into the modal.
**Done when:**
- [ ] E2E: flagship example pauses → approve with edit via UI → run continues; edited payload visible downstream
- [ ] Concurrent decision from another session → this session surfaces the 409 gracefully
- [ ] Reject path routes per config and renders the outcome

#### 18.6 — Ops views: DLQ, queue health, run controls
**Depends on:** 18.1, 6.5
DLQ page (dead-lettered steps with error/attempt context, requeue action), run controls (cancel, park/unpark) with scope-gated buttons, and a queue-health mini panel backed by a new small `GET /v1/system/stats` (ready depth, PEL, delayed, DLQ count, workers seen — from 3.2 introspection + heartbeats).
**Done when:**
- [ ] DLQ requeue round-trips from the UI and the run completes (e2e)
- [ ] Controls hidden/disabled without the required scope (rendered-permissions test)
- [ ] `/v1/system/stats` contract-tested; panel renders live under chaos load

---

# Phase D — Scale, infrastructure, release

---

## Milestone 19 — Load testing & bottleneck remediation (local)

**Goal.** Prove the scale claim with real numbers before touching the cloud: a reproducible load environment (resource-pinned compose, mock provider with realistic latency/token distributions), a Go load generator tracking full run lifecycles, a baseline campaign to the knee point, identification of the **first real bottleneck** with evidence (profiles, pg_stat_statements), remediation, and a certified re-run published in `BENCHMARKS.md`.

**Role.** "Scales to thousands of concurrent executions" must be demonstrated, not architected-for. The likely suspects are pre-registered as hypotheses (Postgres write amplification on events/attempts, outbox drain contention, join-counter hot rows, single-stream serialization) so the investigation is honest — findings drive the remediation tickets, and stream sharding (the ADR-005 lever) is implemented here if the queue is the binder.

**Architecture docs:** `docs/load/plan.md` (targets, scenarios, environment spec, measurement methodology).

**Exit criteria:** ≥1,000 concurrently active runs sustained for 10 minutes on the documented local environment with p50/p99 scheduling latency and throughput published; zero lost runs / duplicate side effects at that load; top bottleneck found, fixed, and the improvement quantified.

#### 19.1 — Load test plan & pinned environment
**Depends on:** 7.5, 8.5
`docs/load/plan.md`: SLO targets (sustain ≥1,000 concurrently active runs for 10 min; scheduling latency ready→running p50/p99 targets; API p99; zero lost/duplicated effects), scenario definitions (linear-10, fanout-50, planner-heavy, agent-loop, mixed), measurement methodology (warmup, steady-state windows, percentile sourcing from Prometheus histograms + loadgen HDR). `docker-compose.load.yml`: resource-pinned Postgres/Redis, scaled worker replicas, mock provider latency/token distributions.
**Done when:**
- [ ] Plan reviewed and committed; hypotheses list pre-registered
- [ ] Load environment boots with one command; resource pins documented
- [ ] Scenario definitions runnable as named configs

#### 19.2 — Load generator
**Depends on:** 19.1
`cmd/loadgen`: open-loop arrival-rate control (constant + ramp), scenario execution against the API, run-lifecycle tracking (submit→terminal via polling/firehose sampling), HDR histograms (submit latency, end-to-end run latency, scheduling latency sampled), live progress output, JSON/CSV results + summary report artifact.
**Done when:**
- [ ] Dry run (100 runs) produces a complete report artifact with percentiles
- [ ] Open-loop rate verified accurate ±5% under load (no coordinated omission)
- [ ] Failure taxonomy in the report (rejected submits vs failed runs vs timeouts)

#### 19.3 — Baseline campaign & bottleneck identification
**Depends on:** 19.2
Ramp each scenario to the knee; capture Grafana snapshots, pprof profiles (worker + API), `pg_stat_statements`, Redis INFO/latency. Write `docs/load/findings-baseline.md`: observed limits per scenario, evidence-ranked bottleneck list, and the selected top target for remediation.
**Done when:**
- [ ] Knee point identified per scenario with saturation evidence (which resource, which metric)
- [ ] Top bottleneck named with profile/query evidence, not speculation
- [ ] Baseline numbers recorded (they anchor the before/after comparison)

#### 19.4 — Remediation #1 (findings-driven)
**Depends on:** 19.3
Fix the top bottleneck. Pre-authorized options (choose per findings): batch event/attempt inserts and collapse per-completion round-trips; outbox drain via LISTEN/NOTIFY + adaptive batching; index/keyset corrections; pgx batching/pool tuning; join-counter contention fix. Scoped strictly to the identified binder; correctness suites must stay green.
**Done when:**
- [ ] Before/after on the binding metric shows a material, quantified improvement (target set from findings)
- [ ] Chaos suite (5.8) and expansion matrix (13.5) still green
- [ ] Change documented in findings doc + relevant ADR updated

#### 19.5 — Remediation #2 / stream sharding
**Depends on:** 19.4
Attack the *next* binder from the post-fix re-measurement. If queue/dispatch serialization binds: implement the planned lever — shard `steps:ready` into K streams by `hash(run_id)` with worker-side multi-shard consumption (config-driven K, rebalancing documented). Otherwise: the findings' #2 item.
**Done when:**
- [ ] Post-remediation-1 re-measurement selects the target with evidence
- [ ] Fix lands with quantified improvement; correctness suites green
- [ ] If sharding: ADR-005 updated; crash-recovery demo re-verified across shards

#### 19.6 — Certified local results & BENCHMARKS.md
**Depends on:** 19.5
Final full-matrix campaign at target load on the pinned environment. Publish `BENCHMARKS.md`: environment spec, methodology, scenario tables (throughput, p50/p95/p99 scheduling + end-to-end latency), resource profiles, verified-integrity statement (zero lost/dup effects at load), known limits and the *next* bottleneck. Linked from README.
**Done when:**
- [ ] ≥1,000 concurrently active runs sustained 10 min — or the honest shortfall documented with analysis
- [ ] All published numbers reproducible via documented commands
- [ ] Integrity assertions (effect counters, quiescence invariants) passed at load

---

## Milestone 20 — Kubernetes & AWS deployment (Terraform)

**Goal.** Production infrastructure as a distinct, final-phase concern building on Dockerfiles that have existed since M4: hardened images, a Helm chart with probes/PDBs/graceful drain, Terraform-provisioned AWS (EKS, RDS Postgres, ElastiCache Redis, ECR, IRSA), CI/CD deploys, KEDA autoscaling of workers driven by our own queue-depth metrics, the in-cluster observability stack — then the two certifications: the kill-a-pod crash demo and the load-test re-run on EKS, confirming the horizontal-scaling claims in a real cluster.

**Role.** The requirements' infra story verbatim: managed stateful services and EKS justified in an ADR, reproducible infra as a resume-grade artifact, Kubernetes rescheduling composing with lease reclaim, and Prometheus metrics feeding autoscaling decisions.

**Architecture docs:** ADR-020 (infrastructure: EKS vs self-managed, RDS/ElastiCache vs self-hosted, KEDA choice, environment strategy, cost & teardown discipline).

**Exit criteria:** `terraform apply` → CI deploy → flagship example runs on EKS; `kubectl delete pod` mid-run recovers via reclaim with zero duplicate effects; workers autoscale on queue depth; EKS benchmark section added to BENCHMARKS.md; `terraform destroy` leaves nothing behind.

#### 20.1 — ADR-020: infrastructure architecture
**Depends on:** 0.4, 19.6
Write the mandated justifications: **managed stateful services** (RDS/ElastiCache — backups, patching, HA are undifferentiated ops burden and not this project's signal; self-hosting stateful workloads in-cluster adds risk without learning value here), **EKS vs self-managed** (control-plane operations vs orchestration-pattern signal), KEDA vs HPA+prometheus-adapter (choose KEDA; alternative documented), environment strategy (staging profile; prod-shaped), monthly cost estimate per environment, and the **teardown discipline** (this is a portfolio deployment: destroy when idle; documented runbook).
**Done when:**
- [ ] Every mandated justification present with considered alternatives
- [ ] Cost table per environment (idle + under load) with instance choices
- [ ] Teardown/retention policy explicit

#### 20.2 — Hardened images & supply chain
**Depends on:** 4.6, 0.2
Production Dockerfiles: multi-stage, distroless/static, non-root, pinned bases, version/commit stamping (`ldflags`) for api, worker, migrations runner, and the web app (standalone output). CI: image build, Trivy scan gate, SBOM generation. Compose switched to the same images for parity.
**Done when:**
- [ ] Go images <30MB, run as non-root, pass Trivy gate in CI
- [ ] `--version` reports commit/version from build stamping
- [ ] Compose parity verified (same images boot the local stack)

#### 20.3 — Helm chart
**Depends on:** 20.2, 5.7
`deploy/helm/`: API (Deployment/Service/Ingress, HPA on CPU as placeholder until 20.7), worker (Deployment, `preStop` + `terminationGracePeriodSeconds` wired to the 5.7 drain), migrations (pre-install/pre-upgrade Job/hook), ConfigMaps + Secrets (values-driven; SSM/CSI noted as backlog), probes (API `/readyz` = PG+Redis ping; worker liveness/readiness on admin port), resources, PDBs. CI job installs the chart on `kind` with in-cluster PG/Redis (test-mode subcharts) and runs a smoke workflow.
**Done when:**
- [ ] `helm install` on kind → smoke workflow completes (CI)
- [ ] Rolling upgrade under load loses no work (drain verified on kind)
- [ ] Probe misconfiguration cases fail fast and visibly (readiness gates dependencies)

#### 20.4 — Terraform: network, EKS, ECR
**Depends on:** 20.1
`deploy/terraform/`: remote state (S3 + locking), VPC (3 AZ, private worker subnets, endpoints as needed), EKS (managed node group, IRSA/OIDC, core addons), ECR repos with lifecycle policies. Apply/destroy runbook.
**Done when:**
- [ ] Clean `terraform apply` → kubectl-reachable cluster; clean `terraform destroy`
- [ ] IRSA wired (no static AWS keys anywhere in the cluster)
- [ ] State backend + locking verified with a concurrent-apply test

#### 20.5 — Terraform: RDS, ElastiCache, secrets wiring
**Depends on:** 20.4
RDS Postgres (parameter group, backups, multi-AZ toggle) and ElastiCache Redis (replication group toggle; dev tier default), security groups scoped to the cluster, connection secrets into SSM Parameters, and the values pipeline handing Terraform outputs to Helm.
**Done when:**
- [ ] In-cluster connectivity Job passes against both services
- [ ] Secrets flow Terraform → SSM → K8s Secrets without manual steps (documented pipeline)
- [ ] Cost-relevant sizing toggles (dev vs prod-shaped) parameterized

#### 20.6 — CI/CD deploy pipeline
**Depends on:** 20.5, 20.3
GitHub Actions: build/push images to ECR (sha + semver tags) via OIDC role assumption (no long-lived AWS keys), migration Job gate, `helm upgrade` to staging on main-branch merge, rollback procedure (`helm rollback`) tested and documented.
**Done when:**
- [ ] Merge → staging deploy green end-to-end
- [ ] Failed migration blocks rollout (verified with an injected bad migration)
- [ ] Rollback restores the previous release under load (exercised once, documented)

#### 20.7 — Autoscaling: KEDA on queue depth
**Depends on:** 20.6, 7.2
Install KEDA; `ScaledObject` for workers driven by the Prometheus scaler on our own metrics (`engine_queue_ready_depth` + PEL + delayed — the mandated "Prometheus metrics feeding autoscaling"); API HPA on CPU/RPS; scale-in safety via drain (5.7) + PDBs; min/max replica policy.
**Done when:**
- [ ] Injected load → worker replicas scale out; drain-verified scale-in loses no work
- [ ] Scaling thresholds documented with rationale in ADR-020
- [ ] A recorded scaling event (metrics + replica graph) captured into docs

#### 20.8 — In-cluster observability stack
**Depends on:** 20.6, 7.5
kube-prometheus-stack + tracing backend (Tempo or Jaeger) via Helm; our Grafana dashboards provisioned; OTLP endpoint wiring via ConfigMap; logs as structured stdout → CloudWatch (Fluent Bit addon) noted/enabled.
**Done when:**
- [ ] M7 dashboards render live cluster data
- [ ] One run traced across ≥2 worker **pods** (screenshot committed to docs)
- [ ] Alert rules loaded; one alert test-fired via induced condition

#### 20.9 — Kubernetes crash-resilience demo
**Depends on:** 20.7
The flagship demo, productionized: scripted `make demo-k8s-crash` — start a long-running flagship run, `kubectl delete pod` on the lease-holding worker, watch Kubernetes reschedule while lease reclaim resumes the work; side-effect counters prove zero duplicates; dashboard shows the recovery live. Written up as `docs/demos/k8s-crash.md` with capture.
**Done when:**
- [ ] Demo script idempotent and repeatable against staging
- [ ] Zero duplicate side effects asserted post-run (journal counters)
- [ ] Write-up with timeline (kill → reschedule → reclaim → resume) and dashboard capture

#### 20.10 — Load re-run on EKS
**Depends on:** 20.9, 19.6
Re-run the M19 matrix against staging (mock provider in-cluster): confirm the horizontal-scaling claim (throughput vs worker replica count to the knee), compare against local numbers, note cost per 1k runs, identify the next bottleneck in the cluster context. `BENCHMARKS.md` gains the EKS section.
**Done when:**
- [ ] Adding worker replicas increases throughput near-linearly to a documented knee
- [ ] EKS section published: environment, replica curves, percentiles, cost note
- [ ] Divergences from local results explained (not hand-waved)

---

## Milestone 21 — v1.0 polish, examples, release

**Goal.** Make the project legible to strangers: a polished example gallery, data-retention hygiene, a security pass, an overhauled README/docs with the positioning story and demo captures, and a versioned v1.0 release with published images and chart.

**Role.** The difference between "code that works" and "a project people (and interviewers) can evaluate in ten minutes."

**Architecture docs:** none new; all ADRs reviewed for drift as part of 21.4.

**Exit criteria:** a newcomer goes from clean checkout to a live dashboard demo in under 10 minutes following the README; v1.0.0 tag ships images + chart + changelog; security scanners green.

#### 21.1 — Example gallery
**Depends on:** 14.5, 15.5, 13.4
Polish 3–4 examples spanning the feature set: flagship agent loop with HITL + budgets (exists — polish), RAG pipeline (retrieve → answer → cite with validators), map-reduce document summarizer (planner/map + compaction), and an idempotent-side-effect ops workflow (http tool + approval). Each: definition JSON, builder screenshot, README, one-command mock run + env-gated live mode. All run in CI (mock).
**Done when:**
- [ ] Every example runs green in CI mock mode via one command
- [ ] Each README explains which engine features the example demonstrates
- [ ] Gallery indexed from the main README

#### 21.2 — Data retention & pruning
**Depends on:** 10.2, 7.4
Retention config (terminal-run TTL); pruning job (batched deletes honoring FK order; preserves run aggregates/summary rows per policy) as `ctl prune` + in-cluster CronJob; admin endpoint; prune metrics.
**Done when:**
- [ ] Prune removes only out-of-retention terminal data (property test on generated corpus)
- [ ] Batched execution bounded (no long lock/IO spikes — verified under load)
- [ ] CronJob shipped in the Helm chart, disabled by default

#### 21.3 — Security & hardening pass
**Depends on:** 6.2, 8.7
CI gates: gosec, govulncheck, `pnpm audit`, dependency pinning review. Authz sweep: every route's scope covered by a test (matrix completeness check). Secrets audit: automated assertion that key material never appears in logs/traces/errors. `SECURITY.md` + threat-model notes (SSRF allowlist, template-injection stance, webhook HMAC, WS ticket scope).
**Done when:**
- [ ] All scanners green and gating CI
- [ ] Route×scope matrix test proves no unauthenticated/unscoped route slipped in
- [ ] Threat-model doc reviewed; each identified risk has a mitigation or accepted-risk note

#### 21.4 — README & docs overhaul
**Depends on:** 20.10, 21.1
Rewrite README: positioning table (vs n8n/Zapier, Temporal/Airflow, LangGraph), architecture diagram, 5-minute compose quickstart, demo captures (crash recovery, semantic-retry diff, live dashboard), benchmark headline linking BENCHMARKS.md. Organize `docs/` (ADR index, guides: authoring workflows, writing plugins, ops runbook). ADR drift review. CONTRIBUTING, issue/PR templates, link checker in CI.
**Done when:**
- [ ] Fresh-machine quickstart timed under 10 minutes by following the README verbatim
- [ ] Positioning table states concretely what each alternative lacks (matching this roadmap's preamble)
- [ ] All ADRs reviewed; drift fixed or explicitly amended

#### 21.5 — v1.0 release engineering
**Depends on:** 21.4, 21.3
Semver tagging, changelog generation (git-cliff or equivalent), release workflow publishing binaries, images (GHCR and/or ECR), and the Helm chart (OCI); upgrade notes; version surfaced in API (`/healthz`) and UI footer.
**Done when:**
- [ ] `v1.0.0` tag produces all artifacts via CI, installable per docs
- [ ] Changelog accurate for the release; upgrade notes present
- [ ] Post-release smoke: install released chart + images on kind → flagship example passes

---

# Appendix A — Post-v1 backlog (explicitly deferred)

Ideas validated as out-of-scope for v1, preserved with rationale:

- **Streaming LLM responses** (token streams to the dashboard; executor interface extension).
- **Sub-workflows / child runs** (compose definitions; map fan-out covers part of this).
- **Cron/schedule triggers & external event/signal steps** (wait-for-webhook as a step type).
- **Additional retrieval plugins** (pgvector, external vector DBs) on the M8.8 SPI.
- **Priority queues & per-tenant fairness** (weighted shards; ADR-005 extension).
- **Multi-tenancy** (org isolation, per-tenant quotas/budgets) and **OIDC/JWT auth**.
- **Out-of-process / WASM plugin isolation** (ADR-009 records the in-process decision).
- **Workflow definition migrations for in-flight runs** (currently: runs pin their snapshot).
- **Python/TS SDKs** for code-first workflow authoring against the JSON contract.
- **Exactly-once outbound webhook delivery generalization** (beyond 15.5's journal pattern).
- **Live log streaming channel over WS** (logs are poll-based per 7.4/18.3 in v1).

# Appendix B — Key risks & mitigations

| Risk | Mitigation (built into the plan) |
|---|---|
| Dual-write gaps between Postgres and Redis | Transactional outbox + reconciler (4.4); claims are CAS-guarded so duplicate dispatch is harmless; chaos suites (5.8, 13.5) assert convergence. |
| Zombie workers corrupting state after lease expiry | Fencing tokens on every completion CAS (2.6, 4.5); explicitly tested. |
| Redis data loss (transport layer) | Postgres is the source of truth; reconciler re-outboxes; Redis is redeliverable transport, not a ledger (ADR-005). |
| LLM nondeterminism breaking tests/CI | Mock provider (8.5) + recorded fixtures; live calls only behind env flags; load tests run entirely on mock. |
| Cost blowups (API spend, AWS spend) | Budget enforcement is a core feature (M10); load tests use the mock provider; ADR-020 teardown discipline for AWS. |
| Scope creep toward a generic workflow platform | Non-goals section; AI-native differentiators are milestone-level features, not stretch items. |
| Ticket-scope drift under agent execution | Hard DoD, per-ticket ACs, linear dependencies, and the split-and-record rule in "How to use this roadmap." |
