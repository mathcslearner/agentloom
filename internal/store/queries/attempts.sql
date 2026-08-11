-- One row per execution try. Outcome/error/finished_at are written by the
-- completion transitions (transitions.go, ticket 2.6).

-- name: CreateStepAttempt :one
INSERT INTO step_attempts (run_id, step_id, attempt_no, claim_id, started_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- FinishStepAttempt closes an attempt with its outcome; called by the
-- completion transitions in the same transaction as the step CAS.
-- name: FinishStepAttempt :execrows
UPDATE step_attempts
SET outcome = @outcome, error = @error, finished_at = @finished_at::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND attempt_no = @attempt_no;

-- name: ListStepAttempts :many
SELECT * FROM step_attempts
WHERE run_id = $1 AND step_id = $2
ORDER BY attempt_no;

-- ListRunStepAttempts reads a whole run's attempt history in one query,
-- so the run-detail API (4.6) avoids a per-step round trip.
-- name: ListRunStepAttempts :many
SELECT * FROM step_attempts
WHERE run_id = $1
ORDER BY step_id, attempt_no;
