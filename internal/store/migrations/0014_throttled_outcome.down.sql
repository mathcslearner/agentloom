-- Reverse 0014: drop `throttled` from the attempt-outcome CHECK. Any
-- throttled attempt rows must be gone first (this is a dev/test down
-- migration); the reference DB has none, since a throttle only ever writes
-- `throttled` under the new binary.
ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_outcome_check;
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_outcome_check CHECK (
    outcome IN ('succeeded', 'transient', 'permanent', 'timeout', 'cancelled', 'lost'));
