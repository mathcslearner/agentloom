-- Run & definition lifecycle API (ticket 6.5).
--
-- idempotency_fingerprint binds an idempotency token to the payload it was
-- first submitted with: the hex SHA-256 over the canonical definition
-- snapshot, the canonicalized params, and the definition ref (post-M4 audit
-- note — replaying a token with a different payload must 409, not silently
-- return the original run). Nullable: rows created before this migration
-- carry no fingerprint and are grandfathered (reuse is allowed unchecked).
ALTER TABLE runs ADD COLUMN idempotency_fingerprint TEXT;

-- Keyset pagination for GET /v1/runs. The list order is uniformly
-- descending — (created_at DESC, id DESC) — so one row-value comparison
-- `(created_at, id) < (cursor_ts, cursor_id)` is exactly "strictly after
-- the cursor in list order" and both indexes serve it directly.
CREATE INDEX runs_created_at_id_idx ON runs (created_at DESC, id DESC);

-- The definition_id filter (list runs of one stored definition). Partial:
-- inline submissions have NULL definition_id and are never matched by the
-- filter.
CREATE INDEX runs_definition_created_idx
    ON runs (definition_id, created_at DESC, id DESC)
    WHERE definition_id IS NOT NULL;
