-- Per-run graph copy: run_steps (node side) and run_edges (edge side).
-- Inserts and reads only — status transitions and dependency-counter
-- updates are 2.6's guarded CAS; batch instantiation is 2.5's.

-- name: CreateRunStep :one
INSERT INTO run_steps (run_id, step_id, step_type, config, status,
                       remaining_deps, fired_deps, graph_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetRunStep :one
SELECT * FROM run_steps WHERE run_id = $1 AND step_id = $2;

-- name: ListRunSteps :many
SELECT * FROM run_steps WHERE run_id = $1 ORDER BY step_id;

-- name: CreateRunEdge :one
INSERT INTO run_edges (run_id, ordinal, from_step, to_step, edge_type,
                       when_expr, condition, max_iterations, graph_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- ordinal order is semantic: the branch first-match rule evaluates
-- out-edges in declaration order (ADR-004).
-- name: ListRunEdges :many
SELECT * FROM run_edges WHERE run_id = $1 ORDER BY ordinal;
