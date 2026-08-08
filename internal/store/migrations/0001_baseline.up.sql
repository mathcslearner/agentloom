-- Baseline migration proving the migration machinery end-to-end (ticket 2.2).
-- The marker table gives the up/down/dirty tests something observable; the
-- real schema v1 arrives as subsequent migrations in ticket 2.3.
CREATE TABLE schema_baseline (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    note TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
