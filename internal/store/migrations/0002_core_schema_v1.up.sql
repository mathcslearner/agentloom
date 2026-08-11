-- Core schema v1 (ticket 2.3), conforming to ADR-004 (persistence model &
-- state machines). Statuses are TEXT + named CHECK constraints — extended
-- later by dropping and re-adding the constraint (never ALTER TYPE).
-- Lifecycle timestamps (started_at/finished_at/updated_at) are written by
-- the application from its injected clock; only created_at defaults here.

-- Stored-definition registry storage (registry semantics arrive in M6).
-- Rows are immutable; a new version is a new row.
CREATE TABLE workflow_definitions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    version    INT NOT NULL CHECK (version >= 1),
    spec       JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT workflow_definitions_name_version_key UNIQUE (name, version)
);

-- One row per execution. `definition` is the per-run snapshot the run
-- actually executes (ADR-001: snapshots insulate in-flight runs);
-- `definition_id` is bookkeeping back to the registry, null for inline
-- submissions. `next_seq` allocates per-run event sequence numbers.
CREATE TABLE runs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    definition_id     UUID REFERENCES workflow_definitions (id) ON DELETE RESTRICT,
    definition        JSONB NOT NULL,
    status            TEXT NOT NULL,
    params            JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_token TEXT,
    graph_version     INT NOT NULL DEFAULT 1 CHECK (graph_version >= 1),
    next_seq          BIGINT NOT NULL DEFAULT 0,
    steps_total       INT NOT NULL DEFAULT 0,
    steps_succeeded   INT NOT NULL DEFAULT 0,
    steps_failed      INT NOT NULL DEFAULT 0,
    steps_skipped     INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    CONSTRAINT runs_status_check CHECK (status IN ('running', 'succeeded', 'failed'))
);

-- Per-run graph copy, node side. remaining_deps counts unresolved
-- incoming normal edges; fired_deps counts those that resolved fired
-- (ADR-004 dependency bookkeeping — the schema form of dag.ReadySteps).
-- claim_id is the fencing token (M4); graph_version stamps the expansion
-- that introduced the row (1 = instantiation).
CREATE TABLE run_steps (
    run_id         UUID NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    step_id        TEXT NOT NULL,
    step_type      TEXT NOT NULL,
    config         JSONB,
    status         TEXT NOT NULL DEFAULT 'pending',
    remaining_deps INT NOT NULL DEFAULT 0 CHECK (remaining_deps >= 0),
    fired_deps     INT NOT NULL DEFAULT 0 CHECK (fired_deps >= 0),
    claim_id       UUID,
    attempt_count  INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    output         JSONB,
    error          JSONB,
    graph_version  INT NOT NULL DEFAULT 1 CHECK (graph_version >= 1),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    PRIMARY KEY (run_id, step_id),
    CONSTRAINT run_steps_status_check CHECK (
        status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped'))
);

-- Reconciler / stuck-state scans: only in-flight steps, by staleness.
CREATE INDEX run_steps_inflight_idx
    ON run_steps (status, updated_at)
    WHERE status IN ('ready', 'running');

-- Per-run graph copy, edge side. ordinal preserves declaration order
-- (semantic: the branch first-match rule). Endpoints carry no FK to
-- run_steps by design (ADR-004) — expansion revalidates the whole graph
-- in its own transaction.
CREATE TABLE run_edges (
    run_id         UUID NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    ordinal        INT NOT NULL CHECK (ordinal >= 0),
    from_step      TEXT NOT NULL,
    to_step        TEXT NOT NULL,
    edge_type      TEXT NOT NULL DEFAULT 'normal',
    when_expr      TEXT,
    condition      TEXT,
    max_iterations INT,
    resolution     TEXT NOT NULL DEFAULT 'unresolved',
    graph_version  INT NOT NULL DEFAULT 1 CHECK (graph_version >= 1),
    PRIMARY KEY (run_id, ordinal),
    CONSTRAINT run_edges_type_check CHECK (edge_type IN ('normal', 'loop')),
    CONSTRAINT run_edges_resolution_check CHECK (resolution IN ('unresolved', 'fired', 'skipped'))
);

-- One row per execution try. outcome is nullable TEXT with no CHECK yet:
-- v1 writes succeeded/failed plus the administrative `lost` (ticket 4.5's
-- takeover closes a displaced holder's attempt); the full taxonomy is
-- ADR-006's (M5).
CREATE TABLE step_attempts (
    run_id      UUID NOT NULL,
    step_id     TEXT NOT NULL,
    attempt_no  INT NOT NULL CHECK (attempt_no >= 1),
    claim_id    UUID NOT NULL,
    outcome     TEXT,
    error       JSONB,
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    PRIMARY KEY (run_id, step_id, attempt_no),
    FOREIGN KEY (run_id, step_id) REFERENCES run_steps (run_id, step_id) ON DELETE CASCADE
);

-- Append-only audit/UI feed; seq is per-run monotonic, allocated from
-- runs.next_seq in the appending transaction. Never replayed for state.
CREATE TABLE events (
    run_id     UUID NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq        BIGINT NOT NULL CHECK (seq >= 1),
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

-- Transactional Postgres→Redis dispatch buffer (ADR-002). Drained rows
-- are deleted (row exists ⇔ dispatch pending); ascending id is the drain
-- order. Deliberately no FK — dispatch never blocks on run-row churn.
CREATE TABLE task_outbox (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     UUID NOT NULL,
    step_id    TEXT NOT NULL,
    reason     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hot-path indexes on runs: list/rollup by status, idempotent submission.
CREATE INDEX runs_status_created_at_idx ON runs (status, created_at DESC);
CREATE UNIQUE INDEX runs_idempotency_token_key
    ON runs (idempotency_token)
    WHERE idempotency_token IS NOT NULL;
