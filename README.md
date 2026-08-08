# agentloom

[![CI](https://github.com/mathcslearner/agentloom/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mathcslearner/agentloom/actions/workflows/ci.yml)

**A distributed, durable execution engine purpose-built for AI agent workflows — Temporal-grade distributed-systems guarantees, AI-native orchestration semantics.**

Define workflows as DAGs of steps (LLM calls, tool calls, conditionals, fan-out/fan-in, human approvals); agentloom distributes execution across independent worker processes, persists durable state so runs survive crashes and resume from the last completed step, and provides retries, timeouts, idempotency, and dead-lettering. It sits between **n8n/Zapier** (easy, not production-grade), **Temporal/Airflow** (production-grade, not AI-native), and **LangGraph** (great agent logic, but an in-process library with no distributed coordination or crash recovery) — with AI-native features as first-class primitives: semantic/self-correcting retries, dynamic runtime DAG expansion, cost-aware scheduling with budgets, context/memory management, multi-agent handoff, and human-in-the-loop approvals.

Go module path: `github.com/mathcslearner/agentloom`

## Status

Early development. The engine is being built milestone-by-milestone per [ROADMAP.md](ROADMAP.md); currently in **Milestone 2 — Durable state: Postgres persistence**.

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
make up         # boot Postgres 16 + Redis 7, wait until both are healthy
make psql       # psql shell inside the postgres container
make redis-cli  # redis-cli shell inside the redis container
make down       # stop the stack — data volumes are KEPT
```

The stack works out of the box with dev-only defaults; to change credentials or host ports (e.g. if 5432/6379 are taken by a Postgres/Redis already on your machine), copy [.env.example](.env.example) to `.env` and edit it — both Compose and the Make targets pick it up automatically, so keep the `AGENTLOOM_*_DSN` entries in sync with the ports. `.env` is gitignored; never commit it.

Data lives in named Docker volumes and survives `make down && make up`. To start over from scratch:

```sh
make nuke
```

**`make nuke` is destructive**: it tears down the stack *and deletes all data volumes*. It prompts for confirmation before doing anything.

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

Directories marked *(planned)* are placeholders that gain content in later milestones.

```
cmd/            api, worker, ctl, loadgen binaries (planned)
  demo/         throwaway config + logging wiring demo (deleted in M4)
  migrate/      dev-time schema migration tool (make migrate-up/down/new)
internal/
  version/      build version of agentloom binaries
  config/       env-driven configuration (defaults < env, fail-fast validation)
  dag/          definition types, validation, graph algorithms, CEL
  store/        Postgres persistence: migrations + migrator, storetest/ harness; repositories (planned)
  queue/        Redis Streams, leases, delayed delivery (planned)
  engine/       claim/execute/complete pipeline, outbox, reconciler (planned)
  exec/         executor SPI, registry, middleware, side-effect journal (planned)
  llm/          provider interface: Anthropic, OpenAI, mock (planned)
  tools/        tool SPI + built-ins (planned)
  ratelimit/    Redis token buckets (planned)
  cache/        response cache (planned)
  cost/         pricing catalog, ledger, budget enforcement (planned)
  contextmgr/   token counting, blackboard, assembly, compaction (planned)
  validate/     validator SPI, deterministic + LLM-judge validators (planned)
  api/          HTTP handlers, auth, WS (planned)
  obs/          observability: log/ (slog JSON logger, context helpers); metrics, tracing (planned)
web/            Next.js builder + dashboard (planned)
deploy/         helm/, terraform/, dockerfiles (planned)
docs/           architecture.md + doc index; adr/, demos/, load/ (planned)
examples/       canonical workflow JSON fixtures (definitions/)
test/           cross-cutting integration + chaos suites (smoke/ env checks today)
api/            openapi.yaml — the REST API contract (planned)
```

## License

[Apache-2.0](LICENSE)
