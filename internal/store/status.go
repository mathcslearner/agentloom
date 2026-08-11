package store

// Status vocabulary v1 (ADR-004). The named CHECK constraints in the schema
// are authoritative; these constants mirror them so callers never hand-write
// status strings. Later milestones extend both together (constraint recipe
// in ADR-004).

// Run statuses.
const (
	RunStatusRunning   = "running"
	RunStatusSucceeded = "succeeded"
	RunStatusFailed    = "failed"
)

// Step statuses.
const (
	StepStatusPending   = "pending"
	StepStatusReady     = "ready"
	StepStatusRunning   = "running"
	StepStatusSucceeded = "succeeded"
	StepStatusFailed    = "failed"
	StepStatusSkipped   = "skipped"
	// StepStatusRetrying: a failed attempt was recorded and another is due
	// at run_steps.next_attempt_at (ticket 5.2, ADR-006). Not terminal —
	// the claim CAS moves it back to running once the backoff elapses.
	StepStatusRetrying = "retrying"
)

// Edge types.
const (
	EdgeTypeNormal = "normal"
	EdgeTypeLoop   = "loop"
)

// Edge resolutions (ADR-004 dependency bookkeeping).
const (
	EdgeResolutionUnresolved = "unresolved"
	EdgeResolutionFired      = "fired"
	EdgeResolutionSkipped    = "skipped"
)

// Outbox enqueue reasons; ADR-005 owns the task envelope they are carried
// into. The vocabulary is small and closed (safe as a metric label).
const (
	// OutboxReasonStepReady: the step just became ready (instantiation 2.5,
	// completion fan-out 4.3).
	OutboxReasonStepReady = "step_ready"
	// OutboxReasonReconcileReady: the reconciler re-outboxed a step stuck
	// in ready — its original dispatch was lost (ticket 4.4, ADR-005
	// P2/R1(a)). Handled identically to step_ready downstream; the distinct
	// reason exists so healed dispatches are visible in stream entries and
	// logs.
	OutboxReasonReconcileReady = "reconcile_ready"
	// OutboxReasonReconcileRunning: the reconciler took over a stale
	// running step — its holder is presumed dead with no reclaimable lease
	// (ticket 4.5, ADR-005 R1(c)) — and re-outboxed it for a fresh claim.
	OutboxReasonReconcileRunning = "reconcile_running"
	// OutboxReasonReconcileRetry: the reconciler re-dispatched a retrying
	// step whose due time is long past with no delayed entry to show for it
	// — the failure-commit/delayed-schedule crash gap (ticket 5.2,
	// ADR-006). The step stays `retrying`; the claim CAS accepts it once
	// due.
	OutboxReasonReconcileRetry = "reconcile_retry"
)

// Attempt outcomes (ADR-006's error classes, plus `succeeded` and the
// administrative `lost`). The bare `failed` written by 2.6–4.x is retired:
// migration 0003 backfilled it to `permanent` and added the CHECK. The
// class constants mirror dag's ErrorClass vocabulary — string-typed here
// because outcomes are a storage vocabulary, not the contract type.
const (
	// AttemptOutcomeLost: the holder went silent past the lease TTL and the
	// step was taken over (TakeoverStep) — administrative closure of the
	// dangling attempt, deliberately outside ADR-006's outcome taxonomy
	// (lost attempts never consume the retry budget).
	AttemptOutcomeLost = "lost"
	// AttemptOutcomeTransient: a judged failure that can plausibly heal on
	// its own; counts against the retry budget.
	AttemptOutcomeTransient = "transient"
	// AttemptOutcomePermanent: a judged failure no re-execution can fix;
	// never retried.
	AttemptOutcomePermanent = "permanent"
	// AttemptOutcomeTimeout: the attempt was cancelled at its execution
	// deadline by a live worker (M5.3); counts against the retry budget.
	AttemptOutcomeTimeout = "timeout"
	// AttemptOutcomeCancelled: the attempt was cancelled by run-level
	// control flow (M5.6/5.7); never retried.
	AttemptOutcomeCancelled = "cancelled"
)

// Event types written by run instantiation (2.5) and the guarded
// transitions (2.6); ADR-018 (M16) owns formalizing the envelope.
// `events.type` is free-form TEXT in schema v1, so these constants are the
// vocabulary.
const (
	EventRunCreated    = "run_created"
	EventStepReady     = "step_ready"
	EventStepClaimed   = "step_claimed"
	EventStepSucceeded = "step_succeeded"
	EventStepFailed    = "step_failed"
	EventStepSkipped   = "step_skipped"
	// EventStepReclaimed: lease-expiry takeover (running → ready, claim
	// cleared — ticket 4.5, ADR-005). The payload carries the displaced
	// holder's claim_id and the attempt it strands.
	EventStepReclaimed = "step_reclaimed"
	// EventStepRetryScheduled: a classified-retryable attempt failure was
	// recorded and the step routed running → retrying (ticket 5.2,
	// ADR-006). The payload carries the attempt, its class, and when the
	// next attempt is due.
	EventStepRetryScheduled = "step_retry_scheduled"
	EventRunSucceeded       = "run_succeeded"
	EventRunFailed          = "run_failed"
)
