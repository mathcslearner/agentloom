-- Reconciler scans (ticket 4.4): the periodic healer's read side. All of
-- these are point-in-time diagnostics over durable state — the reconciler
-- re-outboxes or flags; it never transitions state (ADR-005: every
-- recovery is "redeliver and let the claim CAS decide" or "re-outbox from
-- Postgres state").

-- Steps stuck in ready past a staleness threshold with no pending dispatch
-- row — ADR-005 crash cells P2/R1(a): the stream entry was lost after the
-- outbox row was drained. The anti-join keeps the sweep idempotent: a step
-- with a pending task_outbox row is one drain away from dispatch and needs
-- nothing. Served by ADR-004's partial index on (status, updated_at).
-- name: ListStaleReadySteps :many
SELECT rs.run_id, rs.step_id, rs.updated_at
FROM run_steps rs
WHERE rs.status = 'ready'
  AND rs.updated_at < @stale_before::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM task_outbox o
      WHERE o.run_id = rs.run_id AND o.step_id = rs.step_id)
ORDER BY rs.updated_at
LIMIT @row_limit;

-- Steps running past a staleness threshold ≫ lease TTL — ADR-005 R1(c):
-- a dead worker whose PEL entry Redis lost, so no reclaim will ever fire.
-- updated_at moves on transitions, not heartbeats, hence the generous
-- threshold. Flag-only in 4.4; the takeover heal lands with 4.5.
-- name: ListStaleRunningSteps :many
SELECT rs.run_id, rs.step_id, rs.updated_at, rs.claim_id
FROM run_steps rs
WHERE rs.status = 'running'
  AND rs.updated_at < @stale_before::timestamptz
ORDER BY rs.updated_at
LIMIT @row_limit;

-- Runs still running with no live (pending/ready/running) step — an
-- impossible state: the run rollup is atomic with the transition that
-- terminalizes the last step, so observing this means corrupt state or an
-- engine bug. Flag-only, loudly.
-- name: ListStalledRuns :many
SELECT r.id
FROM runs r
WHERE r.status = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM run_steps rs
      WHERE rs.run_id = r.id
        AND rs.status IN ('pending', 'ready', 'running'))
ORDER BY r.created_at
LIMIT @row_limit;

-- Fleet-wide mutual exclusion for the reconciler sweep: an advisory lock
-- scoped to the surrounding transaction. try = a losing worker skips its
-- sweep instead of queueing behind the winner (no thundering herd).
-- name: TryAdvisoryXactLock :one
SELECT pg_try_advisory_xact_lock(@lock_key::bigint) AS acquired;
