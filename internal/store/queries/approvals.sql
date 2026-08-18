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
-- A timeout `reject`/`approve` policy (ticket 15.4) reuses this CAS with
-- @status = 'expired' and @expired_at set, so the same pending guard arbitrates
-- human-vs-timeout; @expired_at is NULL on a human decision.
-- name: DecideApproval :one
UPDATE approvals
SET status          = @status,
    decision        = @decision,
    edited_payload  = @edited_payload,
    comment         = @comment,
    decided_by      = @decided_by,
    decided_at      = @now::timestamptz,
    decision_source = @decision_source,
    expired_at      = @expired_at,
    updated_at      = @now::timestamptz
WHERE id = @id AND status = 'pending'
RETURNING *;

-- MarkApprovalExpiredPending is the `on_timeout: park` marker CAS (ticket
-- 15.4): it stamps expired_at on a still-pending, not-yet-expired approval
-- WITHOUT moving it off pending — the approval stays decidable while the run
-- parks. The (status='pending' AND expired_at IS NULL) guard makes it
-- single-shot: a redelivered expiry (or the reconciler heal) that races it
-- matches nothing, so the run is parked at most once per timeout.
-- name: MarkApprovalExpiredPending :one
UPDATE approvals
SET expired_at = @now::timestamptz,
    updated_at = @now::timestamptz
WHERE id = @id AND status = 'pending' AND expired_at IS NULL
RETURNING *;

-- GetPendingApprovalByStep reads a step's open approval (ticket 15.4): the
-- timeout handler resolves the current pending approval for the (run, step)
-- named by the expiry envelope — the envelope carries no approval id, so a
-- requeue-minted fresh approval is found here, not a stale one. The unique
-- partial index (run_id, step_id) WHERE status = 'pending' makes this at most
-- one row.
-- name: GetPendingApprovalByStep :one
SELECT * FROM approvals
WHERE run_id = @run_id AND step_id = @step_id AND status = 'pending';

-- ListOverdueApprovals is the reconciler's delayed-queue safety net (ticket
-- 15.4): pending approvals whose timeout has passed but whose policy has not
-- been applied (expired_at IS NULL), scanned via approvals_overdue_idx oldest
-- first. The reconciler re-outboxes an approval_timeout envelope for each, so a
-- crash before the post-commit schedule (or a Redis data loss) still fires the
-- policy.
-- name: ListOverdueApprovals :many
SELECT * FROM approvals
WHERE status = 'pending' AND expired_at IS NULL
  AND timeout_at IS NOT NULL AND timeout_at <= @before::timestamptz
ORDER BY timeout_at, id
LIMIT @row_limit;

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
