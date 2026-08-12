-- Run rows. Inserts and reads only: status/counter mutations are guarded
-- CAS transitions (transitions.sql) — no generic UPDATE here by design.

-- name: CreateRun :one
INSERT INTO runs (id, definition_id, definition, status, params,
                  idempotency_token, on_failure, steps_total, started_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1;

-- name: GetRunByIdempotencyToken :one
SELECT * FROM runs WHERE idempotency_token = @token::text;

-- name: ListRuns :many
SELECT * FROM runs ORDER BY created_at DESC, id LIMIT $1;

-- name: ListRunsByStatus :many
SELECT * FROM runs WHERE status = $1 ORDER BY created_at DESC, id LIMIT $2;

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
SELECT id, status FROM runs WHERE id = $1 FOR UPDATE;
