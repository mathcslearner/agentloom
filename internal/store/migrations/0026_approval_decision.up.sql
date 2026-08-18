-- Human-in-the-loop decision API (ticket 15.3, ADR-017): the decision-vs-edge
-- routing needs the `decision` edge marker materialized on run_edges so the
-- runtime gate is a comparison on a column (ADR-017 §"The decision edge
-- marker") rather than a re-decode of the definition snapshot — which cannot
-- cleanly see engine-injected `gate#k` instances. NULL = an unmarked,
-- decision-agnostic edge (fires on approve / a non-approval source). The
-- values mirror dag.ApprovalDecision.
ALTER TABLE run_edges
    ADD COLUMN decision TEXT,
    ADD CONSTRAINT run_edges_decision_check
        CHECK (decision IS NULL OR decision IN ('approve', 'reject'));
