-- Side-effect journal rows (ticket 5.5, ADR-006). These are primitives:
-- the record-intent → execute → record-result protocol that composes them
-- lives in internal/exec/effects, which runs each phase in its own short
-- WithTx transaction — deliberately never spanning the external call.

-- GetSideEffectForUpdate locks the row for the intent phase's
-- read-then-branch (return done result / take over a dangling intent /
-- insert fresh). FOR UPDATE serializes concurrent intent recorders on the
-- same effect.
-- name: GetSideEffectForUpdate :one
SELECT * FROM side_effects
WHERE run_id = $1 AND step_id = $2 AND effect_id = $3
FOR UPDATE;

-- name: GetSideEffect :one
SELECT * FROM side_effects
WHERE run_id = $1 AND step_id = $2 AND effect_id = $3;

-- name: InsertSideEffectIntent :one
INSERT INTO side_effects (run_id, step_id, effect_id, status, attempt, claim_id, intent_at)
VALUES ($1, $2, $3, 'intent', $4, $5, $6)
RETURNING *;

-- TakeoverSideEffectIntent re-arms a dangling intent (a prior attempt died
-- between intent and result) for the current attempt. Guarded on
-- status='intent' so it can never regress a completed effect.
-- name: TakeoverSideEffectIntent :one
UPDATE side_effects
SET attempt = $4, claim_id = $5, intent_at = $6
WHERE run_id = $1 AND step_id = $2 AND effect_id = $3 AND status = 'intent'
RETURNING *;

-- CompleteSideEffect marks the effect done with its journaled result.
-- Guarded on status='intent': first writer wins, a racing completer's
-- update matches nothing and the caller reads back the stored result.
-- name: CompleteSideEffect :one
UPDATE side_effects
SET status = 'done', result = $4, result_at = $5
WHERE run_id = $1 AND step_id = $2 AND effect_id = $3 AND status = 'intent'
RETURNING *;

-- name: ListSideEffectsByStep :many
SELECT * FROM side_effects
WHERE run_id = $1 AND step_id = $2
ORDER BY effect_id;
