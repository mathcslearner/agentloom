-- Ticket 11.1 (ADR-013): output validation & the verdict.
--
-- Two nullable JSONB columns and two widened CHECK constraints — the
-- 0012 (usage) and 0015 (cache_policy) precedents. No table is added:
-- the verdict rides on the attempt like usage, and the chain config rides
-- on the run's materialized step row like cache_policy.

-- run_steps.validation_policy is the step's authored output-validation
-- chain, materialized at instantiation (ADR-013) exactly like cache_policy:
-- the engine's validate stage reads it off the claimed row and never
-- reparses the definition snapshot, so a worker upgrade cannot change an
-- in-flight run's validation. NULL means the step authored no `validation`
-- block — the output is accepted as produced.
ALTER TABLE run_steps ADD COLUMN validation_policy JSONB;

-- step_attempts.verdict is the chain verdict for an attempt whose step
-- carried a validation chain (ADR-013): {schema_version, status, score?,
-- issues[], results[]}. One JSON object, mirroring the usage/error columns
-- and extensible without another migration. NULL for every attempt of an
-- unvalidated step, and every pre-0018 row (no backfill).
ALTER TABLE step_attempts ADD COLUMN verdict JSONB;

-- validation_failed joins the attempt-outcome vocabulary. Unlike the
-- administrative outcomes lost (4.5) / throttled (9.2) / budget_exceeded
-- (10.3) — decided before the executor runs and outside ADR-006's
-- taxonomy — validation_failed is a *genuine* ADR-006 error class
-- (dag.ClassValidationFailed): the executor succeeded but the output was
-- rejected. It is still excluded from the transport retry budget
-- (CountCountedFailures counts transient/timeout only — no query change),
-- because a validation failure retries under its own semantic budget
-- (ADR-013), not the transport one. The value is new, so there are no
-- rows to backfill (constraint recipe: drop/re-add, never ALTER TYPE —
-- ADR-004). `run_events.type` is free-form TEXT in schema v1, so no event
-- DDL is needed.
ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_outcome_check;
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_outcome_check CHECK (
    outcome IN ('succeeded', 'transient', 'permanent', 'timeout', 'cancelled', 'lost', 'throttled', 'budget_exceeded', 'validation_failed'));

-- A dead-lettered validation failure records class validation_failed on its
-- dead_letters row, so the DLQ class CHECK widens to admit it (the row's
-- source is retries_exhausted — the semantic-retry budget, implicitly one
-- attempt in 11.1, is exhausted; ADR-013).
ALTER TABLE dead_letters DROP CONSTRAINT dead_letters_class_check;
ALTER TABLE dead_letters ADD CONSTRAINT dead_letters_class_check CHECK (
    class IN ('transient', 'permanent', 'timeout', 'cancelled', 'validation_failed'));
