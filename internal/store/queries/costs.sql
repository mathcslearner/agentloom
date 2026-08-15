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
-- the exact sum of the run's cost_ledger rows. It RETURNs the post-bump run
-- totals and budget so ApplyAttemptCost can carry them on the cost_updated
-- event (ticket 10.5): because the bump and the event append share the run
-- lock and the same monotonic seq, the totals a run's cost_updated events
-- report are non-decreasing in seq order by construction. Empty result set
-- (no such run) is a caller error, checked in Go.
-- name: AddRunCost :one
UPDATE runs
SET spent_nano_usd = spent_nano_usd + @d_spent::bigint,
    saved_nano_usd = saved_nano_usd + @d_saved::bigint
WHERE id = @run_id
RETURNING spent_nano_usd, saved_nano_usd, budget_nano_usd;

-- SumCostLedgerByStep is a step's cumulative attributed spend so far (all
-- attempts, all entries). The claim-time budget check (ticket 10.3) reads it
-- to enforce a step-level max_usd cap against the step's own running total —
-- so a step whose retries would push its total over the cap is refused before
-- the next attempt runs.
-- name: SumCostLedgerByStep :one
SELECT COALESCE(SUM(cost_nano_usd), 0)::bigint AS spent_nano_usd
FROM cost_ledger
WHERE run_id = $1 AND step_id = $2;

-- SetRunBudget raises (or sets) a run's spend budget (ticket 10.3: PATCH
-- /v1/runs/{id}/budget). Guarded to non-terminal runs so a settled run's
-- budget is immutable; the Go wrapper takes the run lock and appends the
-- run_budget_updated event in the same transaction.
-- name: SetRunBudget :one
UPDATE runs
SET budget_nano_usd = @budget_nano_usd::bigint
WHERE id = @run_id
  AND status IN ('running', 'parked', 'cancelling')
RETURNING *;

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
