-- Guarded state transitions (ticket 2.6): every status change is a
-- conditional UPDATE whose WHERE clause encodes the ADR-004 transition
-- matrix guard. Zero rows updated means the transition lost a race or was
-- illegal — the transition functions in transitions.go re-read the row to
-- return a typed error. These queries are deliberately absent from the
-- public repository interfaces: transitions.go is the only mutation
-- surface, so a status change can never skip its event append.

-- Claim: ready → running, or retrying → running once the step's backoff
-- has elapsed (ticket 5.2 — ADR-006's `retrying → ready → running` hop is
-- realized at claim time; delayed promotion writes nothing to Postgres).
-- The next_attempt_at guard is what makes backoff enforceable: an early
-- duplicate delivery of a retrying step matches nothing and is dropped by
-- the claim classifier. Sets the fresh fencing token, increments the
-- durable attempt counter, and stamps started_at on the first claim only
-- (reclaims and retries keep the original).
-- name: ClaimRunStep :one
UPDATE run_steps
SET status          = 'running',
    claim_id        = @claim_id,
    attempt_count   = attempt_count + 1,
    next_attempt_at = NULL,
    started_at      = COALESCE(started_at, @now::timestamptz),
    updated_at      = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id
  AND (status = 'ready'
       OR (status = 'retrying' AND next_attempt_at <= @now::timestamptz))
RETURNING *;

-- Completion: running → succeeded, fenced by claim_id (a zombie whose
-- lease was reclaimed presents a stale claim and matches nothing).
-- name: SucceedRunStep :one
UPDATE run_steps
SET status      = 'succeeded',
    output      = @output,
    finished_at = @now::timestamptz,
    updated_at  = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id
  AND status = 'running' AND claim_id = @claim_id
RETURNING *;

-- Completion: running → failed, fenced by claim_id. error holds the last
-- failure summary; per-attempt detail lives on step_attempts.
-- name: FailRunStep :one
UPDATE run_steps
SET status      = 'failed',
    error       = @error,
    finished_at = @now::timestamptz,
    updated_at  = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id
  AND status = 'running' AND claim_id = @claim_id
RETURNING *;

-- Retry routing: running → retrying, fenced by claim_id (ticket 5.2,
-- ADR-006 "Step failure lifecycle" — `failed` is a routing state the
-- completion transaction passes through, never left resting). Records the
-- last failure summary, stamps when the next attempt is due, and clears
-- claim_id — a retrying step holds no claim and no lease. finished_at
-- stays NULL: the step is not terminal.
-- name: RetryRunStep :one
UPDATE run_steps
SET status          = 'retrying',
    error           = @error,
    claim_id        = NULL,
    next_attempt_at = @next_attempt_at::timestamptz,
    updated_at      = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id
  AND status = 'running' AND claim_id = @claim_id
RETURNING *;

-- Lease-expiry takeover: running → ready, fenced on the observed holder's
-- claim_id (ADR-005). Clearing claim_id is the moment the zombie loses its
-- fence; guarding on the observed claim closes the ABA window where the
-- step was already taken over and re-claimed by a live worker between
-- observation and this CAS — without it a takeover could steal a live
-- claim.
-- name: TakeoverRunStep :one
UPDATE run_steps
SET status     = 'ready',
    claim_id   = NULL,
    updated_at = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id
  AND status = 'running' AND claim_id = @claim_id
RETURNING *;

-- Edge resolution bookkeeping (not a status transition): the unresolved
-- guard is what makes retried completion transactions idempotent — an
-- already-resolved edge matches nothing, so counters can never
-- double-decrement (ADR-004). Loop edges stay unresolved forever in v1.
-- name: ResolveRunEdge :one
UPDATE run_edges
SET resolution = @resolution
WHERE run_id = @run_id AND ordinal = @ordinal
  AND edge_type = 'normal' AND resolution = 'unresolved'
RETURNING *;

-- name: GetRunEdge :one
SELECT * FROM run_edges WHERE run_id = @run_id AND ordinal = @ordinal;

-- Counter side of an edge resolution, applied to the edge's target step:
-- one resolved incoming edge, fired or skipped (fired_delta 1 or 0). The
-- remaining_deps > 0 guard turns an underflow (an unresolved edge pointing
-- at a fully-drained step — graph corruption) into a diagnosable zero-row
-- result instead of a raw CHECK violation.
-- name: ApplyEdgeResolution :one
UPDATE run_steps
SET remaining_deps = remaining_deps - 1,
    fired_deps     = fired_deps + @fired_delta::int,
    updated_at     = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND remaining_deps > 0
RETURNING *;

-- Readiness: pending → ready when the ADR-004 counter guard holds.
-- join_any is the step's join mode (from its definition config): a
-- `join any` step readies on the first fired edge regardless of
-- remaining_deps.
-- name: ReadyRunStep :one
UPDATE run_steps
SET status     = 'ready',
    updated_at = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND status = 'pending'
  AND fired_deps >= 1 AND (remaining_deps = 0 OR @join_any::boolean)
RETURNING *;

-- Skip propagation: pending → skipped when every incoming normal edge
-- resolved and none fired. finished_at stays NULL — the step never ran.
-- name: SkipRunStep :one
UPDATE run_steps
SET status     = 'skipped',
    updated_at = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND status = 'pending'
  AND remaining_deps = 0 AND fired_deps = 0
RETURNING *;

-- Run aggregate counters, bumped in the same transaction as the step
-- transition they mirror (deltas are 0 or 1).
-- name: BumpRunStepCounters :execrows
UPDATE runs
SET steps_succeeded = steps_succeeded + @d_succeeded::int,
    steps_failed    = steps_failed + @d_failed::int,
    steps_skipped   = steps_skipped + @d_skipped::int
WHERE id = @run_id;

-- Rollup: running → succeeded when every step is terminal and none failed
-- (aggregate-counter form: succeeded + skipped = total, failed = 0; the
-- counters are maintained by the transitions above in the same
-- transactions, so they are trustworthy).
-- name: SucceedRun :one
UPDATE runs
SET status      = 'succeeded',
    finished_at = @now::timestamptz
WHERE id = @run_id AND status = 'running'
  AND steps_failed = 0 AND steps_succeeded + steps_skipped = steps_total
RETURNING *;

-- Rollup: running → failed. The guard is the v1 minimum (some step
-- failed); *when* to halt a run is workflow failure policy (ADR-006, M5).
-- name: FailRun :one
UPDATE runs
SET status      = 'failed',
    finished_at = @now::timestamptz
WHERE id = @run_id AND status = 'running' AND steps_failed >= 1
RETURNING *;
