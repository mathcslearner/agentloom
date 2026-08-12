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
	// RunStatusParked: dispatch is paused (ticket 5.6) — the claim path
	// refuses the run's steps — while in-flight steps keep executing and
	// settle normally. Not terminal; unpark (or a rollup of already
	// in-flight work, or a cancel) is the way out.
	RunStatusParked = "parked"
	// RunStatusCancelling: a cancel was requested (ticket 5.6) — the
	// quiescing state. No new claims; claimless non-terminal steps were
	// cancelled by the request's sweep; in-flight steps settle to
	// cancelled as their workers notice. Not terminal.
	RunStatusCancelling = "cancelling"
	// RunStatusCancelled: the terminal cancel state (ticket 5.6) — every
	// step accounted for, none left in flight.
	RunStatusCancelled = "cancelled"
)

// Park reasons (ticket 5.6, ADR-004's typed-reason matrix row). Only
// manual is produced today; budget_exceeded (M10) and awaiting_human
// (M15) are reserved in the schema CHECK.
const (
	ParkReasonManual         = "manual"
	ParkReasonBudgetExceeded = "budget_exceeded"
	ParkReasonAwaitingHuman  = "awaiting_human"
)

// Run cancel reasons (ticket 5.6): why the run was cancelled — an
// operator (manual) or the reconciler's max_wall_clock deadline sweep.
const (
	RunCancelReasonManual           = "manual"
	RunCancelReasonDeadlineExceeded = "deadline_exceeded"
)

// Step statuses.
const (
	StepStatusPending   = "pending"
	StepStatusReady     = "ready"
	StepStatusRunning   = "running"
	StepStatusSucceeded = "succeeded"
	// StepStatusFailed: retired as a resting state by ticket 5.4 — ADR-006
	// made `failed` a routing state the completion transaction passes
	// through, and the terminal failure state is dead_lettered. The
	// constant (and the schema CHECK entry) remain for pre-5.4 rows;
	// nothing writes it anymore.
	StepStatusFailed  = "failed"
	StepStatusSkipped = "skipped"
	// StepStatusRetrying: a failed attempt was recorded and another is due
	// at run_steps.next_attempt_at (ticket 5.2, ADR-006). Not terminal —
	// the claim CAS moves it back to running once the backoff elapses.
	StepStatusRetrying = "retrying"
	// StepStatusDeadLettered: the terminal failure state (ticket 5.4,
	// ADR-006) — exhausted retries, a never-retryable class, or a poison
	// message. Every dead_lettered step has at least one dead_letters row;
	// the requeue op is the only way out (→ ready).
	StepStatusDeadLettered = "dead_lettered"
	// StepStatusCancelled: the step will never run — written off because a
	// dead-lettered upstream step made its readiness impossible (ticket
	// 5.4, under continue_independent_branches), or cancelled by run-level
	// control flow (M5.6). Terminal unless a requeue revives it (→
	// pending).
	StepStatusCancelled = "cancelled"
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
	// OutboxReasonDLQRequeue: the requeue op re-dispatched a dead-lettered
	// step it just reset to ready (ticket 5.4, ADR-006 "Requeue") — plus
	// any other ready step of the run whose dispatch was consumed while
	// the run was failed.
	OutboxReasonDLQRequeue = "dlq_requeue"
	// OutboxReasonUnpark: the unpark op re-dispatched a ready step whose
	// delivery was consumed by the run-status guard while the run was
	// parked (ticket 5.6).
	OutboxReasonUnpark = "unpark"
)

// Dead-letter sources (ticket 5.4, ADR-006 "Dead-letter model"): why a
// step landed in the DLQ. Mirrored by the dead_letters_source_check
// constraint.
const (
	// DeadLetterSourceRetriesExhausted: a retryable class ran out of
	// budget — the final counted failure was the step's last attempt.
	DeadLetterSourceRetriesExhausted = "retries_exhausted"
	// DeadLetterSourcePermanent: the failure was never retryable — a
	// permanent class, a class outside the step's retry_on, or a corrupt
	// materialized policy that made the retry decision impossible.
	DeadLetterSourcePermanent = "permanent"
	// DeadLetterSourcePoison: the queue's delivery-count threshold tripped
	// (ADR-005) — the entry's handlers kept dying without ever recording a
	// judgment, so class is NULL and the raw envelope is preserved in
	// payload.
	DeadLetterSourcePoison = "poison"
)

// Step-cancel reasons — the step_cancelled event payload.
const (
	// CancelReasonUpstreamDeadLettered: the 5.4 write-off — a dead-lettered
	// upstream step made this step's readiness impossible.
	CancelReasonUpstreamDeadLettered = "upstream_dead_lettered"
	// CancelReasonRunCancelled: run-level cancellation (ticket 5.6) — the
	// request's sweep (pending/ready/retrying steps), an in-flight worker
	// settling its step after noticing the cancel (running steps), or the
	// reconciler's cancelling-run heal after that worker crashed.
	CancelReasonRunCancelled = "run_cancelled"
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
	// EventStepDeadLettered: the step reached the terminal failure state
	// (ticket 5.4, ADR-006). The payload carries the source, the judged
	// class (empty for poison), the attempt count at death, and the
	// dead_letters seq.
	EventStepDeadLettered = "step_dead_lettered"
	// EventStepCancelled: the step was written off — it can never become
	// ready (ticket 5.4). The payload carries the reason
	// (upstream_dead_lettered; M5.6 adds run-cancel reasons).
	EventStepCancelled = "step_cancelled"
	// EventStepRequeued: the requeue op reset a dead-lettered step to
	// ready, re-arming its full retry policy (ticket 5.4).
	EventStepRequeued = "step_requeued"
	// EventStepRevived: a requeue made a written-off (cancelled) step's
	// readiness possible again — back to pending (ticket 5.4).
	EventStepRevived  = "step_revived"
	EventRunSucceeded = "run_succeeded"
	EventRunFailed    = "run_failed"
	// EventRunResumed: a requeue re-opened a failed run (ticket 5.4) — the
	// claim path admits its steps again.
	EventRunResumed = "run_resumed"
	// EventRunParked: dispatch paused with a typed reason (ticket 5.6).
	EventRunParked = "run_parked"
	// EventRunUnparked: dispatch resumed; the unpark op re-outboxes the
	// run's ready steps (ticket 5.6).
	EventRunUnparked = "run_unparked"
	// EventRunCancelling: a cancel was requested with a typed reason
	// (ticket 5.6) — the run entered the quiescing state.
	EventRunCancelling = "run_cancelling"
	// EventRunCancelled: the cancel finalized — every step terminal
	// (ticket 5.6).
	EventRunCancelled = "run_cancelled"
)
