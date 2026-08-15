# agentloom documentation

- [Architecture overview](architecture.md) — components, execution data flow, tech-stack rationale, glossary.
- [API usage guide](api.md) — curl walkthroughs for the main flows: auth bootstrap, submission + idempotency, pagination, the definition registry, lifecycle steering, DLQ requeue, errors and rate limits. The formal contract is [`api/openapi.yaml`](../api/openapi.yaml) (drift-checked against the router in CI).
- [Architecture decision records](adr/README.md) — the ADR index, template, and conventions.
- [Observability guide](observability.md) — the provisioned Grafana dashboards (Engine, API), the example Prometheus alert rules and their test-fire, key signals, and how to correlate metrics → traces → logs.
- [Edge condition expressions](expressions.md) — the CEL environment for `when`/`condition` predicates, typing rules, and the evaluation-error policy.
- [Plugin SPI guide](plugins.md) — the five plugin kinds, capability flags, and a worked "writing a retriever plugin" walkthrough. The design record is [ADR-009](adr/009-plugin-spi.md).
- [Operations runbook](ops-runbook.md) — operational procedures. Currently: response-cache invalidation (TTL / version bump / admin bust), reading cache stats, disabling the cache. Grows with later milestones. The design record is [ADR-011](adr/011-response-cache.md).
- [`schema/`](schema/) — the generated workflow definition JSON Schema (do not edit by hand: regenerate with `make generate`; CI fails on drift).
- [Canonical example definitions](../examples/definitions/README.md) (repo root, `examples/definitions/`) — the golden workflow fixture corpus: linear pipeline, fan-out/fan-in, conditional branch, critic loop, and a kitchen-sink exercising every construct.
- [ROADMAP.md](../ROADMAP.md) (repo root) — the milestone/ticket build plan and source of truth for sequencing.
- [Progress log](progress.md) — per-ticket implementation history: what each ticket delivered, non-obvious decisions, deferred quirks. Appended as tickets complete.

Planned (arriving with later milestones):

- `demos/` — crash-recovery and feature demo scripts.
- `load/` — load-test methodology and published numbers.
- `examples/` — annotated workflow walkthroughs (the canonical JSON fixtures themselves already live in [`examples/definitions/`](../examples/definitions/) at the repo root).
