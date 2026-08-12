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
-- name: CreateRunStep :one
INSERT INTO run_steps (run_id, step_id, step_type, config, retry_policy,
                       timeout, status, remaining_deps, fired_deps,
                       graph_version, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- CreateRunSteps is the batch (COPY) form for run instantiation (2.5) and
-- expansion (M13), which write whole graphs at once.
-- name: CreateRunSteps :copyfrom
INSERT INTO run_steps (run_id, step_id, step_type, config, retry_policy,
                       timeout, status, remaining_deps, fired_deps,
                       graph_version, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: GetRunStep :one
SELECT * FROM run_steps WHERE run_id = $1 AND step_id = $2;

-- name: ListRunSteps :many
SELECT * FROM run_steps WHERE run_id = $1 ORDER BY step_id;

-- name: CreateRunEdge :one
INSERT INTO run_edges (run_id, ordinal, from_step, to_step, edge_type,
                       when_expr, condition, max_iterations, graph_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- CreateRunEdges is the batch (COPY) form for run instantiation (2.5) and
-- expansion (M13).
-- name: CreateRunEdges :copyfrom
INSERT INTO run_edges (run_id, ordinal, from_step, to_step, edge_type,
                       when_expr, condition, max_iterations, graph_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

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
