# agentloom documentation

- [Architecture overview](architecture.md) — components, execution data flow, tech-stack rationale, glossary.
- [Architecture decision records](adr/README.md) — the ADR index, template, and conventions.
- [Edge condition expressions](expressions.md) — the CEL environment for `when`/`condition` predicates, typing rules, and the evaluation-error policy.
- [`schema/`](schema/) — the generated workflow definition JSON Schema (do not edit by hand: regenerate with `make generate`; CI fails on drift).
- [ROADMAP.md](../ROADMAP.md) (repo root) — the milestone/ticket build plan and source of truth for sequencing.

Planned (arriving with later milestones):

- `demos/` — crash-recovery and feature demo scripts.
- `load/` — load-test methodology and published numbers.
- `examples/` — annotated workflow walkthroughs (canonical JSON fixtures live in [`examples/`](../examples/) at the repo root).
