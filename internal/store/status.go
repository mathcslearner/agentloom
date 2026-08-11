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
)

// Attempt outcomes beyond the step statuses the completion transitions
// reuse (succeeded/failed). ADR-006 (M5) owns the full failure taxonomy.
const (
	// AttemptOutcomeLost: the holder went silent past the lease TTL and the
	// step was taken over (TakeoverStep) — administrative closure of the
	// dangling attempt, deliberately outside ADR-006's outcome taxonomy.
	AttemptOutcomeLost = "lost"
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
	EventRunSucceeded  = "run_succeeded"
	EventRunFailed     = "run_failed"
)
