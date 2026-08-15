-- Ticket 11.4 (ADR-013): the semantic-retry engine.
--
-- Two nullable JSONB columns — the 0012 (usage) / 0018 (verdict) / 0019
-- (repair) precedent. No new table, no new outcome or class: a semantic
-- retry reuses the existing validation_failed attempt outcome (0018) and the
-- running → retrying CAS (5.2), and the critique it carries rides on the
-- step and attempt rows like usage.

-- run_steps.feedback is the critique the step's NEXT attempt must carry
-- (ADR-013, ticket 11.4): the rendered feedback text plus its semantic-attempt
-- bookkeeping ({schema_version, semantic_attempt, max_attempts, prior_attempt,
-- text}). The semantic-retry completion writes it; the claim CAS copies it
-- onto the new attempt row and clears it there; the succeed / dead-letter
-- transitions clear it (the pending critique is consumed or abandoned).
-- Storing it on the step (not only the attempt) is what makes it survive a
-- crash/takeover and an interleaved transport retry between semantic attempts.
-- NULL means no pending critique — a first attempt, or a step with no
-- semantic policy.
ALTER TABLE run_steps ADD COLUMN feedback JSONB;

-- step_attempts.feedback is the critique THIS attempt was given (ADR-013):
-- the same record, copied off the step at claim so the attempt history is a
-- durable, diffable record of what each semantic re-attempt was told. NULL on
-- every first attempt and every attempt of a step with no semantic policy,
-- and every pre-0020 row (no backfill).
ALTER TABLE step_attempts ADD COLUMN feedback JSONB;
