-- API keys (ticket 6.1, ADR-007). Bearer credentials for the /v1 API:
-- the row stores the hex SHA-256 of the full plaintext plus a short
-- clear-text lookup prefix (`sk_` + 8 chars) — the plaintext itself is
-- returned once by the create endpoint and never persisted anywhere.
-- Verification is one indexed read by prefix, then a constant-time hash
-- compare, then the revocation/expiry predicates below.
--
-- Lifecycle is create / revoke / expire only: revoke is soft (revoked_at
-- stamped, row kept for audit), expiry is judged at verification time
-- against expires_at, and there is no un-revoke or rotation primitive.
-- Timestamps are written from the injected clock like every other table;
-- no DEFAULT now() so tests stay deterministic.
CREATE TABLE api_keys (
    id         UUID PRIMARY KEY,
    name       TEXT NOT NULL,
    prefix     TEXT NOT NULL UNIQUE,
    key_hash   TEXT NOT NULL UNIQUE,
    -- The scope set (ADR-007): admin implies all others; approve is
    -- reserved for M15 but assignable now.
    scopes     TEXT[] NOT NULL CHECK (
        scopes <@ ARRAY['submit', 'read', 'approve', 'admin']
        AND cardinality(scopes) >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);
