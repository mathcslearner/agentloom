-- Reverse ticket 13.4b: drop the steps_collected counter and remove the
-- `collected` status from the run_steps CHECK. Any `collected` rows must be
-- resolved before the down migration (as with every terminal-status
-- narrowing).
ALTER TABLE runs DROP COLUMN steps_collected;

ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ADD CONSTRAINT run_steps_status_check CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped',
               'retrying', 'dead_lettered', 'cancelled'));
