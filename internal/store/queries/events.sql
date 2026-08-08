-- Append-only audit/UI feed (ADR-004): insert and range-read only. No
-- UPDATE/DELETE queries exist by design — append-only is enforced by this
-- API surface, not by triggers. seq comes from AllocateEventSeq (runs.sql)
-- in the same transaction.

-- name: AppendEvent :one
INSERT INTO events (run_id, seq, type, payload)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- ListEvents is the M16 backfill shape: everything after last_seq, in order.
-- name: ListEvents :many
SELECT * FROM events
WHERE run_id = $1 AND seq > $2
ORDER BY seq
LIMIT $3;
