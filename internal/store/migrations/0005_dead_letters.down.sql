-- Rollback of dead-letter handling (ticket 5.4). Downgrading collapses
-- the 5.4 statuses onto their pre-5.4 readings: dead_lettered steps
-- return to `failed` (the pre-5.4 terminal failure state — their
-- steps_failed bumps already happened), and written-off `cancelled` steps
-- return to `pending` (pre-5.4 semantics: a failed parent blocks its
-- descendants in permanent pending limbo). The dead_letters records are
-- dropped with the table — this is the lossy half, as with 0003's
-- outcome collapse.

DROP TABLE dead_letters;

UPDATE run_steps SET status = 'failed' WHERE status = 'dead_lettered';
UPDATE run_steps SET status = 'pending' WHERE status = 'cancelled';
ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ADD CONSTRAINT run_steps_status_check CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped', 'retrying'));

ALTER TABLE runs DROP COLUMN steps_cancelled;
ALTER TABLE runs DROP CONSTRAINT runs_on_failure_check;
ALTER TABLE runs DROP COLUMN on_failure;
