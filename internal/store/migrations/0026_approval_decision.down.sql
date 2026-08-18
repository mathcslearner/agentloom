-- Reverse ticket 15.3: drop the run_edges.decision marker column.
ALTER TABLE run_edges
    DROP CONSTRAINT run_edges_decision_check,
    DROP COLUMN decision;
