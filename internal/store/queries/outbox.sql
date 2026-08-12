-- Transactional Postgres→Redis dispatch buffer (ADR-002/004). Drained rows
-- are deleted — row exists ⇔ dispatch pending.

-- name: CreateOutboxTask :one
-- trace_parent/trace_state (ticket 7.3) carry the enqueuing span's context
-- when the writer runs inside one; NULL rows fall back to the run's durable
-- root context at drain time.
INSERT INTO task_outbox (run_id, step_id, reason, trace_parent, trace_state)
VALUES ($1, $2, $3, sqlc.narg('trace_parent')::text, sqlc.narg('trace_state')::text)
RETURNING *;

-- name: ListOutboxTasks :many
SELECT * FROM task_outbox ORDER BY id LIMIT $1;

-- Drain batch (ticket 4.4): SKIP LOCKED partitions concurrent drainers
-- onto disjoint row sets, so a row is dispatched by exactly one drainer
-- unless that drainer's transaction rolls back — in which case the retry
-- is a duplicate the claim CAS absorbs (ADR-005 P1).
-- The runs join (ticket 7.3) supplies the envelope's trace context:
-- the row's own context when its writer stamped one, else the run's
-- durable root context — how healed re-dispatches stay in the run trace.
-- FOR UPDATE OF t: only outbox rows are locked; the run row is a plain
-- MVCC read, so drains never contend with the run-lock ordering.
-- name: ListOutboxTasksForDrain :many
SELECT t.*,
       COALESCE(t.trace_parent, r.trace_parent) AS effective_trace_parent,
       COALESCE(t.trace_state, r.trace_state)   AS effective_trace_state
FROM task_outbox t
JOIN runs r ON r.id = t.run_id
ORDER BY t.id LIMIT $1
FOR UPDATE OF t SKIP LOCKED;

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
