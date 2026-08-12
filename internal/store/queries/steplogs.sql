-- Per-step log capture (ticket 7.4, ADR-008). Rows are written only by
-- the async buffered writer (internal/exec/steplog); the ring cap and the
-- derived truncation marker are its and the logs API's business — the
-- queries here are deliberately dumb.

-- CreateStepLogs is the writer's batch insert. COPY, like the graph batch
-- inserts: a flush's lines are all-or-nothing with its trim in one
-- transaction, and per-attempt seq uniqueness is guaranteed by the
-- single-writer-per-attempt protocol, so no conflict handling is needed.
-- name: CreateStepLogs :copyfrom
INSERT INTO step_logs (run_id, step_id, attempt, seq, level, message,
                       fields, trace_id, logged_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- TrimStepLogs is the ring-cap enforcement: after a flush, the writer
-- deletes everything at or below max(seq written) - cap. Seq gaps from
-- buffer drops mean slightly fewer than cap rows may remain — acceptable,
-- the cap is an upper bound.
-- name: TrimStepLogs :execrows
DELETE FROM step_logs
WHERE run_id = $1 AND step_id = $2 AND attempt = $3 AND seq <= $4;

-- ListStepLogsPage is the logs API's keyset page: ascending seq
-- (chronological) within one attempt, from strictly after the cursor,
-- restricted to the given level set (the API always passes the full
-- suffix of the severity order — a minimum-level filter).
-- name: ListStepLogsPage :many
SELECT * FROM step_logs
WHERE run_id = $1 AND step_id = $2 AND attempt = $3
  AND seq > $4
  AND level = ANY(@levels::text[])
ORDER BY seq
LIMIT $5;

-- StepLogStats feeds the derived truncation marker: dropped lines =
-- max_seq - stored (every line consumed a seq; absent ones were
-- cap-evicted or dropped under writer backpressure).
-- name: StepLogStats :one
SELECT count(*)::bigint AS stored, COALESCE(max(seq), 0)::bigint AS max_seq
FROM step_logs
WHERE run_id = $1 AND step_id = $2 AND attempt = $3;
