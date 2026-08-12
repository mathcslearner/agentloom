-- Side-effect journal (ticket 5.5, ADR-006 "Idempotency keys & side-effect
-- journal"). One row per (run, step, effect): executors record intent,
-- perform the external call, then record the result — a journaled result
-- short-circuits re-execution on any later attempt of the same step, which
-- is what makes external side effects effectively-once across retries,
-- reclaims, and zombie takeovers. Postgres holds the journal for the same
-- reason it holds the DLQ: the record must survive anything Redis can lose
-- and join against attempt history.

-- effect_id is executor-chosen and scopes multiple effects within one step
-- (the per-(run, step) idempotency key plus effect_id is the full external
-- identity). status is a two-phase marker: 'intent' means the effect may
-- have fired (the attempt died before recording the outcome); 'done' means
-- the journaled result is the truth and execution never happens again.
-- attempt/claim_id record who last held the intent — diagnostics, not
-- fencing: the result write is first-wins (a displaced zombie's late
-- result loses to the row already marked done, keeping the journal
-- single-valued). Timestamps are written from the injected clock like
-- every transition; no DEFAULT now() so tests stay deterministic.
CREATE TABLE side_effects (
    run_id     UUID NOT NULL,
    step_id    TEXT NOT NULL,
    effect_id  TEXT NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('intent', 'done')),
    attempt    INT  NOT NULL CHECK (attempt >= 1),
    claim_id   UUID NOT NULL,
    result     JSONB,
    intent_at  TIMESTAMPTZ NOT NULL,
    result_at  TIMESTAMPTZ,
    PRIMARY KEY (run_id, step_id, effect_id),
    FOREIGN KEY (run_id, step_id) REFERENCES run_steps (run_id, step_id) ON DELETE CASCADE,
    CONSTRAINT side_effects_done_has_result_at CHECK (
        status <> 'done' OR result_at IS NOT NULL)
);
