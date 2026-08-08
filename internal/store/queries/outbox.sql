-- Transactional Postgres→Redis dispatch buffer (ADR-002/004). Drained rows
-- are deleted — row exists ⇔ dispatch pending. The FOR UPDATE SKIP LOCKED
-- drain query arrives with the queue layer (M4).

-- name: CreateOutboxTask :one
INSERT INTO task_outbox (run_id, step_id, reason)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListOutboxTasks :many
SELECT * FROM task_outbox ORDER BY id LIMIT $1;

-- name: DeleteOutboxTasks :execrows
DELETE FROM task_outbox WHERE id = ANY(@ids::bigint[]);
