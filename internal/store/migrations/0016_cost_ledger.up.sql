-- Cost ledger and run-level aggregates (ticket 10.2, ADR-012). One
-- cost_ledger row per cost-bearing attempt completion (an llm attempt or a
-- priced-tool invocation), written in the same transaction as the attempt's
-- success CAS so the run aggregate below always equals the exact sum of the
-- rows. Money is integer nano-USD (ADR-012: 1 USD = 1e9), so the aggregate
-- is an exact integer sum with no floating-point drift.
--
-- entry discriminates the kind of charge: 'attempt' is the productive
-- provider/tool call; ADR-012 rule 4 reserves 'judge' and 'compaction'
-- overhead entries for M11/M12, which attach to the same (run, step,
-- attempt) with overhead = true — so entry is part of the primary key now
-- and those milestones add rows without a schema change. In 10.2 every row
-- is entry = 'attempt', overhead = false.
--
-- rate is the resolved price snapshot (the rate that priced this row, its
-- resolved catalog-entry name, and its effective_from) so historical spend
-- stays auditable against the pricing that was in effect when the run ran,
-- independent of later catalog edits. rate_source records the resolution
-- provenance (exact | wildcard | fallback). A cache hit (ADR-011) is a $0
-- row with cache_hit = true and saved_nano_usd carrying the counterfactual
-- "would-have-cost" figure priced from the stored usage snapshot.
CREATE TABLE cost_ledger (
    run_id         UUID NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    step_id        TEXT NOT NULL,
    attempt        INT  NOT NULL,
    entry          TEXT NOT NULL DEFAULT 'attempt',
    resource       TEXT NOT NULL,
    usage          JSONB,
    rate           JSONB NOT NULL,
    rate_source    TEXT NOT NULL,
    cache_hit      BOOLEAN NOT NULL DEFAULT false,
    overhead       BOOLEAN NOT NULL DEFAULT false,
    cost_nano_usd  BIGINT NOT NULL CHECK (cost_nano_usd >= 0),
    saved_nano_usd BIGINT NOT NULL DEFAULT 0 CHECK (saved_nano_usd >= 0),
    created_at     TIMESTAMPTZ NOT NULL,
    -- The claim-fenced success CAS lands at most one attempt-completion per
    -- (run, step, attempt), so the productive 'attempt' row can never
    -- conflict; the entry discriminator keeps M11/M12 overhead rows distinct
    -- on the same attempt. run_id leads so every run-scoped read is a prefix
    -- scan — no secondary index needed.
    PRIMARY KEY (run_id, step_id, attempt, entry)
);

-- Materialized run aggregates, incremented in the attempt-completion
-- transaction under the run lock every transition already takes. spent is
-- what 10.3's claim-time budget check reads; saved is the cache-savings
-- total 10.5's saved-by-cache metric surfaces. By-step and by-model
-- breakdowns are GROUP BY reads over cost_ledger at API time (same
-- database, exactly consistent) rather than materialized here.
ALTER TABLE runs
    ADD COLUMN spent_nano_usd BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN saved_nano_usd BIGINT NOT NULL DEFAULT 0;
