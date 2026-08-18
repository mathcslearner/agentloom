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
	// StepStatusCollected: a map instance that failed terminally under
	// on_item_failure=collect_errors (ticket 13.4b, ADR-015). Terminal, but —
	// unlike dead_lettered — tolerated: it carries an error-marker output the
	// gather collects, its out-edge to the gather is fired, and it bumps
	// steps_collected (not steps_failed), so the run may still succeed.
	StepStatusCollected = "collected"
	// StepStatusAwaitingHuman: a human_approval step parked the run's branch
	// (ticket 15.2, ADR-017) — it holds no claim (the executor cleared it) and
	// no lease or worker slot, but its attempt row stays open (the attempt
	// spans the human wait). Not terminal: a decision (15.3) resumes it —
	// running → succeeded/dead_lettered — a run cancel sweeps it to cancelled,
	// and the reconciler treats it as healthy-parked.
	StepStatusAwaitingHuman = "awaiting_human"
)

// Approval statuses (ticket 15.2, ADR-017): the lifecycle of one
// human_approval request. Mirrors the approvals_status_check constraint. In
// 15.2 only pending and cancelled are written; approved/rejected/expired
// arrive with the decision API (15.3) and timeout wiring (15.4).
const (
	ApprovalStatusPending   = "pending"
	ApprovalStatusApproved  = "approved"
	ApprovalStatusRejected  = "rejected"
	ApprovalStatusExpired   = "expired"
	ApprovalStatusCancelled = "cancelled"
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
	// OutboxReasonSemanticRetry: a validation-failure completion re-dispatched
	// a step it routed running → retrying for a feedback-augmented re-attempt
	// (ticket 11.4, ADR-013). Unlike a transport retry (delayed ZSET), a
	// semantic retry has no backoff and is enqueued through the outbox in the
	// completion transaction — the reconcile_retry scan still heals a crash
	// between commit and dispatch, since the row is `retrying` and due now.
	OutboxReasonSemanticRetry = "semantic_retry"
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
	// AttemptOutcomeThrottled: a fleet-wide rate-limit denial deferred the
	// attempt (ticket 9.2, ADR-010). Like `lost`, an administrative outcome
	// outside ADR-006's taxonomy — never counted against the retry budget
	// (the step was never executed), decided by the limiter before the
	// executor runs. Routed running → retrying, re-dispatched at
	// next_attempt_at.
	AttemptOutcomeThrottled = "throttled"
	// AttemptOutcomeBudgetExceeded: the claim's projected spend would exceed
	// the run budget under the park policy, so the step was released and the
	// run parked (ticket 10.3, ADR-012). Like `lost` and `throttled`, an
	// administrative outcome outside ADR-006's taxonomy — never counted
	// against the retry budget (the step was never executed), decided before
	// the executor runs. Routed running → ready; unpark re-dispatches it.
	AttemptOutcomeBudgetExceeded = "budget_exceeded"
	// AttemptOutcomeValidationFailed: the executor succeeded but the output
	// failed its validation chain (ticket 11.1, ADR-013). Unlike the
	// administrative outcomes above, this is a *genuine* ADR-006 error class
	// (dag.ClassValidationFailed) — the attempt ran and spent — but it is
	// still excluded from the transport retry budget (CountCountedFailures
	// counts transient/timeout only): a validation failure retries under its
	// own semantic budget (11.4), not the transport one. In 11.1 it routes
	// terminal (dead-letter, source retries_exhausted).
	AttemptOutcomeValidationFailed = "validation_failed"
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
	// EventStepThrottled: a fleet-wide rate-limit denial routed the step
	// running → retrying without executing it (ticket 9.2, ADR-010). The
	// payload carries the attempt, the resource, which bucket denied, the
	// limiter's retry_after, and when the re-dispatch is due.
	EventStepThrottled = "step_throttled"
	// EventStepSemanticRetryScheduled: an output-validation failure routed the
	// step running → retrying with a feedback-augmented re-attempt (ticket
	// 11.4, ADR-013). The payload carries the attempt, the semantic-attempt
	// depth and budget, the failing verdict's issue count, and when the
	// re-dispatch is due. Distinct from step_retry_scheduled (transport) so the
	// two budgets read apart in the event feed.
	EventStepSemanticRetryScheduled = "step_semantic_retry_scheduled"
	// EventStepDeadLettered: the step reached the terminal failure state
	// (ticket 5.4, ADR-006). The payload carries the source, the judged
	// class (empty for poison), the attempt count at death, and the
	// dead_letters seq.
	EventStepDeadLettered = "step_dead_lettered"
	// EventStepCancelled: the step was written off — it can never become
	// ready (ticket 5.4). The payload carries the reason
	// (upstream_dead_lettered; M5.6 adds run-cancel reasons).
	EventStepCancelled = "step_cancelled"
	// EventStepCollected: a map instance failed terminally under
	// on_item_failure=collect_errors and was tolerated (ticket 13.4b,
	// ADR-015). The payload carries the failure class and the attempt count —
	// the failure is recorded but did not fail the run.
	EventStepCollected = "step_collected"
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
	// EventCostUnknownModel: a cost-bearing attempt named a model with no
	// catalog entry and was priced at the fallback rate (ticket 10.2,
	// ADR-012). Appended by ApplyAttemptCost in the attempt-completion
	// transaction; the payload is cost.UnknownModelWarning. Mirrors
	// cost.EventTypeUnknownModel — the value the 10.1 contract fixed.
	EventCostUnknownModel = "cost_unknown_model"
	// EventCostUpdated: one cost-bearing attempt completed and its charge was
	// folded into the run aggregate (ticket 10.5, ADR-012). The payload
	// (CostUpdatedEvent) carries the attempt's charge and the run's running
	// spend/saved totals after the bump. Appended by ApplyAttemptCost in the
	// completion transaction, under the run lock and the same monotonic seq as
	// the aggregate bump — so the totals across a run's cost_updated events are
	// non-decreasing in seq order. Mirrors cost.EventTypeCostUpdated. Feeds the
	// M18 live meter via the M16 event feed.
	EventCostUpdated = "cost_updated"
	// EventBudgetExceeded: a claim's projected spend crossed a budget limit
	// (ticket 10.3, ADR-012). The payload (budgetExceededPayload) carries the
	// step, attempt, resource, the run spend and estimate that made the
	// projection, the limit crossed, and the action taken (park or fail).
	// Appended by BudgetParkStep and by the budget-fail completion.
	EventBudgetExceeded = "budget_exceeded"
	// EventRunBudgetUpdated: PATCH /v1/runs/{id}/budget raised the run's
	// spend budget (ticket 10.3). The payload carries the previous and new
	// budget in nano-USD.
	EventRunBudgetUpdated = "run_budget_updated"
	// EventModelDowngraded: the claim-time budget check routed an llm step to
	// a cheaper model in its model_fallbacks chain (ticket 10.4, ADR-012).
	// The payload (ModelDowngradedEvent) carries the from/to models and
	// resources, the trigger, and the spend/budget projection. Appended by
	// RecordModelDowngrade in its own fenced transaction before the
	// re-targeted attempt runs; the used model is durable on the attempt's
	// cost-ledger resource and its output, so no state transition is needed.
	EventModelDowngraded = "model_downgraded"

	// EventContextAssembled: the pre-execution context-assembly stage built an
	// llm step's request from its `context` spec (ticket 12.3, ADR-014). The
	// payload (ContextAssembledEvent) carries the per-source disposition
	// (included / skipped / truncated), the counter fingerprint, the assembled
	// context tokens, and the pre-flight request total. Appended by
	// RecordContextAssembled in its own fenced transaction before the assembled
	// request runs; it is not a state transition (the assembled content is
	// durable on the attempt via the request the executor sends).
	EventContextAssembled = "context_assembled"

	// EventGraphExpanded: a planner (or, later, a map/loop) expansion spliced
	// new steps and edges into the running graph (ticket 13.2, ADR-015). The
	// payload (GraphExpandedEvent) carries the origin, the graph_version
	// transition, the injected steps' depth, the verbatim PlanOutput delta, and
	// the readied/widened step ids. Appended by ExpandRun inside the origin's
	// completion transaction, under the run lock and the same monotonic seq as
	// the graph_version bump — so the run's graph_expanded events reconstruct
	// every version's delta in order (the 13.6 introspection API reads them).
	EventGraphExpanded = "graph_expanded"

	// EventContextRevision: one deterministic compaction strategy was applied
	// to shrink an over-budget assembled context (ticket 12.4, ADR-014). The
	// payload (ContextRevisionEvent) carries the strategy, its parameters, the
	// framed-request tokens before and after, and the per-entry drop/truncate
	// actions. Appended by RecordContextAssembled in the same fenced
	// transaction as the context_assembled event (one atomic audit under the
	// claim fence), one event per strategy that ran, before the assembled event.
	EventContextRevision = "context_revision"

	// EventLoopExhausted: a marked loop edge reached its max_iterations bound
	// while its condition still signaled "iterate again" (ticket 14.3, ADR-016).
	// The payload (LoopExhaustedEvent) carries the loop source, the body entry,
	// the iteration reached and the configured cap, the condition, and the
	// termination policy (proceed / fail) with the action taken. Appended in the
	// loop source's completion transaction — no expansion happens on the
	// exhausting completion; execution routes to the loop source's normal
	// (non-loop) outgoing edges (proceed) or the run fails (fail).
	EventLoopExhausted = "loop_exhausted"

	// EventGuardTripped: a run-level guard halted the run (ticket 14.4, ADR-016).
	// The payload (GuardTrippedEvent) names the guard (max_added_steps /
	// max_total_steps / max_expansions / max_depth / max_wall_clock), the current
	// value and configured cap, the unit, and the action taken (fail / cancel).
	// The iteration-cap and budget guards keep their own richer events
	// (loop_exhausted, budget_exceeded); this covers the expansion caps and the
	// wall-clock deadline, which otherwise carried only a reason string.
	EventGuardTripped = "guard_tripped"

	// EventLoopNoProgress: a loop's optional no-progress guard fired — two
	// consecutive iterations produced an identical output hash (ticket 14.4,
	// ADR-016). The payload (LoopNoProgressEvent) carries the loop source, the
	// compared step and pointer, the matching hash, and the policy (proceed /
	// fail) with the action taken, mirroring loop_exhausted's shape. Appended in
	// the loop source's completion transaction; proceed routes the loop source's
	// normal exit edges, fail dead-letters it.
	EventLoopNoProgress = "loop_no_progress"

	// EventApprovalRequested: a human_approval step parked without a lease and
	// wrote a pending approvals row (ticket 15.2, ADR-017). The payload
	// (ApprovalRequestedEvent) carries the approval id, the rendered title, the
	// allowed decisions, whether edits are permitted, and the timeout deadline
	// (nil = wait indefinitely). The decision events (approval_decided,
	// approval_expired) arrive with the decision API (15.3/15.4).
	EventApprovalRequested = "approval_requested"

	// EventApprovalCancelled: a pending approval was cancelled because the run
	// was cancelled while its step was parked (ticket 15.2, ADR-017). The
	// payload (ApprovalCancelledEvent) carries the approval id and the reason.
	EventApprovalCancelled = "approval_cancelled"
)
