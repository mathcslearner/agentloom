-- Run-scoped blackboard: shared memory steps read and write during a run
-- (ticket 12.2, ADR-014). The substrate M14's multi-agent handoffs and
-- M12.3's context assembly build on. Append-only per key: a write never
-- overwrites, it inserts the next version, so the full revision history is
-- durable for audit (the ADR-014 requirement compaction and agent threads
-- rest on). Retained for the life of the run and beyond for post-run audit;
-- pruning is M21.
--
-- token_count is the value's size under token_counter (a tokens.Counter
-- fingerprint like 'openai/o200k_base@1' or 'fallback/chars4@1'), stored
-- together so a consumer knows which counter produced the number and can
-- recompute on a fingerprint mismatch (ADR-014). tags is a per-version,
-- immutable label set — re-tagging (including pinning) is a new version, so
-- the history stays honest — and carries the reserved 'pinned' tag no
-- compaction strategy may drop (12.4). author_step_id / author_attempt
-- record which step wrote the version (NULL for a future non-step writer).
CREATE TABLE blackboard_entries (
    run_id         UUID        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    key            TEXT        NOT NULL,
    version        INT         NOT NULL CHECK (version >= 1),
    value          JSONB       NOT NULL,
    token_count    INT         NOT NULL DEFAULT 0 CHECK (token_count >= 0),
    token_counter  TEXT        NOT NULL DEFAULT '',
    tags           TEXT[]      NOT NULL DEFAULT '{}',
    author_step_id TEXT,
    author_attempt INT,
    created_at     TIMESTAMPTZ NOT NULL,
    -- run_id leads so every run-scoped read is a prefix scan; (run_id, key)
    -- groups a key's versions and the head is MAX(version) within the group.
    -- The write path allocates version = head + 1 under the run lock, so two
    -- concurrent writers of the same key can never collide on this key.
    PRIMARY KEY (run_id, key, version)
);

-- Tag queries (List by tag, the API's ?tag= filter) use array-containment
-- (tags @> ARRAY[...]); a GIN index serves them without scanning the run.
CREATE INDEX blackboard_entries_tags_gin ON blackboard_entries USING GIN (tags);

-- The step's authored `blackboard` block (dag.BlackboardPolicy: declarative
-- writes) is materialized at instantiation like config, retry_policy, and
-- cache_policy — the completion path reads the effective policy off the
-- claimed row and never reparses the definition snapshot, and a worker
-- upgrade cannot change an in-flight run's blackboard behavior. NULL when
-- the step authored no `blackboard` block (no declarative writes).
ALTER TABLE run_steps ADD COLUMN blackboard_policy JSONB;
