-- Run-level controls (ticket 5.6), conforming to ADR-004's reserved run
-- statuses and ADR-006 (run cancel / park / deadline semantics). Cancel
-- is cooperative: `cancelling` is the quiescing state (no new claims;
-- in-flight steps settle to `cancelled`), `cancelled` the terminal one.
-- `parked` pauses dispatch — the claim path's run-status guard (5.2)
-- refuses its steps — without touching in-flight work.
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (
    status IN ('running', 'succeeded', 'failed', 'parked', 'cancelling', 'cancelled'));

-- The typed park reason (ADR-004's matrix row): `manual` now,
-- `budget_exceeded` (M10) and `awaiting_human` (M15) reserved. NULL when
-- the run is not parked; unpark clears it.
ALTER TABLE runs ADD COLUMN park_reason TEXT;
ALTER TABLE runs ADD CONSTRAINT runs_park_reason_check CHECK (
    park_reason IN ('manual', 'budget_exceeded', 'awaiting_human'));

-- Why the run was cancelled: `manual` (operator) or `deadline_exceeded`
-- (the reconciler's max_wall_clock sweep). Written by the
-- running/parked → cancelling transition and kept on the terminal row.
ALTER TABLE runs ADD COLUMN cancel_reason TEXT;
ALTER TABLE runs ADD CONSTRAINT runs_cancel_reason_check CHECK (
    cancel_reason IN ('manual', 'deadline_exceeded'));

-- The optional run wall-clock deadline, materialized at instantiation as
-- created_at + the definition's max_wall_clock (same reasons as
-- retry_policy/timeout: the enforcement path never reparses the
-- definition snapshot). NULL means no deadline — the honest value for
-- every pre-5.6 row, so there is no backfill. The partial index is the
-- reconciler's deadline scan: only unfinished runs with a deadline are
-- ever candidates.
ALTER TABLE runs ADD COLUMN deadline_at TIMESTAMPTZ;
CREATE INDEX runs_deadline_scan_idx ON runs (deadline_at)
    WHERE deadline_at IS NOT NULL AND status IN ('running', 'parked');
