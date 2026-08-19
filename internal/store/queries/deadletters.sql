-- Dead-letter records (ticket 5.4, ADR-006 "Dead-letter model"). Inserts
-- happen only inside the dead-lettering transitions (transitions.go) —
-- the row is written in the same transaction as the step's terminal CAS,
-- so a dead_lettered step and its record cannot disagree. Reads serve
-- tests now and the M6.5 DLQ API later.

-- CreateDeadLetter allocates the per-step seq (the death count) from the
-- existing rows — safe without an explicit lock because every caller runs
-- inside a transition holding the run-row lock, which serializes writers
-- per run. The aggregate subquery yields one row even when no prior
-- deaths exist.
-- name: CreateDeadLetter :one
INSERT INTO dead_letters (run_id, step_id, seq, source, class, error, payload, attempts_at_death, created_at)
SELECT @run_id, @step_id, COALESCE(MAX(seq), 0) + 1, @source, @class, @error, @payload,
       @attempts_at_death::int, @created_at::timestamptz
FROM dead_letters
WHERE run_id = @run_id AND step_id = @step_id
RETURNING *;

-- name: ListDeadLettersByStep :many
SELECT * FROM dead_letters
WHERE run_id = $1 AND step_id = $2
ORDER BY seq;

-- name: ListDeadLettersByRun :many
SELECT * FROM dead_letters
WHERE run_id = $1
ORDER BY step_id, seq;

-- ListReadyStepsWithoutOutbox feeds the requeue op's re-dispatch (ticket
-- 5.4): after a requeue re-opens a failed run, every ready step with no
-- pending outbox row needs a fresh dispatch — the requeued step itself,
-- plus fail_fast siblings whose deliveries were ack-dropped while the run
-- was failed. Same anti-join shape as the reconciler's stale-ready scan.
-- name: ListReadyStepsWithoutOutbox :many
SELECT rs.step_id
FROM run_steps rs
WHERE rs.run_id = @run_id AND rs.status = 'ready'
  AND NOT EXISTS (
      SELECT 1 FROM task_outbox o
      WHERE o.run_id = rs.run_id AND o.step_id = rs.step_id)
ORDER BY rs.step_id;

-- ListDeadLettersPage is the cross-run DLQ list API's keyset page read (ticket
-- 18.6). It joins each death record to its step (current status + type) and run
-- (current status + definition id) so the operator triage view shows live
-- context. status = 'open' keeps only the death whose step is still
-- dead_lettered AND whose seq is the step's latest (a requeued-then-re-died step
-- has multiple rows; only the last is open); an all-mode filter keeps every row.
-- The optional run_id / source filters and the (created_at, run_id, step_id, seq)
-- keyset cursor mirror ListApprovals. Order is uniformly descending (newest
-- first), served by dead_letters_created_idx (0029).
-- name: ListDeadLettersPage :many
SELECT dl.*, rs.status AS step_status, rs.step_type,
       r.status AS run_status, r.definition_id
FROM dead_letters dl
JOIN run_steps rs ON rs.run_id = dl.run_id AND rs.step_id = dl.step_id
JOIN runs r ON r.id = dl.run_id
WHERE (sqlc.narg('run_id')::uuid IS NULL OR dl.run_id = sqlc.narg('run_id')::uuid)
  AND (sqlc.narg('source')::text IS NULL OR dl.source = sqlc.narg('source')::text)
  AND (NOT @open_only::boolean OR (rs.status = 'dead_lettered'
       AND dl.seq = (SELECT MAX(d2.seq) FROM dead_letters d2
                     WHERE d2.run_id = dl.run_id AND d2.step_id = dl.step_id)))
  AND (sqlc.narg('cursor_created_at')::timestamptz IS NULL
       OR (dl.created_at, dl.run_id, dl.step_id, dl.seq)
          < (sqlc.narg('cursor_created_at')::timestamptz, sqlc.narg('cursor_run_id')::uuid,
             sqlc.narg('cursor_step_id')::text, sqlc.narg('cursor_seq')::int))
ORDER BY dl.created_at DESC, dl.run_id DESC, dl.step_id DESC, dl.seq DESC
LIMIT @row_limit;

-- CountOpenDeadLetters counts the steps currently dead_lettered and awaiting a
-- requeue — the DLQ backlog behind /v1/system/stats (ticket 18.6). One dead
-- step counts once regardless of how many times it has died. Served by
-- run_steps_dead_lettered_idx (0029).
-- name: CountOpenDeadLetters :one
SELECT count(*) FROM run_steps WHERE status = 'dead_lettered';
