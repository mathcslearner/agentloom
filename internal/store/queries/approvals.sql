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

-- GetApproval reads one approval by id (15.3's decide path; the run view
-- reads via ListApprovalsByRun).
-- name: GetApproval :one
SELECT * FROM approvals WHERE id = @id;

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
