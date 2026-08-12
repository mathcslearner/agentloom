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

-- Terminal failure: running → dead_lettered, fenced by claim_id (ticket
-- 5.4, ADR-006 — the conceptual `failed` routing state is passed through
-- inside the completion transaction, exactly like the retry route). error
-- holds the last failure summary; per-attempt detail lives on
-- step_attempts, the full death context on dead_letters.
-- name: DeadLetterRunStep :one
UPDATE run_steps
SET status      = 'dead_lettered',
    error       = @error,
    finished_at = @now::timestamptz,
    updated_at  = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id
  AND status = 'running' AND claim_id = @claim_id
RETURNING *;

-- Poison dead-lettering: any non-terminal status → dead_lettered, with no
-- claim fence — the poison path holds no claim; the entry's handlers kept
-- dying without ever recording a judgment (ticket 5.4, ADR-006 sources
-- table). Clearing claim_id defences any zombie holder: its eventual
-- completion CAS rejects on claim_mismatch and abandons.
-- name: PoisonDeadLetterRunStep :one
UPDATE run_steps
SET status          = 'dead_lettered',
    error           = @error,
    claim_id        = NULL,
    next_attempt_at = NULL,
    finished_at     = @now::timestamptz,
    updated_at      = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id
  AND status IN ('pending', 'ready', 'running', 'retrying')
RETURNING *;

-- Write-off: pending → cancelled, when a dead-lettered upstream step made
-- readiness impossible (ADR-006 continue_independent_branches; 5.6's
-- run-cancel writes the same status with a different event reason).
-- finished_at stays NULL — the step never ran, like skipped.
-- name: CancelRunStep :one
UPDATE run_steps
SET status     = 'cancelled',
    updated_at = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND status = 'pending'
RETURNING *;

-- Requeue: dead_lettered → ready (ticket 5.4, ADR-006 "Requeue"). The
-- step re-arms with its full policy — error and schedule state cleared,
-- attempt history untouched (the budget counts from the dead_letters
-- baseline).
-- name: RequeueRunStep :one
UPDATE run_steps
SET status          = 'ready',
    error           = NULL,
    next_attempt_at = NULL,
    finished_at     = NULL,
    updated_at      = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND status = 'dead_lettered'
RETURNING *;

-- Revival: cancelled → pending, when a requeue made a written-off step's
-- readiness possible again. Dependency counters were never touched by the
-- write-off (edges of a dead step stay unresolved), so the status flip
-- alone restores the pre-write-off state.
-- name: ReviveRunStep :one
UPDATE run_steps
SET status     = 'pending',
    updated_at = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND status = 'cancelled'
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
    steps_skipped   = steps_skipped + @d_skipped::int,
    steps_cancelled = steps_cancelled + @d_cancelled::int
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

-- Rollup: running → failed, immediately — the fail_fast disposition
-- (ADR-006): the guard requires only that some step failed terminally.
-- name: FailRun :one
UPDATE runs
SET status      = 'failed',
    finished_at = @now::timestamptz
WHERE id = @run_id AND status = 'running' AND steps_failed >= 1
RETURNING *;

-- Rollup: running → failed, once every step is terminal — the
-- continue_independent_branches terminalizer (ticket 5.4, ADR-006): the
-- run keeps running while independent branches finish and lands failed
-- when the counters account for every step with at least one terminal
-- failure. Attempted (and its conflict dropped) by every completion
-- transaction, mirroring SucceedRun.
-- name: FailRunRollup :one
UPDATE runs
SET status      = 'failed',
    finished_at = @now::timestamptz
WHERE id = @run_id AND status = 'running' AND steps_failed >= 1
  AND steps_succeeded + steps_failed + steps_skipped + steps_cancelled = steps_total
RETURNING *;

-- Requeue revival: failed → running (ticket 5.4). An operator requeueing
-- a dead-lettered step of a failed run re-opens the run so the claim path
-- admits its steps again; finished_at clears — the run is live.
-- name: ResumeRun :one
UPDATE runs
SET status      = 'running',
    finished_at = NULL
WHERE id = @run_id AND status = 'failed'
RETURNING *;
