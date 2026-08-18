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
| [006](006-failure-taxonomy-and-retries.md) | Failure taxonomy & retry semantics | Accepted | 2026-08-11 |
| [007](007-authn-authz-and-api-rate-limiting.md) | Authentication, authorization & API rate limiting | Accepted | 2026-08-12 |
| [008](008-observability-conventions.md) | Observability conventions | Accepted | 2026-08-12 |
| [009](009-plugin-spi.md) | Plugin SPI — kinds, registration, config schemas, capability flags | Accepted | 2026-08-14 |
| [010](010-rate-limiting-and-backpressure.md) | Fleet-wide rate limiting & backpressure | Accepted | 2026-08-14 |
| [011](011-response-cache.md) | Response cache — key design & invalidation | Accepted | 2026-08-14 |
| [012](012-cost-model.md) | Cost model — attribution, estimation & the pricing catalog | Accepted | 2026-08-14 |
| [013](013-output-validation-and-semantic-retries.md) | Output validation & semantic retries | Accepted | 2026-08-15 |
| [014](014-context-and-memory-model.md) | Context & memory model — token counting, sources, budgets, compaction | Accepted | 2026-08-15 |
| [015](015-dynamic-graph-expansion.md) | Dynamic graph expansion — planner steps, PlanOutput, caps & crash matrix | Accepted | 2026-08-15 |
| [016](016-multi-agent-orchestration.md) | Multi-agent orchestration — agent roles, handoff, loop unrolling | Accepted | 2026-08-16 |
| [017](017-human-in-the-loop.md) | Human-in-the-loop — approval steps, decisions, edits, timeouts | Accepted | 2026-08-17 |
