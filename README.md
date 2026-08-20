# agentloom

[![CI](https://github.com/mathcslearner/agentloom/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mathcslearner/agentloom/actions/workflows/ci.yml)

**A distributed, durable execution engine purpose-built for AI agent workflows — Temporal-grade distributed-systems guarantees, AI-native orchestration semantics.**

Define workflows as DAGs of steps (LLM calls, tool calls, conditionals, fan-out/fan-in, human approvals); agentloom distributes execution across independent worker processes, persists durable state so runs survive crashes and resume from the last completed step, and provides retries, timeouts, idempotency, and dead-lettering. It sits between **n8n/Zapier** (easy, not production-grade), **Temporal/Airflow** (production-grade, not AI-native), and **LangGraph** (great agent logic, but an in-process library with no distributed coordination or crash recovery) — with AI-native features as first-class primitives: semantic/self-correcting retries, dynamic runtime DAG expansion, cost-aware scheduling with budgets, context/memory management, multi-agent handoff, and human-in-the-loop approvals.

Go module path: `github.com/mathcslearner/agentloom`

## Status

Actively developed. The core engine is implemented: durable run execution with crash recovery, retries, timeouts, idempotency and dead-lettering; distributed rate limiting and response caching; cost budgets; output validation with semantic retries; context/memory management; dynamic DAG expansion; multi-agent orchestration; and human-in-the-loop approvals — behind a REST + WebSocket API, a Next.js visual builder and live execution dashboard, and a Prometheus/Grafana/Jaeger observability stack.

## Getting started

Requires Go 1.26+ and Docker with Compose v2 (for the dev stack).

```sh
make lint   # golangci-lint (auto-installs a pinned version into ./bin)
make test   # unit tests with the race detector
```

Other targets: `make fmt` (format + tidy), `make test-integration` (integration suite; requires the dev stack — see below). Run `make` alone to list all targets.

### Dev stack (Postgres + Redis)

Local development and integration tests run against a Docker Compose stack defined in [docker-compose.yml](docker-compose.yml):

```sh
make up         # boot Postgres 16 + Redis 7 (plus a dedicated chaos Redis), wait until healthy
make up-app     # boot the full stack: stores + migrations + api + 2 workers
make psql       # psql shell inside the postgres container
make redis-cli  # redis-cli shell inside the redis container
make down       # stop the stack — data volumes are KEPT
```

(The `redis-chaos` service on port 6380 exists solely for the sustained chaos suite, which restarts it mid-test — see `make test-chaos-long`.)

`make up-app` (the compose `app` profile) builds the deployable images from [deploy/dockerfiles/Dockerfile](deploy/dockerfiles/Dockerfile), applies migrations via a one-shot job, and publishes the API on `127.0.0.1:8080` (`AGENTLOOM_API_PORT`; loopback-only by default — set `AGENTLOOM_API_BIND=0.0.0.0` to expose it).

Every `/v1` route requires a scoped bearer key (ADR-007). Bootstrap one before first use: generate a root credential into `.env`, boot the stack, mint a real key with it, then export the key for `ctl`:

```sh
echo "AGENTLOOM_API_ROOT_KEY=sk_$(openssl rand 32 | base64 | tr '+/' '-_' | tr -d '=')" >> .env
make up-app
export AGENTLOOM_API_KEY=$(source .env && go run ./cmd/ctl --key "$AGENTLOOM_API_ROOT_KEY" keys create --name dev --scopes submit,read)
```

Then submit and watch a run with the `ctl` CLI:

```sh
go run ./cmd/ctl validate examples/definitions/fanout.json
go run ./cmd/ctl watch "$(go run ./cmd/ctl submit examples/definitions/fanout.json --params '{"topic": "durable execution"}')"
```

(`fanout.json`'s `llm` steps use the built-in deterministic mock provider and its tool/retrieve steps run offline, so the example completes on the stack with no API keys configured. Set `AGENTLOOM_ANTHROPIC_API_KEY` / `AGENTLOOM_OPENAI_API_KEY` and target a real model to run against Anthropic or OpenAI.)

To see the headline crash-recovery guarantee live — a worker SIGKILLed mid-step, the survivor reclaiming and finishing the run, then a full-stack restart resuming mid-run — run the narrated two-act demo against the stack:

```sh
make demo-crash
```

It is documented in [docs/demos/crash-recovery.md](docs/demos/crash-recovery.md). For an AI-native end-to-end example, `make demo-research` runs the flagship research → write → critique pipeline (multi-agent handoff, a critic refinement loop, output validation, budgets, and context compaction) against the stack on a deterministic mock provider — see [docs/examples/research-critic-writer.md](docs/examples/research-critic-writer.md).

The stack works out of the box with dev-only defaults; to change credentials or host ports (e.g. if 5432/6379 are taken by a Postgres/Redis already on your machine), copy [.env.example](.env.example) to `.env` and edit it — both Compose and the Make targets pick it up automatically, so keep the `AGENTLOOM_*_DSN` entries in sync with the ports. `.env` is gitignored; never commit it.

Data lives in named Docker volumes and survives `make down && make up`. To start over from scratch:

```sh
make nuke
```

**`make nuke` is destructive**: it tears down the stack *and deletes all data volumes*. It prompts for confirmation before doing anything.

### Web UI

The web workspace (`web/`) is a Next.js app providing a visual DAG builder — drag-and-drop steps, schema-driven config panels, client-side graph validation, import/export — and a live execution dashboard — the run graph updating in real time over the event WebSocket, a step inspector, a live cost meter, the approval inbox, and operator views (dead-letter queue, queue health, run controls). It talks to the API through a same-origin proxy that holds the bearer key server-side.

```sh
make web-install   # install workspace deps (pnpm via Corepack; first run only)
make up-app        # the API + workers it talks to
make web-dev       # run the app at http://localhost:3000
```

Configure the app's API endpoint and key in `web/app/.env.local` (see `web/app/README.md`).

### Observability

The `obs` compose profile adds Prometheus, Grafana (with provisioned Engine and API dashboards), and Jaeger; the deployables expose Prometheus metrics on an admin listener and export OpenTelemetry traces that propagate through the queue.

```sh
make up-obs   # full stack + Prometheus/Grafana/Jaeger, with trace export on
```

See [docs/observability.md](docs/observability.md) for the dashboards, example alert rules, and how to correlate metrics → traces → logs.

### Migrations & integration tests

Schema migrations are SQL files in `internal/store/migrations/`, embedded into the binaries and applied with [golang-migrate](https://github.com/golang-migrate/migrate):

```sh
make migrate-up                       # apply all pending migrations
make migrate-down                     # roll back the most recent one (one step)
make migrate-new name=add_runs_table  # create the next NNNN_<name>.{up,down}.sql pair
```

The target database comes from `AGENTLOOM_POSTGRES_DSN` (default: the dev stack). If a migration run dies mid-apply, the database is flagged dirty and further runs refuse with instructions; after fixing the database by hand, clear the flag with `go run ./cmd/migrate force <version>`.

Integration tests carry the `integration` build tag and run against the dev stack:

```sh
make up
make test-integration
```

Each test gets its own throwaway database via `internal/store/storetest` (created, migrated, and dropped per test), so parallel tests never share state. Set `AGENTLOOM_TEST_POSTGRES_DSN` / `AGENTLOOM_TEST_REDIS_ADDR` (see `.env.example`) if the stack is not on the default ports.

## Repository layout

```
cmd/
  api/          REST + WebSocket API server (auth, run/definition lifecycle, plugins, event streaming)
  worker/       step-execution worker (claim, execute, complete, dispatch, reconcile, lease heartbeats)
  ctl/          operator CLI: validate / submit / watch / keys / runs / plugins / cache / approvals / ...
  migrate/      schema migration tool (make migrate-up/down/new)
internal/
  version/      build version of agentloom binaries
  config/       env-driven configuration (defaults < env, fail-fast validation)
  dag/          definition types, validation, graph algorithms, CEL, input templating, dynamic-expansion contract
  store/        Postgres persistence: migrations, sqlc repositories, WithTx, run instantiation, guarded CAS transitions
  queue/        Redis Streams: producer/consumer, leases, delayed delivery, queuetest/ chaos harness
  engine/       claim/execute/complete pipeline, executor middleware chain, outbox dispatcher, reconciler, fencing, control surface
  exec/         executor SPI, registry, built-in executors, side-effect journal, per-step log capture
  plugin/       shared plugin registry + manifest model (the five plugin kinds)
  llm/          model-provider interface + Anthropic, OpenAI, and deterministic mock providers; model routing
  tools/        tool SPI + built-ins (http_request, json_transform)
  retrieval/    retriever SPI + Postgres full-text reference backend
  validate/     validator SPI + deterministic validators and the llm_judge
  jsonrepair/   deterministic JSON repair for structured-output modes
  tokens/       token counting (tiktoken + provider estimators)
  contextmgr/   context assembly and compaction (deterministic + summarization)
  blackboard/   run-scoped shared memory store
  cost/         pricing catalog, cost ledger, budget enforcement
  ratelimit/    Redis token buckets (shared by API rate limiting and fleet resource limits)
  limits/       resource-limit configuration
  cache/        response cache + cache-key builder
  notify/       outbound approval-notification webhooks (HMAC-signed)
  event/        event taxonomy, typed payloads, and Redis pub/sub fan-out
  api/          HTTP handlers, auth, wire types, WebSocket endpoints
  obs/          observability: structured logging, Prometheus metrics, OpenTelemetry tracing
web/
  app/          Next.js visual DAG builder + live execution dashboard (App Router, TS strict)
  lib/          pure TS packages: api-client, engine-client (typed event stream), graphdef (serialization boundary)
deploy/         dockerfiles/ (compose app profile), observability/ (Grafana dashboards + Prometheus rules)
docs/           architecture.md, adr/ (decision records), api.md, observability.md, schema/, demos/, examples/
examples/       canonical workflow JSON fixtures (definitions/)
test/           cross-cutting integration and chaos suites
api/            openapi.yaml — the REST API contract (drift-checked in CI)
```

## License

[Apache-2.0](LICENSE)
