-- Stored-definition registry storage (ADR-004). Rows are immutable: no
-- UPDATE queries by design. Registry semantics (version assignment,
-- creation API) are ticket 6.5's: Store.CreateDefinition /
-- Store.CreateDefinitionVersion in definitions.go.

-- name: CreateDefinition :one
INSERT INTO workflow_definitions (name, version, spec)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetDefinition :one
SELECT * FROM workflow_definitions WHERE id = $1;

-- name: GetDefinitionByNameVersion :one
SELECT * FROM workflow_definitions WHERE name = $1 AND version = $2;

-- name: ListDefinitionVersions :many
SELECT * FROM workflow_definitions WHERE name = $1 ORDER BY version;

-- name: ListDefinitions :many
SELECT * FROM workflow_definitions ORDER BY name, version;

-- name: DeleteDefinition :execrows
DELETE FROM workflow_definitions WHERE id = $1;

-- NextDefinitionVersion computes the version a new row of this name should
-- take: 1 for an unseen name. Allocation is serialized by the per-name
-- advisory lock below (Store.CreateDefinitionVersion), not by row locks —
-- MAX cannot lock rows that do not exist yet.
-- name: NextDefinitionVersion :one
SELECT (COALESCE(MAX(version), 0) + 1)::int AS next_version
FROM workflow_definitions WHERE name = $1;

-- AcquireDefinitionNameLock serializes version allocation per name
-- (ticket 6.5): a transaction-scoped advisory lock keyed on the name's
-- hash, released at commit/rollback. A cross-name hash collision merely
-- serializes two unrelated appends.
-- name: AcquireDefinitionNameLock :exec
SELECT pg_advisory_xact_lock(@lock_key::bigint);

-- ListDefinitionsLatest returns the newest version of each name, keyset-
-- paginated by name (the definitions list API, 6.5).
-- name: ListDefinitionsLatest :many
SELECT DISTINCT ON (name) * FROM workflow_definitions
WHERE (sqlc.narg('cursor_name')::text IS NULL OR name > sqlc.narg('cursor_name')::text)
ORDER BY name, version DESC
LIMIT @row_limit;
