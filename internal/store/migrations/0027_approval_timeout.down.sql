-- Revert ticket 15.4's approval-timeout marker and reconciler scan index.
DROP INDEX IF EXISTS approvals_overdue_idx;
ALTER TABLE approvals DROP COLUMN IF EXISTS expired_at;
