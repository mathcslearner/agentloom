-- Load-environment Postgres init (ticket 19.1). Runs once, on first boot of
-- the resource-pinned load database (docker-compose.load.yml mounts this into
-- /docker-entrypoint-initdb.d/). pg_stat_statements is preloaded via the
-- server's shared_preload_libraries (set in the compose command:), and this
-- creates the extension so the load campaign (19.3) can read per-statement
-- totals — the evidence for the write-amplification hypotheses (H2).
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
