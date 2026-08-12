-- Distributed tracing across the queue (ticket 7.3, ADR-008).
--
-- runs.trace_parent/trace_state: the run's root trace context (W3C
-- traceparent/tracestate), captured from the POST /v1/runs server span at
-- instantiation. This is the durable copy ADR-008 requires: re-enqueues
-- that descend from no live span — the reconciler's reconcile_* heals,
-- dlq_requeue, unpark — restore linkage from here instead of starting an
-- orphan trace. Nullable: no context (tracing off, pre-7.3 rows) means
-- exactly what an absent envelope field means.
ALTER TABLE runs ADD COLUMN trace_parent TEXT;
ALTER TABLE runs ADD COLUMN trace_state TEXT;

-- task_outbox.trace_parent/trace_state: the enqueuing span's context,
-- stamped by writers that run inside a live span (the completion
-- transaction's fan-out, instantiation's entry steps). The dispatcher
-- copies it into the envelope; rows written with no live span (reconciler,
-- unpark, dlq_requeue) leave it NULL and the drain falls back to the run
-- row's durable context.
ALTER TABLE task_outbox ADD COLUMN trace_parent TEXT;
ALTER TABLE task_outbox ADD COLUMN trace_state TEXT;

-- run_steps.trace_span: the current attempt's span context (traceparent
-- format), stamped by the claim CAS. Its previous value — observed by the
-- pre-claim read — is how the next attempt links back to the failed or
-- lost attempt it re-executes (ADR-008: retries and reclaim/takeover
-- re-executions are span links, never parent-child), surviving crashes and
-- healed re-dispatches because it rides the step row, not the envelope.
ALTER TABLE run_steps ADD COLUMN trace_span TEXT;
