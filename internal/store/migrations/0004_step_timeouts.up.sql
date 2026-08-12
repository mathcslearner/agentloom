-- Step execution timeouts (ticket 5.3), conforming to ADR-006 (row 3 of
-- the taxonomy: the engine assigns the `timeout` class at the watchdog
-- deadline). The per-attempt timeout is materialized at instantiation
-- like config and retry_policy — the execution path never reparses the
-- definition snapshot, and a worker upgrade cannot change an in-flight
-- run's timeout. A Go duration string, matching the retry_policy JSON's
-- duration convention (readable in the row, guaranteed parseable by
-- submit-time validation). NULL means no timeout — the honest value for
-- every pre-5.3 row, so there is no backfill.
ALTER TABLE run_steps ADD COLUMN timeout TEXT;
