-- Reverse 0018. Any validation_failed attempt/dead_letter rows must be gone
-- first (this is a dev/test down migration); the reference DB has none,
-- since a validation failure only ever writes `validation_failed` under the
-- new binary.
ALTER TABLE dead_letters DROP CONSTRAINT dead_letters_class_check;
ALTER TABLE dead_letters ADD CONSTRAINT dead_letters_class_check CHECK (
    class IN ('transient', 'permanent', 'timeout', 'cancelled'));

ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_outcome_check;
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_outcome_check CHECK (
    outcome IN ('succeeded', 'transient', 'permanent', 'timeout', 'cancelled', 'lost', 'throttled', 'budget_exceeded'));

ALTER TABLE step_attempts DROP COLUMN verdict;

ALTER TABLE run_steps DROP COLUMN validation_policy;
