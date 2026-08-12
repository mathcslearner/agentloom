-- Transactional Postgres→Redis dispatch buffer (ADR-002/004). Drained rows
-- are deleted — row exists ⇔ dispatch pending.

-- name: CreateOutboxTask :one
INSERT INTO task_outbox (run_id, step_id, reason)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListOutboxTasks :many
SELECT * FROM task_outbox ORDER BY id LIMIT $1;

-- Drain batch (ticket 4.4): SKIP LOCKED partitions concurrent drainers
-- onto disjoint row sets, so a row is dispatched by exactly one drainer
-- unless that drainer's transaction rolls back — in which case the retry
-- is a duplicate the claim CAS absorbs (ADR-005 P1).
-- name: ListOutboxTasksForDrain :many
SELECT * FROM task_outbox ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED;

-- name: DeleteOutboxTasks :execrows
DELETE FROM task_outbox WHERE id = ANY(@ids::bigint[]);

-- Backlog snapshot (ticket 7.2): pending-row count plus the oldest row's
-- created_at, one aggregate scan feeding the outbox gauges. Diagnostics
-- only — never an input to drain logic.
-- COALESCE keeps the column NOT NULL for sqlc; the repo maps the epoch
-- placeholder back to "no rows" via the zero backlog.
-- name: OutboxStats :one
SELECT count(*) AS backlog,
       COALESCE(min(created_at), 'epoch'::timestamptz)::timestamptz AS oldest_created_at
FROM task_outbox;
