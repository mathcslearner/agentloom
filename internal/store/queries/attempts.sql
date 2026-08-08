-- One row per execution try. Outcome/error/finished_at are written by the
-- completion transitions (2.6); v1 records them at insert only when the
-- caller already knows them (tests, backfills).

-- name: CreateStepAttempt :one
INSERT INTO step_attempts (run_id, step_id, attempt_no, claim_id, started_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListStepAttempts :many
SELECT * FROM step_attempts
WHERE run_id = $1 AND step_id = $2
ORDER BY attempt_no;
