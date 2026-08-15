-- One row per execution try. Outcome/error/finished_at are written by the
-- completion transitions (transitions.go, ticket 2.6).

-- name: CreateStepAttempt :one
INSERT INTO step_attempts (run_id, step_id, attempt_no, claim_id, started_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- FinishStepAttempt closes an attempt with its outcome; called by the
-- completion transitions in the same transaction as the step CAS. Usage
-- is the provider's token accounting on a successful llm call (ticket
-- 8.6); NULL for every other outcome and step type. Verdict is the
-- output-validation chain verdict (ticket 11.1, ADR-013), present on a
-- succeeded or validation_failed attempt of a step carrying a validation
-- chain; NULL otherwise. Repair is the structured-output provenance
-- (ticket 11.3), present on an attempt of an llm step that declared an
-- output_format; NULL otherwise.
-- name: FinishStepAttempt :execrows
UPDATE step_attempts
SET outcome = @outcome, error = @error, usage = @usage, verdict = @verdict, repair = @repair, finished_at = @finished_at::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND attempt_no = @attempt_no;

-- name: ListStepAttempts :many
SELECT * FROM step_attempts
WHERE run_id = $1 AND step_id = $2
ORDER BY attempt_no;

-- CountCountedFailures is the durable retry budget (ADR-006 "Retry budget
-- vs. delivery count"): judged attempt failures only — outcomes transient
-- and timeout. `lost` closures are excluded by construction (a crashed
-- host recorded no judgment). Since ticket 5.4 the count starts at the
-- requeue baseline: attempt history is immutable, so a requeued step
-- re-arms its full policy by counting only attempts past the latest
-- dead_letters row's attempts_at_death.
-- name: CountCountedFailures :one
SELECT count(*) FROM step_attempts sa
WHERE sa.run_id = $1 AND sa.step_id = $2 AND sa.outcome IN ('transient', 'timeout')
  AND sa.attempt_no > COALESCE((
      SELECT MAX(dl.attempts_at_death) FROM dead_letters dl
      WHERE dl.run_id = $1 AND dl.step_id = $2), 0);

-- ListRunStepAttempts reads a whole run's attempt history in one query,
-- so the run-detail API (4.6) avoids a per-step round trip.
-- name: ListRunStepAttempts :many
SELECT * FROM step_attempts
WHERE run_id = $1
ORDER BY step_id, attempt_no;
