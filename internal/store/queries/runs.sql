-- Run rows. Inserts and reads only: status/counter mutations are guarded
-- CAS transitions (transitions.sql) — no generic UPDATE here by design.

-- name: CreateRun :one
INSERT INTO runs (id, definition_id, definition, status, params,
                  idempotency_token, idempotency_fingerprint, on_failure,
                  steps_total, started_at, deadline_at,
                  trace_parent, trace_state,
                  budget_nano_usd, on_budget_exceeded)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1;

-- GetRunStatus is the lightweight unlocked status read behind the
-- engine's in-flight cancellation poller (ticket 5.6): one indexed point
-- read per poll, no row lock — the poller only wants a hint, the
-- completion transaction re-checks under the run lock.
-- name: GetRunStatus :one
SELECT status FROM runs WHERE id = $1;

-- name: GetRunByIdempotencyToken :one
SELECT * FROM runs WHERE idempotency_token = @token::text;

-- name: ListRuns :many
SELECT * FROM runs ORDER BY created_at DESC, id LIMIT $1;

-- name: ListRunsByStatus :many
SELECT * FROM runs WHERE status = $1 ORDER BY created_at DESC, id LIMIT $2;

-- ListRunsPage is the run-list API's keyset page read (ticket 6.5). Every
-- filter is optional (NULL disables it); the cursor is the last row of the
-- previous page and the row-value comparison is exactly "strictly after the
-- cursor in list order" because the order is uniformly descending. Served
-- by runs_created_at_id_idx / runs_definition_created_idx (0009).
-- name: ListRunsPage :many
SELECT * FROM runs
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('definition_id')::uuid IS NULL OR definition_id = sqlc.narg('definition_id')::uuid)
  AND (sqlc.narg('created_after')::timestamptz IS NULL OR created_at >= sqlc.narg('created_after')::timestamptz)
  AND (sqlc.narg('created_before')::timestamptz IS NULL OR created_at < sqlc.narg('created_before')::timestamptz)
  AND (sqlc.narg('cursor_created_at')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('cursor_created_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: DeleteRun :execrows
DELETE FROM runs WHERE id = $1;

-- AllocateEventSeq allocates the next per-run event sequence number
-- (ADR-004 event sequencing): the row lock serializes appends per run.
-- name: AllocateEventSeq :one
UPDATE runs SET next_seq = next_seq + 1 WHERE id = $1 RETURNING next_seq;

-- LockRun acquires the run-row lock without writing anything. It is every
-- transition's first statement (uniform run → step → edge lock ordering;
-- see the transitions.go package comment) and doubles as the existence
-- check — zero rows means the run is gone. It returns the run's status so
-- ClaimStep can enforce the run-status guard (ticket 5.2, ADR-006: the
-- claim path refuses steps of terminal runs; 5.6's park/cancel reuses it).
-- name: LockRun :one
-- trace_parent/trace_state ride along (ticket 7.3) so the claim path can
-- surface the run's root trace context without a second read; the budget
-- columns and spend ride along (ticket 10.3) so the claim path can project
-- spend for the budget check without a second read.
SELECT id, status, trace_parent, trace_state,
       budget_nano_usd, spent_nano_usd, on_budget_exceeded
FROM runs WHERE id = $1 FOR UPDATE;
