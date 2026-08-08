# Architecture Decision Records

ADRs record the significant design decisions of agentloom: the context that
forced each decision, the decision itself, its consequences, and the
alternatives that were rejected. Implementation must conform to accepted ADRs;
if reality contradicts one, the ADR is updated (or superseded) in the same
change that diverges from it.

Conventions (numbering, statuses, supersession) are documented in the
[template](template.md), which every new ADR starts from. ROADMAP.md
pre-assigns numbers to some future decisions (ADR-003 workflow definition
format, ADR-004 persistence model, ADR-005 queue protocol, ...); those numbers
are reserved and appear here once the ADR is written.

## Index

| ADR | Title | Status | Date |
|---|---|---|---|
| [001](001-service-boundaries.md) | Service boundaries — exactly two long-running deployables | Accepted | 2026-08-07 |
| [002](002-scheduling-model.md) | Scheduling model — event-driven, no central scheduler | Accepted | 2026-08-07 |
| [003](003-workflow-definition-format.md) | Workflow definition format & versioning | Accepted | 2026-08-08 |
| [004](004-persistence-model.md) | Persistence model & state machines | Accepted | 2026-08-08 |
| [005](005-dispatch-lease-protocol.md) | Dispatch & lease protocol (Redis Streams) | Accepted | 2026-08-08 |
