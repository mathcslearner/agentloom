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

// OutboxReasonStepReady is the v1 outbox enqueue reason; ADR-005 (M3) owns
// the task envelope reasons are carried into.
const OutboxReasonStepReady = "step_ready"

// Event types written by run instantiation (2.5). Transition events join in
// 2.6; ADR-018 (M16) owns formalizing the envelope. `events.type` is
// free-form TEXT in schema v1, so these constants are the vocabulary.
const (
	EventRunCreated = "run_created"
	EventStepReady  = "step_ready"
)
