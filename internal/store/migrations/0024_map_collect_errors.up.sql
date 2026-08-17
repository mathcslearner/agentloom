-- Map collect-errors policy (ticket 13.4b, ADR-015): a map instance that
-- fails terminally under on_item_failure=collect_errors is settled to the new
-- `collected` step status instead of dead-lettering — the run stays alive, the
-- failure is recorded with an error-marker output, and the generated gather
-- collects it as an error slot in the ordered result array.

-- run_steps gains the `collected` terminal status. Distinct from
-- `dead_lettered` (which stops progress and counts against the run) — a
-- collected instance's failure is tolerated by construction, its out-edge to
-- the gather fired, and the run may still succeed.
ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ADD CONSTRAINT run_steps_status_check CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped',
               'retrying', 'dead_lettered', 'cancelled', 'collected'));

-- runs.steps_collected mirrors steps_skipped: a terminal-but-not-failed
-- counter the success rollup tolerates. It is added to the SucceedRun /
-- FailRunRollup / CancelRunRollup all-terminal sums so a run with collected
-- instances rolls up honestly (SucceedRun still requires steps_failed = 0, so
-- collected — unlike dead_lettered — never blocks success). Existing rows
-- default 0.
ALTER TABLE runs ADD COLUMN steps_collected INT NOT NULL DEFAULT 0;
