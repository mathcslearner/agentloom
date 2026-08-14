-- Reference retrieval corpus queries (ticket 8.8). The pg_fulltext
-- retriever (internal/retrieval/pgfts) is the only caller: Ingest upserts,
-- Query ranks with Postgres full-text search. There are no state
-- transitions here — retrieval_docs is a plain corpus table, not part of
-- the run/step CAS machinery.

-- UpsertRetrievalDoc adds or updates one document keyed by id, so
-- re-ingesting a corpus is idempotent (content/metadata replaced,
-- updated_at bumped). created_at is preserved on update.
-- name: UpsertRetrievalDoc :exec
INSERT INTO retrieval_docs (id, content, metadata)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE
SET content = EXCLUDED.content,
    metadata = EXCLUDED.metadata,
    updated_at = now();

-- QueryRetrievalDocs returns the documents matching the full-text query,
-- ranked by ts_rank descending (id ascending as a deterministic tiebreak
-- so equal-rank results order stably for tests), capped at row_limit. An
-- empty or no-match query matches nothing and returns no rows —
-- websearch_to_tsquery never errors on arbitrary input, so there is no
-- query-syntax failure path. ts_rank returns real (float4); the ::float8
-- cast keeps the sqlc-generated score a float64.
-- name: QueryRetrievalDocs :many
SELECT id, content, metadata,
       ts_rank(to_tsvector('english', content), websearch_to_tsquery('english', @query::text))::float8 AS score
FROM retrieval_docs
WHERE to_tsvector('english', content) @@ websearch_to_tsquery('english', @query::text)
ORDER BY score DESC, id ASC
LIMIT @row_limit::int;

-- CountRetrievalDocs reports the corpus size — a test/diagnostic aid, not
-- on any execution path.
-- name: CountRetrievalDocs :one
SELECT count(*) FROM retrieval_docs;
