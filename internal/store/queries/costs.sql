-- Cost ledger (ticket 10.2, ADR-012): per-attempt cost rows plus the run
-- aggregate bump, written together in the attempt-completion transaction.
-- Reads back the ledger and its breakdowns for the cost API.

-- InsertCostLedgerEntry records one cost-bearing attempt's charge. Called
-- inside the success completion transaction, after the fenced CAS, so a
-- zombie completion (which never lands the CAS) never writes a row.
-- name: InsertCostLedgerEntry :exec
INSERT INTO cost_ledger (run_id, step_id, attempt, entry, resource, usage,
                         rate, rate_source, cache_hit, overhead,
                         cost_nano_usd, saved_nano_usd, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- AddRunCost applies one ledger row's spend and savings to the run
-- aggregate, in the same transaction — so runs.spent_nano_usd always equals
-- the exact sum of the run's cost_ledger rows.
-- name: AddRunCost :execrows
UPDATE runs
SET spent_nano_usd = spent_nano_usd + @d_spent::bigint,
    saved_nano_usd = saved_nano_usd + @d_saved::bigint
WHERE id = @run_id;

-- ListCostLedgerByRun reads a run's ledger rows in a stable order for the
-- cost API's per-entry breakdown.
-- name: ListCostLedgerByRun :many
SELECT * FROM cost_ledger
WHERE run_id = $1
ORDER BY step_id, attempt, entry;

-- SumCostLedgerByRun independently sums a run's ledger, so the 10.2 property
-- test can compare it against the materialized runs aggregate.
-- name: SumCostLedgerByRun :one
SELECT COALESCE(SUM(cost_nano_usd), 0)::bigint  AS spent_nano_usd,
       COALESCE(SUM(saved_nano_usd), 0)::bigint AS saved_nano_usd
FROM cost_ledger
WHERE run_id = $1;

-- AggregateCostByStep is the per-step breakdown: spend, savings, and row
-- count grouped by step, ordered by spend descending.
-- name: AggregateCostByStep :many
SELECT step_id,
       COUNT(*)::bigint                         AS entries,
       COALESCE(SUM(cost_nano_usd), 0)::bigint  AS spent_nano_usd,
       COALESCE(SUM(saved_nano_usd), 0)::bigint AS saved_nano_usd
FROM cost_ledger
WHERE run_id = $1
GROUP BY step_id
ORDER BY spent_nano_usd DESC, step_id;

-- AggregateCostByResource is the per-model / per-tool breakdown (ADR-012:
-- the resource name is the model or "tool:<name>"): spend, savings, call
-- count, and summed input/output tokens per resource.
-- name: AggregateCostByResource :many
SELECT resource,
       COUNT(*)::bigint                                                        AS entries,
       COALESCE(SUM(cost_nano_usd), 0)::bigint                                 AS spent_nano_usd,
       COALESCE(SUM(saved_nano_usd), 0)::bigint                                AS saved_nano_usd,
       COALESCE(SUM((usage->>'input_tokens')::bigint), 0)::bigint             AS input_tokens,
       COALESCE(SUM((usage->>'output_tokens')::bigint), 0)::bigint            AS output_tokens
FROM cost_ledger
WHERE run_id = $1
GROUP BY resource
ORDER BY spent_nano_usd DESC, resource;
