-- Rollback of run-level controls (ticket 5.6). Downgrading collapses the
-- 5.6 run statuses onto their pre-5.6 readings: a parked or cancelling
-- run returns to `running` (pre-5.6 semantics: nothing pauses dispatch,
-- the run is simply live), and a cancelled run becomes `failed` (the
-- nearest pre-5.6 terminal state — its steps' `cancelled` rows are legal
-- since 0005). The reasons and the deadline are dropped with their
-- columns — the lossy half, as with 0003's outcome collapse.

DROP INDEX runs_deadline_scan_idx;
ALTER TABLE runs DROP COLUMN deadline_at;
ALTER TABLE runs DROP CONSTRAINT runs_cancel_reason_check;
ALTER TABLE runs DROP COLUMN cancel_reason;
ALTER TABLE runs DROP CONSTRAINT runs_park_reason_check;
ALTER TABLE runs DROP COLUMN park_reason;

UPDATE runs SET status = 'running' WHERE status IN ('parked', 'cancelling');
UPDATE runs SET status = 'failed', finished_at = COALESCE(finished_at, now())
    WHERE status = 'cancelled';
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (
    status IN ('running', 'succeeded', 'failed'));
