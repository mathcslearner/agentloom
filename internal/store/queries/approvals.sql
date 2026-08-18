-- Approvals (ticket 15.2, ADR-017): one human-approval request per parked
-- step. Writes go through transition-style helpers (approval.go) so the
-- approvals row, the step CAS, and the events are one atomic unit; these
-- read queries serve the run-status view and the pending-approvals gauge.

-- InsertApproval writes the pending approval row inside the park
-- transaction. The unique partial index (run_id, step_id) WHERE
-- status = 'pending' rejects a second pending row for the same step.
-- name: InsertApproval :one
INSERT INTO approvals (
    id, run_id, step_id, attempt, status,
    title, description, payload, allowed_decisions, allow_edit, edit_schema,
    timeout_at, created_at, updated_at)
VALUES (
    @id, @run_id, @step_id, @attempt, 'pending',
    @title, @description, @payload, @allowed_decisions, @allow_edit, @edit_schema,
    @timeout_at, @now::timestamptz, @now::timestamptz)
RETURNING *;

-- CancelPendingApprovalByStep marks a step's pending approval cancelled (the
-- run-cancel sweep, ticket 15.2) in one CAS keyed by the unique pending
-- index. The pending guard makes it a single-arbiter transition: a stale
-- expiry or a concurrent decide (15.3/15.4) that already moved the row off
-- pending matches nothing.
-- name: CancelPendingApprovalByStep :one
UPDATE approvals
SET status     = 'cancelled',
    updated_at = @now::timestamptz
WHERE run_id = @run_id AND step_id = @step_id AND status = 'pending'
RETURNING *;

-- DecideApproval is the single-arbiter CAS of the decision API (ticket 15.3,
-- ADR-017): pending → approved|rejected, keyed by id with a pending guard, so
-- two concurrent decisions (or a decision racing 15.4's timeout expiry) have
-- exactly one winner — the loser matches nothing and gets a 409. It records
-- the immutable decision fields (decision, edited payload, comment, actor,
-- timestamp, source) that the run view and the approval_decided event expose.
-- name: DecideApproval :one
UPDATE approvals
SET status          = @status,
    decision        = @decision,
    edited_payload  = @edited_payload,
    comment         = @comment,
    decided_by      = @decided_by,
    decided_at      = @now::timestamptz,
    decision_source = @decision_source,
    updated_at      = @now::timestamptz
WHERE id = @id AND status = 'pending'
RETURNING *;

-- GetApproval reads one approval by id (15.3's decide path; the run view
-- reads via ListApprovalsByRun).
-- name: GetApproval :one
SELECT * FROM approvals WHERE id = @id;

-- ListApprovals is the GET /v1/approvals list (ticket 15.3): optional status
-- and run filters, keyset paginated oldest-first (created_at, id) — the
-- inbox order the approvals_pending_idx serves. A NULL @status / @run_id
-- disables that filter; the cursor is the last row's (created_at, id).
-- name: ListApprovals :many
SELECT * FROM approvals
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('run_id')::uuid IS NULL OR run_id = sqlc.narg('run_id')::uuid)
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (created_at, id) > (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY created_at, id
LIMIT @row_limit;

-- ListApprovalsByRun returns a run's approvals, newest first — the
-- run-status view's `approvals` array (ticket 15.2 criterion c).
-- name: ListApprovalsByRun :many
SELECT * FROM approvals
WHERE run_id = @run_id
ORDER BY created_at DESC, id DESC;

-- CountPendingApprovals is the fleet-wide engine_approval_pending gauge
-- source, sampled by the worker's metrics loop (the outbox-depth precedent).
-- name: CountPendingApprovals :one
SELECT count(*) FROM approvals WHERE status = 'pending';
