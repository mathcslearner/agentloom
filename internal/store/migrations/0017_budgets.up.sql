-- Budgets and budget enforcement at claim time (ticket 10.3, ADR-012).
-- Materialize the authored run-level budget and per-step caps so the claim
-- path enforces projected spend before a cost-bearing step runs. Money is
-- integer nano-USD (ADR-012), the same unit as runs.spent_nano_usd (0016),
-- so the projection compare (spent + estimate vs budget) is exact.
--
-- budget_nano_usd is NULLABLE: NULL means unbudgeted (the honest value for
-- every run authored without a budget_usd, and for every pre-10.3 row — no
-- backfill), deliberately distinct from 0 (which would park every
-- cost-bearing step). PATCH /v1/runs/{id}/budget raises it. on_budget_exceeded
-- is the materialized run disposition (like on_failure, 5.4): park the run
-- resumably, or fail the over-budget step. Downgrade (10.4) joins the CHECK
-- when it lands.
ALTER TABLE runs
    ADD COLUMN budget_nano_usd    BIGINT,
    ADD COLUMN on_budget_exceeded TEXT NOT NULL DEFAULT 'park';

ALTER TABLE runs ADD CONSTRAINT runs_on_budget_exceeded_check CHECK (
    on_budget_exceeded IN ('park', 'fail'));

-- The authored per-step budget caps (max_usd, max_tokens), materialized like
-- cache_policy (9.5): the claim-time check reads them off the row rather than
-- reparsing the definition snapshot. NULL when the step authored no `budget`
-- block — only the run budget then applies.
ALTER TABLE run_steps ADD COLUMN budget_policy JSONB;

-- A claim refused because its projected spend would exceed the run budget,
-- when on_budget_exceeded = park, releases the step to `ready` and records
-- the administrative attempt outcome `budget_exceeded` — the third such
-- outcome after `lost` (4.5) and `throttled` (9.2), likewise outside
-- ADR-006's error-class taxonomy: never counted against the retry budget,
-- never judged by classifyFailure. The claim path decides it structurally,
-- before the executor runs. `run_events.type` is free-form TEXT in schema v1
-- (status.go is its vocabulary), so the `budget_exceeded` run event needs no
-- DDL. The value is new, so there are no rows to backfill (the constraint
-- recipe: drop/re-add, never ALTER TYPE — ADR-004).
ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_outcome_check;
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_outcome_check CHECK (
    outcome IN ('succeeded', 'transient', 'permanent', 'timeout', 'cancelled', 'lost', 'throttled', 'budget_exceeded'));
