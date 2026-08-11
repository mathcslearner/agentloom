-- Rollback of retry semantics (ticket 5.2). Downgrading loses the classed
-- outcome detail: transient/timeout/cancelled collapse to the pre-M5 bare
-- `failed`, and `retrying` steps re-enter `ready` (their re-dispatch is
-- the reconciler's stale-ready heal after rollback).

DROP INDEX task_outbox_run_step_idx;

ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_outcome_check;
UPDATE step_attempts SET outcome = 'failed'
WHERE outcome IN ('transient', 'permanent', 'timeout', 'cancelled');

DROP INDEX run_steps_retrying_idx;

UPDATE run_steps SET status = 'ready', next_attempt_at = NULL
WHERE status = 'retrying';
ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ADD CONSTRAINT run_steps_status_check CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped'));

ALTER TABLE run_steps DROP COLUMN next_attempt_at;
ALTER TABLE run_steps DROP COLUMN retry_policy;
