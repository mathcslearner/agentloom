-- Retry semantics (ticket 5.2), conforming to ADR-006 (failure taxonomy &
-- retry semantics). Adds the `retrying` step status, the materialized
-- effective retry policy, the durable next-attempt schedule, and the
-- attempt-outcome CHECK ADR-004 postponed until the taxonomy existed.

-- The effective retry policy (explicit fields merged over engine defaults
-- at instantiation, ADR-006 "Where the policy lives at runtime") — the
-- failure path never reparses the definition snapshot, and a worker
-- upgrade cannot change an in-flight run's policy. Existing rows carry no
-- authored policy, so the engine-default policy is their honest frozen
-- value (pre-M5 semantics: the defaults).
ALTER TABLE run_steps ADD COLUMN retry_policy JSONB;
UPDATE run_steps SET retry_policy =
    '{"max_attempts": 3, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "full", "retry_on": ["transient", "timeout"]}'::jsonb;
ALTER TABLE run_steps ALTER COLUMN retry_policy SET NOT NULL;

-- When the next attempt is due — set on running → retrying, cleared at
-- claim. Durable so the reconciler can heal a crash between the failure
-- commit and the delayed-ZSET schedule (ADR-006).
ALTER TABLE run_steps ADD COLUMN next_attempt_at TIMESTAMPTZ;

-- `retrying` joins the step-status vocabulary (constraint recipe:
-- drop/re-add, never ALTER TYPE — ADR-004). `dead_lettered` and
-- `cancelled` deliberately wait for 5.4's migration.
ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ADD CONSTRAINT run_steps_status_check CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped', 'retrying'));

-- Reconciler retry scan: overdue retrying steps by their due time.
CREATE INDEX run_steps_retrying_idx
    ON run_steps (next_attempt_at)
    WHERE status = 'retrying';

-- Attempt outcomes gain the classed vocabulary (ADR-006 "Error classes"):
-- the bare `failed` written by 2.6–4.x is retired — under pre-M5 semantics
-- every failure was terminal, which is what `permanent` now means. Then
-- the CHECK ADR-004 left off lands; NULL stays legal (in-flight attempts).
-- `validation_failed` is deliberately absent until M11 unlocks it.
UPDATE step_attempts SET outcome = 'permanent' WHERE outcome = 'failed';
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_outcome_check CHECK (
    outcome IN ('succeeded', 'transient', 'permanent', 'timeout', 'cancelled', 'lost'));

-- Post-M4 audit note resolved: the reconciler's anti-joins (stale-ready,
-- stale-running pending-row probe, and 5.2's overdue-retrying) probe the
-- outbox by (run_id, step_id); with retry re-dispatch keeping the outbox
-- populated they should not scan the identity PK.
CREATE INDEX task_outbox_run_step_idx ON task_outbox (run_id, step_id);
