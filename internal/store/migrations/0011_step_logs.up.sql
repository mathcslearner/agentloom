-- Per-step log capture (ticket 7.4, ADR-008). One row per captured
-- executor log line, keyed by attempt: retries, reclaims, and takeovers
-- always mint a new attempt number at claim, so each attempt's ring has
-- exactly one writer and seq can be allocated in-process (an atomic
-- counter on the capture handler) without coordination. A zombie's late
-- flush lands under its old attempt number — harmless, preserved
-- diagnostics.
--
-- The ring is size-capped per attempt: the async writer deletes rows with
-- seq <= max(seq) - cap after each flush. The truncation marker is
-- derived, never stored: any line that consumed a seq but is absent —
-- cap-evicted here, or dropped by the writer's bounded buffer under
-- backpressure — surfaces as max(seq) - count(*), which the logs API
-- reports as dropped_lines.
CREATE TABLE step_logs (
    run_id    UUID NOT NULL,
    step_id   TEXT NOT NULL,
    attempt   INT NOT NULL CHECK (attempt >= 1),
    seq       BIGINT NOT NULL CHECK (seq >= 1),
    level     TEXT NOT NULL,
    message   TEXT NOT NULL,
    -- Executor call-site attrs as one JSON object; NULL when the line
    -- carried none. The canonical correlation fields (run_id/step_id/
    -- attempt/trace_id) are columns, never duplicated in here.
    fields    JSONB,
    -- Hex trace id of the attempt span the line was emitted under; NULL
    -- when tracing is off (ADR-008: trace_id joins logs to Jaeger).
    trace_id  TEXT,
    -- The slog record's timestamp (call-site wall clock — diagnostics,
    -- not engine state, so the injectable-clock invariant does not apply).
    logged_at TIMESTAMPTZ NOT NULL,
    -- The PK is exactly the read path's keyset index (ascending seq walk
    -- within one attempt).
    PRIMARY KEY (run_id, step_id, attempt, seq),
    FOREIGN KEY (run_id, step_id) REFERENCES run_steps (run_id, step_id) ON DELETE CASCADE,
    -- Capture canonicalizes slog levels onto this closed vocabulary.
    CONSTRAINT step_logs_level_check CHECK (level IN ('debug', 'info', 'warn', 'error'))
);
