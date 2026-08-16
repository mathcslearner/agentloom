-- Run-scoped blackboard (ticket 12.2, ADR-014). Append-only per key: a
-- write inserts the next version, reads select the head (latest version) or
-- the full history. The write path (store.PutBlackboardEntry) allocates the
-- version under the run lock every transition takes first, so two concurrent
-- writers of one key never collide on the primary key.

-- BlackboardHeadVersion returns the current head version of a key, or 0 when
-- the key has never been written. The write path reads it to allocate the
-- next version and to evaluate a compare-and-swap guard; run under the run
-- lock, it needs no FOR UPDATE.
-- name: BlackboardHeadVersion :one
SELECT COALESCE(MAX(version), 0)::int AS head
FROM blackboard_entries
WHERE run_id = $1 AND key = $2;

-- InsertBlackboardEntry appends one version. The caller has already resolved
-- the version (head + 1) and validated the CAS guard.
-- name: InsertBlackboardEntry :one
INSERT INTO blackboard_entries (run_id, key, version, value, token_count,
                                token_counter, tags, author_step_id,
                                author_attempt, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- GetBlackboardHead returns the latest version of one key (ErrNoRows when
-- the key does not exist).
-- name: GetBlackboardHead :one
SELECT * FROM blackboard_entries
WHERE run_id = $1 AND key = $2
ORDER BY version DESC
LIMIT 1;

-- ListBlackboardHistory returns every version of one key in ascending
-- version order.
-- name: ListBlackboardHistory :many
SELECT * FROM blackboard_entries
WHERE run_id = $1 AND key = $2
ORDER BY version;

-- ListBlackboardHeads returns the head of every key matching the optional
-- key and tag filters, ordered by key. The heads CTE picks the latest
-- version per key first, so the tag filter applies to the head — a tag on an
-- older, superseded version does not surface the key. An empty @keys / @tags
-- array means "no filter on this dimension".
-- name: ListBlackboardHeads :many
WITH heads AS (
    SELECT DISTINCT ON (key) *
    FROM blackboard_entries
    WHERE run_id = $1
    ORDER BY key, version DESC
)
SELECT * FROM heads
WHERE (cardinality(@keys::text[]) = 0 OR key = ANY(@keys::text[]))
  AND (cardinality(@tags::text[]) = 0 OR tags @> @tags::text[])
ORDER BY key;

-- ListBlackboardHeadsPage is the API's keyset page over heads: strictly
-- after @after_key, with the same optional key/tag filters, ascending by
-- key.
-- name: ListBlackboardHeadsPage :many
WITH heads AS (
    SELECT DISTINCT ON (key) *
    FROM blackboard_entries
    WHERE run_id = $1
    ORDER BY key, version DESC
)
SELECT * FROM heads
WHERE key > @after_key
  AND (cardinality(@keys::text[]) = 0 OR key = ANY(@keys::text[]))
  AND (cardinality(@tags::text[]) = 0 OR tags @> @tags::text[])
ORDER BY key
LIMIT @row_limit;

-- ListBlackboardVersionsPage is the API's keyset page over full history
-- (every version, not just heads): strictly after the (@after_key,
-- @after_version) cursor, ascending by (key, version), with the same
-- optional filters (a tag filter here matches each version by its own tags).
-- name: ListBlackboardVersionsPage :many
SELECT * FROM blackboard_entries
WHERE run_id = $1
  AND (cardinality(@keys::text[]) = 0 OR key = ANY(@keys::text[]))
  AND (cardinality(@tags::text[]) = 0 OR tags @> @tags::text[])
  AND (key, version) > (@after_key, @after_version::int)
ORDER BY key, version
LIMIT @row_limit;
