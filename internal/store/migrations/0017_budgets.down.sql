-- Reverse 0017. Any budget_exceeded attempt rows must be gone first (this is
-- a dev/test down migration); the reference DB has none, since a budget park
-- only ever writes `budget_exceeded` under the new binary.
ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_outcome_check;
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_outcome_check CHECK (
    outcome IN ('succeeded', 'transient', 'permanent', 'timeout', 'cancelled', 'lost', 'throttled'));

ALTER TABLE run_steps DROP COLUMN budget_policy;

ALTER TABLE runs DROP CONSTRAINT runs_on_budget_exceeded_check;
ALTER TABLE runs
    DROP COLUMN on_budget_exceeded,
    DROP COLUMN budget_nano_usd;
