# agentloom documentation

- [Architecture overview](architecture.md) — components, execution data flow, tech-stack rationale, glossary.
- [Architecture decision records](adr/README.md) — the ADR index, template, and conventions.
- [Edge condition expressions](expressions.md) — the CEL environment for `when`/`condition` predicates, typing rules, and the evaluation-error policy.
- [`schema/`](schema/) — the generated workflow definition JSON Schema (do not edit by hand: regenerate with `make generate`; CI fails on drift).
- [Canonical example definitions](../examples/definitions/README.md) (repo root, `examples/definitions/`) — the golden workflow fixture corpus: linear pipeline, fan-out/fan-in, conditional branch, critic loop, and a kitchen-sink exercising every construct.
- [ROADMAP.md](../ROADMAP.md) (repo root) — the milestone/ticket build plan and source of truth for sequencing.

Planned (arriving with later milestones):

- `demos/` — crash-recovery and feature demo scripts.
- `load/` — load-test methodology and published numbers.
- `examples/` — annotated workflow walkthroughs (the canonical JSON fixtures themselves already live in [`examples/definitions/`](../examples/definitions/) at the repo root).
