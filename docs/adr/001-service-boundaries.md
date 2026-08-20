# ADR-001: Service boundaries — exactly two long-running deployables

- **Status:** Accepted
- **Date:** 2026-08-07
- **Ticket:** ticket 0.4

## Context

agentloom is a distributed, durable execution engine: it must keep accepting
API traffic while executing workflow steps across independent processes that
can crash and recover. That forces at least one process boundary — execution
capacity and API traffic scale on different axes (queue depth vs. HTTP load),
and a worker crash mid-step must not take the API down, because crash recovery
is the product's core differentiator, not an edge case.

At the same time, this is a monorepo built by a small team with operational
simplicity as a stated goal (fine-grained microservices are an explicit
non-goal). Every additional deployable adds an RPC contract, a deploy
pipeline, an HA story, and a failure mode. The question is where to draw the
line: how many long-running processes, and what lives inside them versus in
shared code?

## Decision

We will build **exactly two long-running deployables**:

1. **API server** (`cmd/api`) — HTTP/REST endpoints, request validation, run
   instantiation, WebSocket fan-out of run events, dashboard/CLI backend.
2. **Worker** (`cmd/worker`) — claims ready steps from the queue, executes them
   through the middleware chain, commits completion transactions, drains the
   outbox, and runs the reconciler. A fleet of identical worker processes is
   the unit of horizontal scaling.

Everything else — the DAG model, leasing, retries, cost accounting, context
management, caching, the tool/agent/retrieval SPI — is **shared internal Go
packages** under `internal/`, compiled into both binaries as needed. The
compatibility surface between the two deployables is the shared datastores
(Postgres schema, Redis stream/key conventions), not RPC contracts.

`cmd/ctl` (operator CLI) and `cmd/loadgen` (load generator) are short-lived
command-line tools, not deployables; they talk to the API like any client.

A third long-running service may only be introduced if the escape criteria in
[ADR-002](002-scheduling-model.md) are met — notably, a dedicated scheduler is
explicitly *not* one of the two deployables.

## Consequences

Positive:

- **Independent scaling on the right axes.** Workers autoscale on queue depth
  (KEDA, M20); the API scales on request load. Neither redeploys the other.
- **Fault isolation where it matters.** Worker crashes are an expected,
  first-class event (leases, reclaim, fencing); the API stays up through them,
  and vice versa.
- **No internal RPC layer.** Coordination happens through Postgres and Redis,
  which the design already requires for durability and dispatch. There are no
  service-to-service APIs to version, mock, or trace across.
- **Trivial dependency management.** Shared packages are versioned by the
  monorepo commit; both binaries are always built from the same tree, so there
  is no skew between "library version in the API" and "in the worker."

Negative:

- **Shared-package changes redeploy both binaries.** A change to `internal/dag`
  ships in both images even if only one uses the changed path.
- **Blast radius of shared bugs.** A defect in a shared package (e.g. the
  store layer) can affect both deployables at once.
- **Schema as contract.** Because coordination is through the datastores,
  Postgres migrations and Redis conventions must stay compatible across a
  rolling deploy of two binaries; this discipline lands on migrations (M2+)
  rather than API versioning.

## Alternatives considered

- **Fine-grained microservices** (separate scheduler, executor, cost service,
  context service, ...). Rejected: each split multiplies deploy pipelines, RPC
  contracts, and partial-failure modes without buying anything at this scale.
  The concerns are separated in code (packages with clear interfaces), which
  preserves the option to extract a service later if a boundary proves real —
  extraction is cheap when the interface already exists; un-splitting deployed
  services is not.
- **Embedded single binary** (API and worker in one process — effectively an
  in-process library, the LangGraph deployment model). Rejected: it ties step
  execution to API process lifetime, so an executor crash or a redeploy kills
  in-flight runs' host process; it forfeits independent scaling; and it makes
  the distributed crash-recovery story — the reason this project exists over
  in-process frameworks — untestable in its real shape.
