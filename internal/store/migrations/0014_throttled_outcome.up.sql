-- Throttled backpressure outcome (ticket 9.2), conforming to ADR-010
-- (fleet-wide rate limiting & backpressure). A rate-limit denial records
-- the administrative attempt outcome `throttled` — the second such outcome
-- after `lost` (ticket 4.5), deliberately outside ADR-006's error-class
-- taxonomy: it is never counted against the retry budget and never judged
-- by classifyFailure. The limiter decides it structurally, before the
-- executor runs, and reuses 5.2's `running → retrying` CAS wholesale.
--
-- Only the attempt-outcome CHECK changes: `run_events.type` is free-form
-- TEXT in schema v1 (status.go is its vocabulary), so `step_throttled`
-- needs no DDL. The value is new, so there are no rows to backfill (the
-- constraint recipe: drop/re-add, never ALTER TYPE — ADR-004).
ALTER TABLE step_attempts DROP CONSTRAINT step_attempts_outcome_check;
ALTER TABLE step_attempts ADD CONSTRAINT step_attempts_outcome_check CHECK (
    outcome IN ('succeeded', 'transient', 'permanent', 'timeout', 'cancelled', 'lost', 'throttled'));
