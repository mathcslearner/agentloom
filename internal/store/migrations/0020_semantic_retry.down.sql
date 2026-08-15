-- Revert ticket 11.4: drop the semantic-retry feedback columns.
ALTER TABLE step_attempts DROP COLUMN feedback;
ALTER TABLE run_steps DROP COLUMN feedback;
