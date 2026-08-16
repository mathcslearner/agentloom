-- Per-run graph copy: run_steps (node side) and run_edges (edge side).
-- Inserts and reads only — status transitions and dependency-counter
-- updates are the guarded CAS queries in transitions.sql.

-- updated_at is app-written from the injected clock, here and in every
-- transition (ADR-004 timestamp policy) — the reconciler's staleness scan
-- reads it, so tests must be able to control it.
-- retry_policy is the step's effective policy, materialized at
-- instantiation (ticket 5.2, ADR-006) — required on every row.
-- timeout is the step's per-attempt execution timeout, materialized the
-- same way (ticket 5.3); NULL means no timeout.
-- cache_policy is the step's authored response-cache policy, materialized
-- the same way (ticket 9.5, ADR-011); NULL means no `cache` block (engine
-- default policy decides).
-- budget_policy is the step's authored budget caps, materialized the same
-- way (ticket 10.3, ADR-012); NULL means no `budget` block (only the run
-- budget applies).
-- validation_policy is the step's authored output-validation chain,
-- materialized the same way (ticket 11.1, ADR-013); NULL means no
-- `validation` block (the output is accepted as produced).
-- blackboard_policy is the step's authored blackboard block (declarative
-- writes), materialized the same way (ticket 12.2, ADR-014); NULL means no
-- `blackboard` block (no declarative writes).
-- context_policy is the step's authored context-assembly spec, materialized
-- the same way (ticket 12.3, ADR-014); NULL means no `context` block (the
-- request is built from the config alone).
-- depth is the expansion nesting depth (ticket 13.2, ADR-015): 0 for a
-- definition-authored step, origin.depth+1 for an injected one. origin_step /
-- origin_kind name the expansion that introduced the row (NULL = authored).
-- name: CreateRunStep :one
INSERT INTO run_steps (run_id, step_id, step_type, config, retry_policy,
                       timeout, cache_policy, budget_policy, validation_policy, blackboard_policy, context_policy, status, remaining_deps, fired_deps,
                       graph_version, updated_at, depth, origin_step, origin_kind)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
RETURNING *;

-- CreateRunSteps is the batch (COPY) form for run instantiation (2.5) and
-- expansion (13.2), which write whole graphs at once.
-- name: CreateRunSteps :copyfrom
INSERT INTO run_steps (run_id, step_id, step_type, config, retry_policy,
                       timeout, cache_policy, budget_policy, validation_policy, blackboard_policy, context_policy, status, remaining_deps, fired_deps,
                       graph_version, updated_at, depth, origin_step, origin_kind)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19);

-- name: GetRunStep :one
SELECT * FROM run_steps WHERE run_id = $1 AND step_id = $2;

-- The template renderer's upstream-output read (ticket 8.2): the steps a
-- config's `${{ steps.<id>.output... }}` references name, fetched in one
-- round trip before execution. Referenced steps are terminal (their
-- outputs immutable) by the time the referencing step is dispatched, so
-- this read needs no lock.
-- name: ListRunStepsByIDs :many
SELECT * FROM run_steps
WHERE run_id = $1 AND step_id = ANY(@step_ids::text[])
ORDER BY step_id;

-- name: ListRunSteps :many
SELECT * FROM run_steps WHERE run_id = $1 ORDER BY step_id;

-- origin_step / origin_kind name the expansion that spliced the edge in
-- (ticket 13.2, ADR-015); NULL for a definition-authored edge.
-- name: CreateRunEdge :one
INSERT INTO run_edges (run_id, ordinal, from_step, to_step, edge_type,
                       when_expr, condition, max_iterations, graph_version, origin_step, origin_kind)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- CreateRunEdges is the batch (COPY) form for run instantiation (2.5) and
-- expansion (13.2).
-- name: CreateRunEdges :copyfrom
INSERT INTO run_edges (run_id, ordinal, from_step, to_step, edge_type,
                       when_expr, condition, max_iterations, graph_version, origin_step, origin_kind)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- ordinal order is semantic: the branch first-match rule evaluates
-- out-edges in declaration order (ADR-004).
-- name: ListRunEdges :many
SELECT * FROM run_edges WHERE run_id = $1 ORDER BY ordinal;

-- The completion transaction's out-edge read (M4.3): one step's outgoing
-- edges, in the ordinal order the branch first-match rule requires.
-- name: ListRunEdgesFromStep :many
SELECT * FROM run_edges WHERE run_id = $1 AND from_step = $2 ORDER BY ordinal;

-- Fan-in trace links (ticket 7.3, ADR-008): the attempt span of a join
-- step carries links from every firing parent, whose span contexts were
-- stamped onto their run_steps rows at claim time. Per-run edge sets are
-- small; no dedicated index needed.
-- name: ListFiringParentTraceSpans :many
SELECT e.from_step, s.trace_span
FROM run_edges e
JOIN run_steps s ON s.run_id = e.run_id AND s.step_id = e.from_step
WHERE e.run_id = $1 AND e.to_step = $2 AND e.resolution = 'fired'
ORDER BY e.ordinal;

-- MaxRunEdgeOrdinal returns the run's highest edge ordinal, or -1 when it has
-- no edges — so an expansion's first spliced edge continues from MAX+1
-- (ticket 13.2). Read under the run lock the expansion already holds.
-- name: MaxRunEdgeOrdinal :one
SELECT COALESCE(MAX(ordinal), -1)::int FROM run_edges WHERE run_id = $1;

-- AddRunStepRemainingDeps bumps a still-pending existing step's remaining_deps
-- by delta — the "before" splice, where an injected step becomes a new upstream
-- dependency of a pending anchor (ticket 13.2, ADR-015). Guarded on
-- status = 'pending': the anchor status was validated under this same run lock,
-- so zero rows is a graph-integrity error, never a race. updated_at is
-- app-written from the injected clock (ADR-004 timestamp policy).
-- name: AddRunStepRemainingDeps :one
UPDATE run_steps
SET remaining_deps = remaining_deps + @delta::int,
    updated_at     = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND status = 'pending'
RETURNING *;

-- ExpandRunGraph commits an expansion's run-row side atomically with the step
-- and edge inserts (ticket 13.2, ADR-015): increment graph_version and grow
-- steps_total by the injected count. The expected_version CAS is a
-- belt-and-braces guard under the run lock the expansion already holds — it can
-- only fail if a concurrent committed expansion moved the version, which the
-- lock serializes, so a zero-row result is an integrity error. graph_version's
-- new value is the run's expansion count + 1 and stamps every row this
-- expansion introduced.
-- name: ExpandRunGraph :one
UPDATE runs
SET graph_version = graph_version + 1,
    steps_total   = steps_total + @added::int
WHERE id = @run_id AND graph_version = @expected_version::int
RETURNING graph_version, steps_total;
