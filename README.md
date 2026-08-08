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

Other targets: `make fmt` (format + tidy), `make test-integration` (integration suite; requires the dev stack, harness arriving with ticket 2.2). Run `make` alone to list all targets.

### Dev stack (Postgres + Redis)

Local development and integration tests run against a Docker Compose stack defined in [docker-compose.yml](docker-compose.yml):

```sh
make up         # boot Postgres 16 + Redis 7, wait until both are healthy
make psql       # psql shell inside the postgres container
make redis-cli  # redis-cli shell inside the redis container
make down       # stop the stack — data volumes are KEPT
```

The stack works out of the box with dev-only defaults; to change credentials or host ports (e.g. if 5432/6379 are taken), copy [.env.example](.env.example) to `.env` and edit it — Compose picks it up automatically. `.env` is gitignored; never commit it.

Data lives in named Docker volumes and survives `make down && make up`. To start over from scratch:

```sh
make nuke
```

**`make nuke` is destructive**: it tears down the stack *and deletes all data volumes*. It prompts for confirmation before doing anything.

## Repository layout

Directories marked *(planned)* are placeholders that gain content in later milestones.

```
cmd/            api, worker, ctl, loadgen binaries (planned)
  demo/         throwaway config + logging wiring demo (deleted in M4)
internal/
  version/      build version of agentloom binaries
  config/       env-driven configuration (defaults < env, fail-fast validation)
  dag/          definition types, validation, graph algorithms, CEL
  store/        Postgres repositories, migrations, tx helpers (planned)
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
test/           integration + chaos suites (planned)
api/            openapi.yaml — the REST API contract (planned)
```

## License

[Apache-2.0](LICENSE)
