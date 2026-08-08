-- Stored-definition registry storage (ADR-004). Rows are immutable: no
-- UPDATE queries by design. Registry semantics (version assignment,
-- creation API) arrive in M6.

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
