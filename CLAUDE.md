# CLAUDE.md — agentloom: Durable Execution Engine for AI Agent Workflows

## What this project is

A distributed, durable execution engine purpose-built for AI agent workflows — **Temporal-grade distributed-systems guarantees, AI-native orchestration semantics.** Users define workflows as DAGs of steps (LLM calls, tool calls, conditionals, fan-out/fan-in, human approvals); the engine distributes execution across independent worker processes, persists durable state so runs survive crashes and resume from the last completed step, and provides retries, timeouts, idempotency, and dead-lettering.

It sits between: **n8n/Zapier** (easy, not production-grade), **Temporal/Airflow** (production-grade, not AI-native), and **LangGraph** (great agent logic, but an in-process library with no distributed coordination or crash recovery).

**AI-native differentiators (core features, not add-ons):** semantic/self-correcting retries (retry LLM steps with critique-augmented prompts), dynamic runtime DAG generation (planner steps inject steps mid-run, durably), cost-aware scheduling (budgets, model downgrade, park-on-exceed), context/memory management (token accounting + automatic compaction), multi-agent handoff (roles + blackboard + bounded critic⇄writer loops), human-in-the-loop approvals, and a pluggable SPI for tools/agents/retrieval.

## Current status

**M0 in progress.** Ticket 0.1 (repo scaffold & tooling) is done: project named **agentloom**, module path `github.com/mathcslearner/agentloom`, directory skeleton, `Makefile` (`lint` / `test` / `test-integration` / `fmt`, pinned golangci-lint auto-installed into `./bin`), golangci-lint v2 config, Apache-2.0 license, README. Ticket 0.2 (CI pipeline) is implemented: `.github/workflows/ci.yml` runs parallel lint (go vet + golangci-lint, version pinned in sync with the Makefile) and `go test -race` jobs on PRs and pushes to main, with setup-go caching and a README status badge; one acceptance box remains open until the red-path check (a deliberately failing test fails CI, verified once then removed) is done on GitHub. Ticket 0.3 (architecture overview document) is done: `docs/architecture.md` (components + Mermaid diagrams, execution data flow, tech-stack rationale, glossary) and the doc index `docs/README.md`. Ticket 0.4 (ADRs) is done: `docs/adr/` with `template.md` (Nygard-style sections, numbering/supersession conventions), ADR-001 (service boundaries: exactly two deployables, shared internal packages, datastores as the compatibility surface), ADR-002 (scheduling model: no central scheduler, completing worker computes readiness in the completion tx, outbox dispatch, escape criteria for a dedicated scheduler, sharded-streams scale lever), and the index `docs/adr/README.md`. Ticket 0.5 (config & structured logging foundation) is done: `internal/config` (env-driven config with `AGENTLOOM_*` vars, defaults < env precedence, injectable env lookup, all invalid values reported in one joined error; `LogConfig` sub-config), `internal/obs/log` (slog JSON/text logger factory, canonical field constants + typed attr helpers for `run_id`/`step_id`/`attempt`/`worker_id`/`trace_id`, nil-safe `Into`/`From`/`With` context helpers), the `cmd/demo` throwaway (deleted in M4; logs the build version on startup), and `internal/version` (ldflags-injected build version, consumed by the real binaries from M4). **M1 in progress.** Ticket 1.1 (ADR-003: workflow definition format) is done: `docs/adr/003-workflow-definition-format.md` decides the JSON contract — top-level shape (`schema_version` int, `name`, `params` declarations, `steps`, `edges`, opaque `ui`), the full step-type catalog with per-type typed `config` (JSON example each; `#` and `.` reserved in step IDs for M13/M14 instance naming and CEL paths), edge semantics (optional `when` CEL, parallel fan-out by default, `branch` = first-match-in-declaration-order firing rule on its out-edges with at most one trailing default), loop edges (`type: loop`, required `condition` + `max_iterations`, graph-minus-loop-edges must be a DAG, `to` must be an ancestor of `from`, executed by unrolling), readiness/skip-propagation/join semantics (`join all`: skipped parents satisfy, skipped only if all parents skipped; `join any`: first success fires; CEL eval errors are recorded failures, never `false`), versioning (integer bumped on breaking changes only; additive fields don't bump; strict unknown-field rejection everywhere except the byte-for-byte round-tripped `ui` subtree), limits table (10k steps / 20k edges / 1 MiB, etc.), and per-ticket enforcement points for 1.2–1.5. Ticket 1.2 (definition types & JSON codec) is done: `internal/dag` with the definition Go structs (typed per-step-type configs behind a `StepConfig` interface + registry; pointer `Temperature` so explicit 0 survives; `Edge.Type` normalized to `normal` on decode, omitted on encode), a hand-rolled strict decoder (`Decode`: schema_version gated first and reported alone; unknown fields recursively, wrong types, bad enums, and missing required keys all collected into one joined error with JSON paths like `steps[2].config.max_tokens`; opaque payload fields compacted for byte-stable re-encoding), a canonical encoder (`Encode`: fixed field order, defaults omitted, no HTML escaping, `ui` subtree spliced byte-for-byte as captured), JSON Schema generation (invopop/jsonschema from the same structs, per-type config union as `oneOf` on Step, committed at `docs/schema/workflow-definition.v1.json`) wired into `make generate` with a CI drift-check step plus an in-package drift test, and a fixture corpus under `internal/dag/testdata/` (valid round-trip fixtures incl. a kitchen-sink of all 11 step types; one invalid fixture per decode-error class). Ticket 1.3 (structural validation) is done: `internal/dag/validate.go` with `Validate` — pure, runs on decoded or programmatically built definitions, reports every violation in one pass as `ValidationIssue`s carrying stable snake_case codes, severity (warnings don't block), and JSON paths, `errors.As`-reachable through the joined error. Rules: step-ID syntax/uniqueness, registered types, edge endpoints exist, per-type required config (llm/planner = model + exactly one of prompt|messages, etc.), loop-edge rules (condition + max_iterations 1..100 required; loop-only fields rejected on normal edges; `when` rejected on loop edges), branch out-edge rules (≥1 out-edge; only a trailing default may omit `when`), at-least-one-entry-step, isolated-step warnings, and the ADR-003 limits table (the 1 MiB size gate lives at the top of `Decode`, reported alone). ADR-003 gained a 1.3 clarifications subsection (orphans = isolated-step warning; reachability needs no 1.3 rule since non-loop cycles are rejected in 1.4). Fixture corpus extended with `internal/dag/testdata/invalid_structural/` (decode-clean, validation-failing; one per rule; count/size limits generated in tests) plus `valid/isolated_step.json` (warning fixture). Ticket 1.4 (graph algorithms) is done: `internal/dag/graph.go` (`Graph` adjacency view via `NewGraph` — loop edges indexed separately, excluded from adjacency; deterministic Kahn `TopoOrder`; `Ancestors` reachability; iterative three-color-DFS cycle finder reporting one finding per back edge with the full step path) and `internal/dag/ready.go` (`Graph.ReadySteps(completed, skipped, failed)` — ADR-003 readiness/skip-propagation/join semantics as a pure function: single topo-order pass for the skip fixpoint, edge resolutions derived from step state (completed→fired, skipped→skipped, failed/pending→unresolved; runtime `when`/branch outcomes enter by seeding the skipped set), non-join and `join all` share the all-resolved-plus-one-fired rule, `join any` fires on first success, failed parents block, results in declaration order, errors on unknown/overlapping IDs and cyclic graphs). `Validate` gained a graph phase (gated on no duplicate-ID/unknown-endpoint issues) with codes `cycle_detected` and `loop_edge_not_ancestor`; ADR-003 gained a 1.4 clarifications subsection (cycle-reporting granularity, step-level ReadySteps API with per-edge outcomes as the engine seam). Tests: property-based via `pgregory.net/rapid` (random DAG progressions checked against a naive fixpoint reference; monotonicity, no-unmet-deps, termination; topo-order and cycle-rejection properties), fixtures `invalid_structural/{cycle,loop_edge_not_ancestor}.json`, and a 10k-step/19.8k-edge synthetic benchmark — Validate ~9ms + ReadySteps ~0.6ms, with a <100ms gate test (skipped under `-race`). Ticket 1.5 (CEL edge conditions) is done: `internal/dag/cel.go` (cel-go v0.31) — shared `sync.OnceValues` environment declaring exactly `output` (dyn) and `run` (`map(string, dyn)`, only `run.params` populated; standard CEL lib/macros, no custom functions), `CompileExpr` (compile + typecheck; joined `*ExprError`s with 1-based line:col; result type must be bool-or-dyn else `*ExprNotBoolError`), and `CompiledExpr.Eval(output, params)` returning the routing bool or a typed `*EvalError` (missing field, type error, or dyn-non-bool result; per ADR-003 never coerced to `false` — the engine's completion tx (M4) calls it and records failures; classification is ADR-006's). `Validate` compiles every normal-edge `when` and loop-edge `condition` (skipping over-length expressions, already rejected; ~46µs/expr compile cost, benchmarked) with new codes `invalid_expression` (one issue per CEL error, position-prefixed message) and `expression_not_boolean`. Environment documented in `docs/expressions.md` (indexed in `docs/README.md`); ADR-003 gained a 1.5 clarifications subsection. Fixtures: `invalid_structural/{when_syntax_error,when_undeclared_ref,when_not_boolean,condition_syntax_error}.json`, an expression error added to `multi_error_structural.json`, and `valid/expressions.json` (has()/run.params/loop-condition kitchen-sink). All further implementation happens by executing the roadmap; next ticket is 1.6 (canonical example definitions & golden fixtures). The only open M0 item is the one-time CI red-path verification on GitHub (ticket 0.2).

## How to work on this project

1. **`ROADMAP.md` is the source of truth for sequencing.** 22 milestones (M0–M21), 134 tickets, strictly linear — dependencies only point backwards. Pick the first unfinished ticket, complete it fully, move on. Do not skip ahead or start milestones out of order.
2. **One ticket = one focused session** (1–4h, ~a couple hundred LOC). If a ticket balloons, split it and record the split in ROADMAP.md.
3. **Definition of Done (every ticket):** lint + tests green (integration tests when crossing process/datastore boundaries); tests written in the same ticket as the behavior; structured logs (and metrics/traces once M7 exists) on new hot paths; ADRs updated if a decision changed; no secrets anywhere; no TODOs without a ticket reference.
4. **Status convention:** check off acceptance-criteria boxes in ROADMAP.md as you complete them; append ` ✅` to the ticket heading when all are checked.
5. **ADRs govern design.** Most milestones open with an ADR ticket; implementation must conform. If reality contradicts an ADR, update the ADR and affected future tickets in the same change. ADRs live in `docs/adr/`.
6. **Testing discipline:** unit tests always; integration tests (build tag `integration`) run against the Docker Compose stack (`make test-integration`); chaos/crash tests are first-class deliverables, not optional extras.

## Architecture summary

Two long-running deployables + shared internal packages (ADR-001). No central scheduler (ADR-002): scheduling is event-driven — the worker that completes a step computes successor readiness in the same transaction and dispatches via a transactional outbox.

```mermaid
flowchart LR
  UI[Next.js builder + dashboard] -->|REST / WS| API[API server]
  CLI[ctl] -->|REST| API
  API -->|writes runs, reads state| PG[(Postgres\nsource of truth)]
  API -->|WS fan-out| UI
  PG -->|outbox drain| RS[(Redis Streams\nready queue + PEL leases)]
  RS -->|claim + heartbeat| W1[Worker]
  RS --> W2[Worker N]
  W1 -->|CAS transitions, outputs, events| PG
  W1 -->|LLM/tool/retrieval calls| EXT[Anthropic / OpenAI / tools]
  W1 -->|rate limits, cache, delayed queue, pub/sub| RD[(Redis)]
```

**Execution flow:** submit → validate → instantiate run in one tx (per-run graph copy, entry steps `ready`, outbox rows) → outbox drained to Redis Streams → a worker claims via Postgres CAS (`ready → running`, fresh `claim_id`) → executes through the middleware chain → completion tx (output + CAS + evaluate CEL edges + decrement join counters + outbox newly-ready steps + events) → ACK.

**Key mechanisms:**
- **Leases:** the Redis Streams pending-entries list (PEL) is the lease ledger. Heartbeat = `XCLAIM JUSTID` to self; reclaim = `XAUTOCLAIM` with min-idle = lease TTL; poison detection via delivery count. Postgres `claim_id` fencing rejects zombie writes. At-least-once delivery + CAS-guarded claims + idempotency keys = effectively-once execution.
- **Outbox + reconciler:** every Postgres→Redis handoff goes through a transactional outbox drained by all workers (`FOR UPDATE SKIP LOCKED`); a periodic reconciler heals crash gaps and stuck states.
- **Executor middleware chain** (order matters): cache read → rate limit (fleet-wide Redis token buckets; throttled = delayed requeue, not a failure) → budget check (park/downgrade) → context assembly + compaction → execute → validate (semantic retries on `validation_failed`) → cost ledger → cache write.
- **Per-run graph copy:** each run owns its graph rows, so planner steps mutate the graph atomically with their completion (`ExpandRun`, `graph_version++`). Loops are authored as marked loop edges and executed by unrolling iterations through the same expansion machinery — the instance graph stays acyclic.
- **Park/resume:** one primitive underlies manual pause, budget-exceeded halts, and human approvals (approvals ACK the queue message — no lease or worker slot is held while waiting).
- **Events:** append-only per-run event log with monotonic `seq` in Postgres (truth), Redis pub/sub for low-latency fan-out, WS protocol = snapshot → backfill from `last_seq` → live tail.

## Tech stack

| Area | Choice |
|---|---|
| Backend | Go; chi (HTTP), coder/websocket, pgx v5 + sqlc, golang-migrate, go-redis, cel-go (expressions), invopop/jsonschema (schema generation), slog |
| Data | Postgres (durable state, source of truth), Redis (streams/leases, token buckets, delayed ZSET, cache, pub/sub) |
| LLM | Anthropic + OpenAI behind one provider interface; deterministic mock provider for tests and load |
| Observability | Prometheus client_golang, OpenTelemetry (trace context propagated through queue envelopes), Grafana + Jaeger in compose |
| Frontend | Next.js (App Router, TS strict), React Flow (`@xyflow/react`), zustand, Tailwind + shadcn/ui, elkjs, openapi-typescript, vitest + Playwright |
| Infra | Docker multi-stage → Docker Compose (local, from M2) → Helm on EKS, RDS, ElastiCache, ECR, Terraform, KEDA (queue-depth autoscaling), GitHub Actions |

## Planned repository layout

```
cmd/            api, worker, ctl, loadgen
internal/
  version/      build version of agentloom binaries (ldflags-injected)
  dag/          definition types, validation, graph algorithms, CEL
  store/        Postgres repositories (sqlc), migrations, tx helpers
  queue/        Redis Streams, leases, delayed delivery, chaos harness
  engine/       claim/execute/complete pipeline, outbox, reconciler
  exec/         executor SPI, registry, middleware, side-effect journal
  llm/          provider interface, Anthropic, OpenAI, mock
  tools/        tool SPI + built-ins (http_request, json_transform)
  ratelimit/    Redis token bucket (shared by API + fleet limits)
  cache/        response cache, key builder
  cost/         pricing catalog, ledger, budget enforcement
  contextmgr/   token counting, blackboard, assembly, compaction
  validate/     validator SPI, deterministic + LLM-judge validators
  api/          HTTP handlers, auth, WS
  obs/          logging, metrics, tracing
  config/
web/            Next.js app (lib/graphdef = serialization boundary)
deploy/         helm/, terraform/, dockerfiles
docs/           architecture.md, adr/, demos/, load/, examples/
examples/       definitions/ (canonical workflow JSON fixtures)
test/           integration + chaos suites
api/openapi.yaml
```

## Non-negotiable invariants

- **Postgres is the source of truth**; Redis is redeliverable transport + coordination. Any Redis data loss must be recoverable via the reconciler.
- **Every state transition is a guarded CAS** appending an event in the same transaction. Completion requires the matching `claim_id` (fencing).
- **Enqueue is at-least-once; claims dedupe.** Duplicate deliveries are ACK-and-drop, never double-execution.
- **Side effects go through idempotency keys** (stable across attempts/reclaims) and/or the side-effect journal.
- **Graph mutations are atomic with planner completion** — no observable half-expanded state, ever.
- **No metric labels with unbounded cardinality** (`run_id`/`step_id` are log/trace fields, never metric labels).
- **No secrets in code, fixtures, logs, traces, or state.** Provider keys via env/config only.
- **Time is injectable** — no bare `time.Now()`/sleeps in logic under test; chaos and backoff tests use controlled clocks.
- **The workflow definition JSON is a versioned contract** — Go structs are the source of truth; JSON Schema is generated; the UI serialization module round-trips unknown fields.

## Scope boundaries

Not building: model training/fine-tuning, LLM/tool implementations (real provider APIs via the SPI), a generic non-AI automation platform, fine-grained microservices (exactly two deployables), multi-tenancy in v1. See ROADMAP.md non-goals and Appendix A backlog.

## Glossary

**run** — one execution of a workflow definition (owns a graph copy) · **step** — node in a run's graph · **attempt** — one execution try of a step · **lease** — a worker's time-bound claim on a step (PEL entry + heartbeat) · **claim_id** — fencing token guarding state writes · **outbox** — transactional Postgres→Redis dispatch buffer · **reconciler** — periodic healer for crash gaps · **blackboard** — run-scoped shared memory store · **expansion** — atomic runtime graph mutation (planners, map, loop unrolling) · **semantic retry** — re-attempt with critique-augmented prompt after output validation fails · **park** — pause a run without holding leases (manual / budget / approval).
