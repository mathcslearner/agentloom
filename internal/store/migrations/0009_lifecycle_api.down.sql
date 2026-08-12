DROP INDEX runs_definition_created_idx;
DROP INDEX runs_created_at_id_idx;
ALTER TABLE runs DROP COLUMN idempotency_fingerprint;
