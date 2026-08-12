-- Reconciler scans (tickets 4.4 + 4.5): the periodic healer's read side.
-- All of these are point-in-time diagnostics over durable state. The heals
-- the reconciler composes from them live elsewhere: Outbox().Create for
-- the re-outbox, and — since 4.5 — the TakeoverStep transition
-- (transitions.go) for stale-running steps.

-- Steps stuck in ready past a staleness threshold with no pending dispatch
-- row — ADR-005 crash cells P2/R1(a): the stream entry was lost after the
-- outbox row was drained. The anti-join keeps the sweep idempotent: a step
-- with a pending task_outbox row is one drain away from dispatch and needs
-- nothing. Served by ADR-004's partial index on (status, updated_at).
-- Since ticket 5.4 every step scan requires the run to still be running:
-- a failed run legitimately strands ready/running/retrying steps (the
-- fail_fast disposition; the claim path refuses them), and healing those
-- forever would be an infinite re-outbox → deliver → ack-drop churn loop.
-- A requeue that re-opens the run re-outboxes its ready steps itself.
-- name: ListStaleReadySteps :many
SELECT rs.run_id, rs.step_id, rs.updated_at
FROM run_steps rs
WHERE rs.status = 'ready'
  AND rs.updated_at < @stale_before::timestamptz
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.id = rs.run_id AND r.status = 'running')
  AND NOT EXISTS (
      SELECT 1 FROM task_outbox o
      WHERE o.run_id = rs.run_id AND o.step_id = rs.step_id)
ORDER BY rs.updated_at
LIMIT @row_limit;

-- Steps running past a staleness threshold ≫ lease TTL — ADR-005 R1(c):
-- a dead worker whose PEL entry Redis lost, so no reclaim will ever fire.
-- updated_at moves on transitions, not heartbeats, hence the generous
-- threshold — in effect RunningStale is a hard cap on a step's wall-clock
-- execution time (see config.WorkerConfig). Healed since 4.5: takeover +
-- re-outbox. has_pending_outbox lets the healer skip the re-outbox when a
-- pending dispatch row already exists (the P1 shape sustained past the
-- threshold), so the takeover never doubles a pending dispatch.
-- name: ListStaleRunningSteps :many
SELECT rs.run_id, rs.step_id, rs.updated_at, rs.claim_id,
  EXISTS (
      SELECT 1 FROM task_outbox o
      WHERE o.run_id = rs.run_id AND o.step_id = rs.step_id) AS has_pending_outbox
FROM run_steps rs
WHERE rs.status = 'running'
  AND rs.updated_at < @stale_before::timestamptz
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.id = rs.run_id AND r.status = 'running')
ORDER BY rs.updated_at
LIMIT @row_limit;

-- Steps in retrying whose due time passed more than a staleness threshold
-- ago with no pending dispatch row — ADR-006's failure-commit/
-- delayed-schedule crash gap (the worker died, or its Schedule call
-- failed, after committing running → retrying but before the delayed-ZSET
-- write). Same anti-join shape as ListStaleReadySteps, same idempotency: a
-- re-outboxed step stops matching until its row is drained. The heal is an
-- outbox row alone (reason reconcile_retry) — no status change, since the
-- claim CAS accepts a due retrying step directly.
-- name: ListOverdueRetryingSteps :many
SELECT rs.run_id, rs.step_id, rs.next_attempt_at
FROM run_steps rs
WHERE rs.status = 'retrying'
  AND rs.next_attempt_at < @stale_before::timestamptz
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.id = rs.run_id AND r.status = 'running')
  AND NOT EXISTS (
      SELECT 1 FROM task_outbox o
      WHERE o.run_id = rs.run_id AND o.step_id = rs.step_id)
ORDER BY rs.next_attempt_at
LIMIT @row_limit;

-- Runs still running with no live (pending/ready/running/retrying) step —
-- an impossible state: the run rollup is atomic with the transition that
-- terminalizes the last step, so observing this means corrupt state or an
-- engine bug. Flag-only, loudly.
-- name: ListStalledRuns :many
SELECT r.id
FROM runs r
WHERE r.status = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM run_steps rs
      WHERE rs.run_id = r.id
        AND rs.status IN ('pending', 'ready', 'running', 'retrying'))
ORDER BY r.created_at
LIMIT @row_limit;

-- Fleet-wide mutual exclusion for the reconciler sweep: an advisory lock
-- scoped to the surrounding transaction. try = a losing worker skips its
-- sweep instead of queueing behind the winner (no thundering herd).
-- name: TryAdvisoryXactLock :one
SELECT pg_try_advisory_xact_lock(@lock_key::bigint) AS acquired;
