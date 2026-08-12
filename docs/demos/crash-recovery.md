# Crash recovery — the flagship demo

**Run it:** `make demo-crash` (≈3 minutes, self-narrating, exits non-zero
if recovery ever fails to happen).

This demo is the headline guarantee of the engine, live: **a worker
process dies without warning mid-step, and the run completes anyway — no
human intervention, no lost work, no double effects.** It is the
compose-driven twin of the automated suite in [`test/crash`](../../test/crash/)
(run by `make test-integration`), which proves the same scenarios
deterministically in CI with tuned-down timings.

## What happens

The demo drives [`crash-demo.json`](crash-demo.json) — a linear chain
whose `long_task` sleeps for 25 seconds, a window wide enough to kill
things mid-flight on a human timescale — through two acts against the
full compose stack (`--profile app`: api + 2 worker replicas).

**Act 1 — SIGKILL the worker holding a lease.**

1. Submit the run; wait until `long_task` is executing.
2. Find the worker mid-step: the Redis pending-entries list (PEL) *is*
   the lease ledger (ADR-005), so `XPENDING` names the consumer holding
   the entry, and worker logs map that consumer name to a container.
3. `docker kill -s KILL` that container. This is genuine crash death: no
   graceful drain, no ACK — its heartbeats simply stop.
4. Narrate the recovery: after the lease TTL (shortened to 5s for the
   demo) the surviving worker's `XAUTOCLAIM` reclaims the orphaned
   entry, sees the step still `running` under the dead worker's
   `claim_id`, and **takes it over** — closing the dead attempt with the
   administrative outcome `lost`, appending a `step_reclaimed` event, and
   re-executing under a fresh claim.
5. Print the receipt: `long_task`'s attempt history reads
   `lost → succeeded` (two different claim IDs — that pair *is* the
   proof of reclaim), and every other step has exactly one attempt.

**Act 2 — full-stack restart mid-run.**

1. Submit again; let it make real progress (`short_task` completes,
   `long_task` starts).
2. SIGKILL **everything** — the api and every worker at once. Only the
   stores survive, which is the point: Postgres is the source of truth.
3. Boot the stack back up. The fresh workers have new consumer names and
   empty PELs; they reclaim the dead fleet's entries, take over the
   mid-flight step, and finish the run.
4. The receipt shows resume-from-last-completed-step: pre-crash steps
   keep their single attempt — completed work is never re-run.

## Why the recovery works (mechanism map)

| What you see | Mechanism | Where it's specified |
|---|---|---|
| Step stays `running` after the kill | The PEL entry is the lease; Postgres keeps the dead worker's `claim_id` until someone acts | ADR-005 (lease), crash-matrix cell **W3** |
| Survivor picks the step up after ~TTL | `XAUTOCLAIM` with min-idle = lease TTL redelivers the orphaned entry (delivery count > 1 → reclaim path) | ADR-005 reclaim; engine claim classifier (4.5) |
| Attempt 1 becomes `lost`, attempt 2 appears | Lease-expiry takeover: `running → ready → running` under a fresh claim, fenced on the *observed* dead claim | ADR-004 transition matrix, `store.TakeoverStep` (4.5) |
| A resumed zombie could not corrupt anything | Every completion CAS requires the matching `claim_id`; stale writers are rejected and abandon | fencing (4.5) |
| No step's effects fire twice | Duplicate deliveries ACK-drop at the claim CAS; successors are outboxed inside exactly one committed completion | ADR-005 ACK discipline (4.2–4.4) |
| Act 2's frozen run resumes | Redeliver-and-let-the-claim-CAS-decide + outbox re-drain by the fresh fleet | ADR-005 crash matrix (W1–W4, R1) |

Kills are **SIGKILL on purpose**: a graceful stop (SIGTERM) drains the
in-flight handler — that's the orderly-deploy path, and interrupting an
executor gracefully is M5's retry/timeout territory. Crash recovery is
specifically about the worker that never got to say goodbye.

## Knobs

- `DEMO_LEASE_TTL` (default `5s`) — the lease TTL the demo boots the
  workers with (exported as `AGENTLOOM_QUEUE_LEASE_TTL`, which the
  compose file passes through; heartbeat and reclaim intervals derive
  from it). Raise it to make the wait-for-reclaim beat more dramatic.
- The demo loads `.env` exactly like the Makefile and Compose do, so
  remapped ports (`AGENTLOOM_API_PORT`, `AGENTLOOM_POSTGRES_PORT`, …)
  just work.
- Auth (ticket 6.2): every `/v1` route requires a bearer key, so the
  demo authenticates as the stack's root credential — the
  `AGENTLOOM_API_ROOT_KEY` from `.env` if set, otherwise an ephemeral
  one minted at startup and handed to the api container through the
  environment. No key setup is needed to run the demo.

## Prerequisites & troubleshooting

Needs `docker compose`, `go`, `curl`, `jq`, and an otherwise **idle**
stack — the lease-holder lookup expects the demo's step to be the only
pending queue entry, so don't run it while other runs are executing.

- *The stack boots with old images*: the script runs
  `docker compose --profile app up -d --build --wait`, which rebuilds;
  if a stale layer sneaks through, `docker compose --profile app build
  --no-cache` and re-run.
- *"timed out waiting for the reclaim"*: check the surviving worker's
  logs (`docker compose logs worker`) — the reclaim line carries the
  step and both claim IDs. A lease TTL far above `DEMO_LEASE_TTL`
  usually means the workers were not recreated with the new env; re-run
  `make demo-crash` (compose recreates on env change).
- The demo leaves the stack up (`make down` to stop it) and its runs in
  the database — handy for poking at the attempt history afterwards via
  `GET /v1/runs/{id}` or `make psql`.
