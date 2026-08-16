-- Dynamic graph expansion (ticket 13.2, ADR-015): the columns ExpandRun
-- writes when a planner (or, later, a map/loop) expansion splices new steps
-- and edges into the running graph. graph_version already exists on runs /
-- run_steps / run_edges (0002) and stamps the version that introduced a row;
-- these three additions carry the rest of the provenance and the run's caps.

-- run_steps.depth is the expansion nesting depth (ADR-015): a
-- definition-authored step is 0, the steps a depth-d origin injects are d+1.
-- The claim-time max_depth guard reads it off the row. origin_step /
-- origin_kind name the step whose completion introduced this row (NULL =
-- definition-authored) — the 13.6 introspection API and M18 UI read them so
-- provenance is a column read, not a join through the event log. The paired
-- CHECK keeps the two columns consistent (both set or both NULL).
ALTER TABLE run_steps
    ADD COLUMN depth       INT NOT NULL DEFAULT 0 CHECK (depth >= 0),
    ADD COLUMN origin_step TEXT,
    ADD COLUMN origin_kind TEXT,
    ADD CONSTRAINT run_steps_origin_kind_check
        CHECK (origin_kind IS NULL OR origin_kind IN ('planner', 'map', 'loop')),
    ADD CONSTRAINT run_steps_origin_pair_check
        CHECK ((origin_step IS NULL) = (origin_kind IS NULL));

-- run_edges carries the same origin provenance so a spliced edge's origin is
-- a column read too (13.6 reconstructs per-version deltas from these + the
-- graph_expanded events).
ALTER TABLE run_edges
    ADD COLUMN origin_step TEXT,
    ADD COLUMN origin_kind TEXT,
    ADD CONSTRAINT run_edges_origin_kind_check
        CHECK (origin_kind IS NULL OR origin_kind IN ('planner', 'map', 'loop')),
    ADD CONSTRAINT run_edges_origin_pair_check
        CHECK ((origin_step IS NULL) = (origin_kind IS NULL));

-- runs.expansion_caps is the run's *resolved* dag.ExpansionCaps, materialized
-- at instantiation like retry_policy/on_failure/deadline_at (ADR-006): the
-- claim/expansion-time guards read the caps off the row and never reparse the
-- definition snapshot, and a worker upgrade cannot change an in-flight run's
-- caps. NULL (a pre-0023 row) means the compiled defaults apply.
ALTER TABLE runs
    ADD COLUMN expansion_caps JSONB;
