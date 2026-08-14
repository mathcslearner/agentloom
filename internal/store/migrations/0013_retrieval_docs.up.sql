-- Reference retrieval corpus (ticket 8.8, ADR-009): the documents the
-- pg_fulltext retriever ingests and ranks. Postgres full-text search is
-- the reference backend precisely because it needs zero new infrastructure
-- — no pgvector, no external vector store (both documented as follow-on
-- plugins in the backlog). One flat corpus in v1; a namespace/collection
-- column is a backlog item alongside the alternate backends.
--
-- Ranking uses a functional GIN index over to_tsvector('english', content)
-- rather than a materialized tsvector column, so the table's row type stays
-- ordinary scalar columns (no tsvector in the sqlc model) while the index
-- still serves the `@@` match in Query. The 'english' configuration is
-- fixed in v1; a per-corpus language is a backlog refinement.
CREATE TABLE retrieval_docs (
    id         TEXT PRIMARY KEY,
    content    TEXT NOT NULL,
    metadata   JSONB NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX retrieval_docs_fts_idx ON retrieval_docs
    USING GIN (to_tsvector('english', content));
