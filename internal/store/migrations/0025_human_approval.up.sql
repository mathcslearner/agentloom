-- Human-in-the-loop park without lease (ticket 15.2, ADR-017): a
-- human_approval step parks the run's branch — holding no lease or worker
-- slot — until a decision (15.3) or the timeout policy (15.4) resumes it.
-- This migration adds the `awaiting_human` step status and the `approvals`
-- table that records one pending decision per parked step.

-- run_steps gains the non-terminal `awaiting_human` status. A step in this
-- status carries no claim (the executor cleared it at park) and an open
-- attempt row (the attempt spans the human wait, closed by the decision);
-- the reconciler treats it as healthy-parked, and the run stays running.
ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ADD CONSTRAINT run_steps_status_check CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped',
               'retrying', 'dead_lettered', 'cancelled', 'collected',
               'awaiting_human'));

-- approvals records one human-approval request per parked step (ADR-017's
-- materialization sketch). The rendered title/description/payload are a
-- snapshot taken at park time — immune to later graph changes. The decision
-- columns (all nullable) are written by 15.3's decide CAS and 15.4's timeout
-- CAS; 15.2 only writes pending rows.
CREATE TABLE approvals (
    id                UUID PRIMARY KEY,
    run_id            UUID NOT NULL,
    step_id           TEXT NOT NULL,
    attempt           INT  NOT NULL,
    status            TEXT NOT NULL DEFAULT 'pending',
    -- Rendered content shown to the approver (8.2 templating resolved).
    title             TEXT        NOT NULL,
    description       TEXT        NOT NULL DEFAULT '',
    payload           JSONB,
    -- Edit constraints carried from the step config (enforced at decide time).
    allowed_decisions TEXT[]      NOT NULL,
    allow_edit        BOOLEAN     NOT NULL DEFAULT FALSE,
    edit_schema       JSONB,
    -- timeout_at is when 15.4's expiry policy fires; NULL = wait indefinitely.
    timeout_at        TIMESTAMPTZ,
    -- Decision fields, written on decision (15.3) or timeout (15.4).
    decision          TEXT,
    edited_payload    JSONB,
    comment           TEXT,
    decided_by        TEXT,
    decided_at        TIMESTAMPTZ,
    decision_source   TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT approvals_status_check CHECK (
        status IN ('pending', 'approved', 'rejected', 'expired', 'cancelled')),
    -- The approval belongs to a real step; cascade with the run's steps.
    CONSTRAINT approvals_step_fk FOREIGN KEY (run_id, step_id)
        REFERENCES run_steps (run_id, step_id) ON DELETE CASCADE
);

-- At most one open approval per step — the unique-pending invariant the
-- executor relies on (a duplicate delivery that raced the park cannot insert
-- a second pending row).
CREATE UNIQUE INDEX approvals_one_pending_per_step_idx
    ON approvals (run_id, step_id) WHERE status = 'pending';

-- The pending-approvals list (15.3 GET /v1/approvals) and the
-- engine_approval_pending gauge sample scan this partial index in id order.
CREATE INDEX approvals_pending_idx
    ON approvals (created_at, id) WHERE status = 'pending';

-- A run's approvals for the run-status view (15.2 criterion: pending
-- approval visible in run status).
CREATE INDEX approvals_run_idx ON approvals (run_id);
