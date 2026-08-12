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
