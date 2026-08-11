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
