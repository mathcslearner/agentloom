ALTER TABLE runs DROP COLUMN expansion_caps;

ALTER TABLE run_edges
    DROP CONSTRAINT run_edges_origin_pair_check,
    DROP CONSTRAINT run_edges_origin_kind_check,
    DROP COLUMN origin_kind,
    DROP COLUMN origin_step;

ALTER TABLE run_steps
    DROP CONSTRAINT run_steps_origin_pair_check,
    DROP CONSTRAINT run_steps_origin_kind_check,
    DROP COLUMN origin_kind,
    DROP COLUMN origin_step,
    DROP COLUMN depth;
