ALTER TABLE run_steps DROP COLUMN trace_span;
ALTER TABLE task_outbox DROP COLUMN trace_state;
ALTER TABLE task_outbox DROP COLUMN trace_parent;
ALTER TABLE runs DROP COLUMN trace_state;
ALTER TABLE runs DROP COLUMN trace_parent;
