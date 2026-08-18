-- Human-approval timeouts (ticket 15.4, ADR-017): a parked approval whose
-- `timeout_at` passes applies its `on_timeout` policy through the same
-- single-arbiter CAS as a human decision. Two policies (`reject`/`approve`)
-- move the row off `pending` (status `expired`) and reuse the 0025 decision
-- columns; the `park` policy leaves the row `pending` (still decidable) but
-- parks the run — so status cannot be its "already applied" marker.

-- expired_at is the durable marker the `park` policy CASes on: NULL until the
-- timeout policy has been applied once. It lets `on_timeout: park` be
-- idempotent under at-least-once redelivery (a second expiry delivery, or the
-- reconciler heal, finds expired_at set and no-ops instead of re-parking a run
-- an operator may have already unparked). The `reject`/`approve` policies also
-- stamp it, alongside moving the row to `expired`.
ALTER TABLE approvals ADD COLUMN expired_at TIMESTAMPTZ;

-- The reconciler's overdue-approvals scan (the delayed-queue safety net,
-- ADR-005 P3 analogue): a pending approval whose timeout has passed but whose
-- policy has not been applied — because the delayed expiry was never
-- scheduled (crash before the post-commit ZADD) or was lost (Redis data loss).
-- The partial index keeps the scan cheap: only still-pending, not-yet-expired,
-- timeout-bearing rows are candidates.
CREATE INDEX approvals_overdue_idx
    ON approvals (timeout_at)
    WHERE status = 'pending' AND expired_at IS NULL AND timeout_at IS NOT NULL;
