-- API key storage (ticket 6.1, ADR-007). Plain CRUD — keys are not run
-- state machines, so no CAS/event machinery. The plaintext never reaches
-- this layer: callers pass the precomputed hash and prefix.

-- name: CreateAPIKey :one
INSERT INTO api_keys (id, name, prefix, key_hash, scopes, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetAPIKey :one
SELECT * FROM api_keys WHERE id = $1;

-- name: GetAPIKeyByPrefix :one
SELECT * FROM api_keys WHERE prefix = $1;

-- name: ListAPIKeys :many
SELECT * FROM api_keys ORDER BY created_at DESC, id;

-- name: RevokeAPIKey :execrows
-- The revoked_at IS NULL guard makes revocation first-wins: a second
-- revoke reports zero rows and the original timestamp survives.
UPDATE api_keys SET revoked_at = $2
WHERE id = $1 AND revoked_at IS NULL;
