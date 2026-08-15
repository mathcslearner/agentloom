-- Revert ticket 11.3: drop the structured-output provenance column.
ALTER TABLE step_attempts DROP COLUMN repair;
