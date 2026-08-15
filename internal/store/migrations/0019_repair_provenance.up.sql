-- Ticket 11.3 (ADR-013): JSON repair & structured-output provenance.
--
-- One nullable JSONB column — the 0012 (usage) / 0018 (verdict) precedent.
-- No table, no CHECK change: repair provenance rides on the attempt like
-- usage and verdict, and introduces no new outcome or class.

-- step_attempts.repair is the structured-output provenance for an attempt of
-- an llm step that declared an output_format (ADR-013): {schema_version,
-- status, steps?, raw_text?}. Status is one of native | raw | repaired |
-- unrepairable — how the completion was shaped into structured JSON before
-- the validate stage. NULL for every attempt of a step with no output_format,
-- and every pre-0019 row (no backfill).
ALTER TABLE step_attempts ADD COLUMN repair JSONB;
