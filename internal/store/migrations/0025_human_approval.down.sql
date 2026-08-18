-- Reverse ticket 15.2: drop the approvals table and remove the
-- `awaiting_human` status from the run_steps CHECK. Any `awaiting_human`
-- rows must be resolved before the down migration (as with every
-- non-terminal-status narrowing).
DROP TABLE approvals;

ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ADD CONSTRAINT run_steps_status_check CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped',
               'retrying', 'dead_lettered', 'cancelled', 'collected'));
