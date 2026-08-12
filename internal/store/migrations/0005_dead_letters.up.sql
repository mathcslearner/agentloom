-- Dead-letter handling (ticket 5.4), conforming to ADR-006 (failure
-- taxonomy & retry semantics). Terminal failures — exhausted retries,
-- permanent class, poison messages — land the step in the new terminal
-- status `dead_lettered` with a durable `dead_letters` record; the run
-- disposition follows the workflow failure policy materialized onto the
-- run row. Postgres is the DLQ: dead-letter records must survive anything
-- Redis can lose, join against attempt history, and support audited
-- requeue.

-- `dead_lettered` (the terminal failure state) and `cancelled` (blocked
-- descendants written off under continue_independent_branches; 5.6's
-- run-cancel writes the same status) join the step-status vocabulary
-- (constraint recipe: drop/re-add, never ALTER TYPE — ADR-004). `failed`
-- stays in the CHECK for pre-5.4 rows: it was the terminal failure state
-- until this migration and remains ADR-006's conceptual routing state,
-- but nothing writes it anymore.
ALTER TABLE run_steps DROP CONSTRAINT run_steps_status_check;
ALTER TABLE run_steps ADD CONSTRAINT run_steps_status_check CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped',
               'retrying', 'dead_lettered', 'cancelled'));

-- The workflow failure policy, materialized at instantiation like the
-- steps' retry_policy and for the same reasons (ADR-006 "Where the policy
-- lives at runtime"): the dead-lettering path never reparses the
-- definition snapshot, and a worker upgrade cannot change an in-flight
-- run's disposition. The default is the honest frozen value for existing
-- rows (absent on_failure means fail_fast).
ALTER TABLE runs ADD COLUMN on_failure TEXT NOT NULL DEFAULT 'fail_fast';
ALTER TABLE runs ADD CONSTRAINT runs_on_failure_check CHECK (
    on_failure IN ('fail_fast', 'continue_independent_branches'));

-- Written-off steps join the run aggregates so the rollup stays a counter
-- check (ADR-006: eager write-off over lazy "no runnable work" scans).
ALTER TABLE runs ADD COLUMN steps_cancelled INT NOT NULL DEFAULT 0;

-- The DLQ. One row per dead-lettering; seq is the per-step death count (a
-- step can die, be requeued, and die again — attempt history is immutable,
-- so the requeue budget counts attempts past the latest attempts_at_death
-- baseline instead of resetting a counter). class is the judged ADR-006
-- error class, NULL for poison entries (a poison message was never judged
-- — its handler kept dying). payload preserves the raw envelope contents
-- for poison entries, which matters most when the envelope itself is the
-- poison. created_at defaults like every other created_at in schema v1;
-- the writing transition passes the injected clock explicitly.
CREATE TABLE dead_letters (
    run_id            UUID NOT NULL,
    step_id           TEXT NOT NULL,
    seq               INT NOT NULL CHECK (seq >= 1),
    source            TEXT NOT NULL,
    class             TEXT,
    error             JSONB,
    payload           JSONB,
    attempts_at_death INT NOT NULL CHECK (attempts_at_death >= 0),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, step_id, seq),
    FOREIGN KEY (run_id, step_id) REFERENCES run_steps (run_id, step_id) ON DELETE CASCADE,
    CONSTRAINT dead_letters_source_check CHECK (
        source IN ('retries_exhausted', 'permanent', 'poison')),
    CONSTRAINT dead_letters_class_check CHECK (
        class IN ('transient', 'permanent', 'timeout', 'cancelled'))
);
