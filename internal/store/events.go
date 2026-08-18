package store

// Event vocabulary and payload types, normalized onto the internal/event leaf
// contract (ticket 16.1, ADR-018). The event package owns the type strings and
// payload structs; this file derives the store's Event* string constants from
// them (so ListByType and event-type comparisons keep working) and re-exports
// the payload structs the engine/api construct as type aliases (so those
// packages keep referencing store.XxxEvent). The store's single typed append
// helper (appendEvent in transitions.go, and the instantiation helper) takes an
// event.Payload and derives the type from it, so a writer can never emit a
// mismatched (type, shape) pair.

import (
	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Event-type string constants, derived from the event vocabulary. These are the
// stored `events.type` values (free-form TEXT in schema v1); callers compare
// and filter with them (Events().ListByType, event-feed reads).
const (
	EventRunCreated                 = string(event.TypeRunCreated)
	EventStepReady                  = string(event.TypeStepReady)
	EventStepClaimed                = string(event.TypeStepClaimed)
	EventStepSucceeded              = string(event.TypeStepSucceeded)
	EventStepFailed                 = string(event.TypeStepFailed)
	EventStepSkipped                = string(event.TypeStepSkipped)
	EventStepReclaimed              = string(event.TypeStepReclaimed)
	EventStepRetryScheduled         = string(event.TypeStepRetryScheduled)
	EventStepThrottled              = string(event.TypeStepThrottled)
	EventStepSemanticRetryScheduled = string(event.TypeStepSemanticRetry)
	EventStepDeadLettered           = string(event.TypeStepDeadLettered)
	EventStepCancelled              = string(event.TypeStepCancelled)
	EventStepCollected              = string(event.TypeStepCollected)
	EventStepRequeued               = string(event.TypeStepRequeued)
	EventStepRevived                = string(event.TypeStepRevived)
	EventRunSucceeded               = string(event.TypeRunSucceeded)
	EventRunFailed                  = string(event.TypeRunFailed)
	EventRunResumed                 = string(event.TypeRunResumed)
	EventRunParked                  = string(event.TypeRunParked)
	EventRunUnparked                = string(event.TypeRunUnparked)
	EventRunCancelling              = string(event.TypeRunCancelling)
	EventRunCancelled               = string(event.TypeRunCancelled)
	EventCostUpdated                = string(event.TypeCostUpdated)
	EventCostUnknownModel           = string(event.TypeCostUnknownModel)
	EventBudgetExceeded             = string(event.TypeBudgetExceeded)
	EventRunBudgetUpdated           = string(event.TypeRunBudgetUpdated)
	EventModelDowngraded            = string(event.TypeModelDowngraded)
	EventBlackboardUpdated          = string(event.TypeBlackboardUpdated)
	EventContextAssembled           = string(event.TypeContextAssembled)
	EventContextRevision            = string(event.TypeContextRevision)
	EventGraphExpanded              = string(event.TypeGraphExpanded)
	EventLoopExhausted              = string(event.TypeLoopExhausted)
	EventLoopNoProgress             = string(event.TypeLoopNoProgress)
	EventGuardTripped               = string(event.TypeGuardTripped)
	EventApprovalRequested          = string(event.TypeApprovalRequested)
	EventApprovalCancelled          = string(event.TypeApprovalCancelled)
	EventApprovalDecided            = string(event.TypeApprovalDecided)
	EventApprovalExpired            = string(event.TypeApprovalExpired)
	EventApprovalNotified           = string(event.TypeApprovalNotified)
	EventApprovalNotificationFailed = string(event.TypeApprovalNotificationFailed)
)

// Payload type aliases: the engine and API construct and read these by their
// store.XxxEvent names; they are the event package's payload structs.
type (
	CostUpdatedEvent             = event.CostUpdated
	GraphExpandedEvent           = event.GraphExpanded
	BudgetExceededEvent          = event.BudgetExceeded
	ModelDowngradedEvent         = event.ModelDowngraded
	LoopExhaustedEvent           = event.LoopExhausted
	LoopNoProgressEvent          = event.LoopNoProgress
	GuardTrippedEvent            = event.GuardTripped
	ContextAssembledEvent        = event.ContextAssembled
	ContextRevisionEvent         = event.ContextRevision
	ContextSourceRecord          = event.ContextSourceRecord
	ContextRevisionActionRecord  = event.ContextRevisionActionRecord
	ContextRevisionSummaryRecord = event.ContextRevisionSummaryRecord
	BlackboardUpdatedEvent       = event.BlackboardUpdated

	ApprovalRequestedEvent          = event.ApprovalRequested
	ApprovalCancelledEvent          = event.ApprovalCancelled
	ApprovalDecidedEvent            = event.ApprovalDecided
	ApprovalExpiredEvent            = event.ApprovalExpired
	ApprovalNotifiedEvent           = event.ApprovalNotified
	ApprovalNotificationFailedEvent = event.ApprovalNotificationFailed
)

// EventEnvelope projects a stored event row into a typed event.Envelope
// (ADR-018): it decodes the payload by type and lifts the step id. It is the
// read counterpart of the typed append — the M16.2+ WS backfill and firehose
// read stored rows through it. A row whose type is not in the catalog, or whose
// payload does not decode, returns an error.
func EventEnvelope(row gen.Event) (event.Envelope, error) {
	return event.DecodeEnvelope(row.RunID, row.Seq, row.CreatedAt, event.Type(row.Type), row.Payload)
}
