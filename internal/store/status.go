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
