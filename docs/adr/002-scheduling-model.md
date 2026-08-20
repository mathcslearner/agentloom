# ADR-002: Scheduling model — event-driven, no central scheduler

- **Status:** Accepted
- **Date:** 2026-08-07
- **Ticket:** ticket 0.4

## Context

When a step completes, something must decide which successor steps have become
ready (all dependencies met, edge conditions true, join thresholds reached) and
get them delivered to a worker. The classic answer is a scheduler service that
watches state and assigns work — and it is the classic bottleneck: a single
component every step transition flows through, with its own HA story, and a
third deployable that [ADR-001](001-service-boundaries.md) does not want.

Two constraints shape the decision:

- **Postgres is the source of truth** (project invariant). Readiness is a pure
  function of durable run state — completed steps, evaluated edge conditions,
  join counters — so it can be computed wherever that state is being written.
- Dispatch crosses a process boundary (Postgres → Redis → some worker), and
  dual writes across that boundary can be lost mid-crash; whatever schedules
  must close that gap durably.

## Decision

We will use **event-driven scheduling with no central scheduler**. There is no
scheduler process, elected leader, or scheduling singleton of any kind.

- **The completing worker schedules.** In the same Postgres transaction that
  commits a step's completion (output write + CAS state transition + event
  append), the worker evaluates the step's outgoing edges (CEL conditions),
  decrements successor join counters, marks newly-ready steps, and inserts one
  outbox row per newly-ready step. Readiness computation is atomic with the
  completion that caused it — there is no window where a step is complete but
  its successors' scheduling is undecided.
- **Every worker participates in dispatch.** All workers drain the
  transactional outbox (`FOR UPDATE SKIP LOCKED`) into the `steps:ready` Redis
  Stream, and all workers run the periodic reconciler that heals crash gaps
  (outbox rows never drained, steps stuck in transient states). Dispatch is a
  fleet-wide responsibility, not a role.
- **Run instantiation follows the same path.** The API server marks entry
  steps ready and writes their outbox rows in the run-creation transaction; it
  never talks to Redis directly for dispatch.

### Why this doesn't bottleneck

- **Dispatch capacity scales with the fleet.** Every worker added contributes
  execution *and* scheduling capacity; there is no fixed-size component that
  all step transitions funnel through, and no scheduler to make highly
  available, lead-elect, or fail over.
- **Scheduling work is naturally partitioned by run.** Readiness is computed
  inside per-run completion transactions touching only that run's rows. The
  only serialization points are row-level (e.g. two parallel branches
  decrementing the same join counter), which Postgres row locks handle; there
  is no global lock or global queue-assignment step.
- **The shared path is already load-bearing.** The outbox drain and the ready
  stream are the only fleet-wide shared structures, and both are horizontal:
  `SKIP LOCKED` lets N drainers cooperate without contention, and consumer
  groups spread stream delivery across the fleet.

### Escape criteria — what would justify a dedicated scheduler

A scheduler service becomes justified only if we need scheduling decisions
that require a **global view across runs** and cannot be expressed at claim
time or in per-run completion transactions:

- **Cross-run fairness or priority policies** — e.g. weighted per-tenant
  scheduling or preemption, where which step should run next depends on
  comparing all currently-ready work, not on any property of one step.
  (Simple priorities can be expressed as separate streams claimed in
  preference order without a scheduler; true weighted fairness cannot.)
- **Global admission control** — fleet-wide concurrency ceilings whose
  enforcement needs a consistent global count rather than the token-bucket
  approximations that claim-time checks can provide.

Until a concrete policy of this shape is required, adding a scheduler buys a
bottleneck and an HA obligation for nothing. If one is ever added, it should
own only these global policies — per-run readiness stays in completion
transactions.

### Scale lever: sharded streams

The planned lever when queue/dispatch serialization binds is to **shard the
`steps:ready` stream into K streams by `hash(run_id)`**, with workers
consuming multiple shards; K is config-driven. A single Redis Stream with one
consumer group is the simplest thing that works and is expected to carry the
system a long way; sharding preserves the model (still no scheduler — the
outbox row simply targets a shard) while removing the single-stream ceiling.
Measurement and, if warranted, implementation happen in M19 (ticket 19.5);
the queue protocol details are owned by ADR-005 (M3).

## Consequences

Positive:

- No scheduler deployable, no scheduler HA story, consistent with ADR-001.
- Readiness is transactionally consistent with completion — crash-recovery
  reasoning reduces to "the completion tx committed or it didn't," with the
  outbox + reconciler covering the Postgres→Redis leg.
- Scheduling latency is one outbox drain away from completion, with no
  scheduler queue in between.

Negative:

- **Completion transactions do more work.** Edge evaluation, join-counter
  updates, and outbox inserts ride the hot-path transaction; heavy fan-out
  makes completions costlier. Accepted: it is the price of atomicity, and
  fan-out size is bounded by definition limits (M1).
- **Global policies are hard by construction.** Anything needing a cross-run
  view doesn't fit this model — that is exactly what the escape criteria
  document.
- **Correctness is distributed.** There is no single scheduler to inspect;
  invariants live in the completion tx, outbox, and reconciler, so chaos tests
  (M6) are the primary way to trust the machinery.

## Alternatives considered

- **Central scheduler service.** Rejected: a third deployable (violates
  ADR-001), a throughput bottleneck every transition flows through, and an HA
  problem (leader election or active/active coordination) — all to compute a
  readiness function that the completing worker can evaluate for free inside a
  transaction it is already holding.
- **Database-polling workers (no queue).** Workers poll Postgres for ready
  steps. Rejected: poll-interval latency vs. poll-rate load is a lose-lose
  knob at scale; no delivery/lease semantics, so claim contention lands
  entirely on Postgres; and it forfeits the PEL-based lease ledger, delivery
  counts (poison detection), and consumer-group fan-out that Redis Streams
  provide.
- **Direct enqueue without an outbox.** The completing worker writes Postgres,
  then enqueues to Redis directly. Rejected: a crash between commit and
  enqueue silently strands ready steps — the dual-write gap. The transactional
  outbox makes dispatch a durable consequence of the commit, and the
  reconciler can always rebuild queue state from Postgres (project invariant:
  any Redis loss is recoverable).
