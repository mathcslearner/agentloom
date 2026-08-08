-- Reverse of core schema v1: drop children before parents (task_outbox
-- and run_edges have no FKs but belong to the same layer).
DROP TABLE task_outbox;
DROP TABLE events;
DROP TABLE step_attempts;
DROP TABLE run_edges;
DROP TABLE run_steps;
DROP TABLE runs;
DROP TABLE workflow_definitions;
