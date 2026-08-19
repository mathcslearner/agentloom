-- Ops views (ticket 18.6): the cross-run dead-letter list and the open-DLQ
-- count that back the operator DLQ page and the /v1/system/stats panel. Both
-- indexes are additive projections of already-persisted data — no new column,
-- no new table.

-- The cross-run DLQ list (GET /v1/dead-letters) orders newest-first with a
-- keyset cursor on (created_at, run_id, step_id, seq), uniformly descending (the
-- runs_created_at_id_idx convention) so the page read is a single index scan and
-- the row-value cursor comparison is index-served.
CREATE INDEX dead_letters_created_idx
    ON dead_letters (created_at DESC, run_id DESC, step_id DESC, seq DESC);

-- The "open" DLQ count (a dead_lettered step awaiting requeue) and the
-- open-only list join both filter run_steps on status = 'dead_lettered'. A
-- partial index keeps that scan proportional to the open backlog, not the run
-- history.
CREATE INDEX run_steps_dead_lettered_idx
    ON run_steps (run_id, step_id)
    WHERE status = 'dead_lettered';
