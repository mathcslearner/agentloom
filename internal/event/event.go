// Package event is the leaf contract for agentloom's per-run event feed
// (ticket 16.1, ADR-018). It defines the normalized event envelope, the closed
// event-type vocabulary, and one typed payload struct per type — the single
// source of truth the store's writers marshal, the WebSocket protocol frames
// (M16.2–16.4), and the TS client's types are generated from (M16.5).
//
// It imports only other leaf contracts (internal/dag for a planner's
// PlanOutput delta, internal/cost for the unknown-model warning shape) plus the
// standard library, so the durable-truth layer (internal/store, which depends
// on pgx/sqlc) can depend on this package without dragging persistence into the
// UI/transport contract. The append-only events table is the durable truth
// (ADR-004); this package is only the shape of what is appended and read back.
package event

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SchemaVersion is the envelope schema version (ADR-018). It is stamped at
// projection time from this constant — there is no schema_version column on the
// events table. Payload evolution within a version is additive-only (new
// optional fields); a breaking payload change bumps this version.
const SchemaVersion = 1

// Type is the closed vocabulary of event types on a run's feed. Every value is
// declared as a Type constant below and registered in Catalog; the two are
// cross-checked by TestCatalogComplete, so a new type cannot ship without its
// payload and catalog entry.
type Type string

// Payload is implemented by every event payload struct. EventType returns the
// type the payload belongs to, which is what makes a payload self-describing:
// the store's single append helper derives the event's type string from the
// payload it is handed, so a writer can never emit a mismatched (type, shape)
// pair. All payload structs are JSON-serializable value types.
type Payload interface {
	EventType() Type
}

// StepScoped is implemented by the payloads of step-scoped events — those about
// one step of the run. EventStepID returns that step's id (the concrete
// instance id for expanded steps, e.g. "critique#2"), which the envelope lifts
// into its StepID field so a consumer can filter a run's feed by step without
// decoding each payload. Run-scoped payloads (run_created, run_succeeded, …) do
// not implement it, and their envelope's StepID is empty.
type StepScoped interface {
	Payload
	EventStepID() string
}

// Envelope is the normalized wire/read shape of one event on a run's feed
// (ADR-018): the durable per-run monotonic seq, the type, the append timestamp,
// the step it concerns (empty for run-scoped events), and the typed payload.
// Ordering is always by Seq — Ts is the append wall-clock (events.created_at)
// for display, never for ordering. Delivery is at-least-once; a consumer
// dedupes and orders by (RunID, Seq).
type Envelope struct {
	// SchemaVersion is the envelope version (== SchemaVersion), stamped at
	// projection time.
	SchemaVersion int `json:"schema_version"`
	// RunID is the run the event belongs to.
	RunID uuid.UUID `json:"run_id"`
	// Seq is the per-run monotonic sequence number (ADR-004). Contiguous and
	// gap-free from 1; the WS protocol resumes from a client's last_seq.
	Seq int64 `json:"seq"`
	// Type is the event type.
	Type Type `json:"type"`
	// Ts is the append timestamp (events.created_at). Display only.
	Ts time.Time `json:"ts"`
	// StepID is the step the event concerns, lifted from a step-scoped payload;
	// empty for a run-scoped event.
	StepID string `json:"step_id,omitempty"`
	// Payload is the typed event body.
	Payload Payload `json:"payload"`
}

// NewEnvelope builds an envelope for a decoded payload at the given run, seq,
// and append time, lifting StepID from a step-scoped payload. It is the single
// place the envelope's SchemaVersion and StepID projection live, shared by the
// store read adapter and tests.
func NewEnvelope(runID uuid.UUID, seq int64, ts time.Time, p Payload) Envelope {
	env := Envelope{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		Seq:           seq,
		Type:          p.EventType(),
		Ts:            ts,
		Payload:       p,
	}
	if sc, ok := p.(StepScoped); ok {
		env.StepID = sc.EventStepID()
	}
	return env
}

// envelopeWire is the on-the-wire shape of an envelope with the payload left as
// raw JSON, so ParseEnvelope can decode the outer fields, dispatch on the type,
// and unmarshal the payload through the catalog. It mirrors Envelope's JSON tags.
type envelopeWire struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         uuid.UUID       `json:"run_id"`
	Seq           int64           `json:"seq"`
	Type          Type            `json:"type"`
	Ts            time.Time       `json:"ts"`
	Payload       json.RawMessage `json:"payload"`
}

// ParseEnvelope decodes a serialized envelope (the wire form produced by
// json.Marshal(Envelope)) back into a typed Envelope: it reads the outer fields,
// rejects an envelope schema version this build does not understand, and decodes
// the payload by type through the catalog (lifting StepID). It is the read path
// the pub/sub subscriber (16.2) and the WS server (16.3) turn a received message
// into a typed envelope with — the counterpart of marshaling an Envelope for
// publish. A malformed message, an unknown envelope version, or an unknown/
// undecodable payload is an error the caller drops (and heals via DB backfill).
func ParseEnvelope(raw []byte) (Envelope, error) {
	var w envelopeWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Envelope{}, fmt.Errorf("event: decoding envelope: %w", err)
	}
	if w.SchemaVersion != SchemaVersion {
		return Envelope{}, fmt.Errorf("event: unsupported envelope schema_version %d (want %d)", w.SchemaVersion, SchemaVersion)
	}
	env, err := DecodeEnvelope(w.RunID, w.Seq, w.Ts, w.Type, w.Payload)
	if err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// Event-type vocabulary (ADR-018). Grouped by producer; the doc comments name
// the ticket that introduced each and where its payload is written. These are
// the authoritative type strings — the store's Event* constants are derived
// from them.
const (
	// Run instantiation (ticket 2.5).
	TypeRunCreated Type = "run_created"

	// Step lifecycle (tickets 2.5/2.6, 4.5, 5.2/5.4, 9.2, 11.4, 13.4b).
	TypeStepReady          Type = "step_ready"
	TypeStepClaimed        Type = "step_claimed"
	TypeStepSucceeded      Type = "step_succeeded"
	TypeStepFailed         Type = "step_failed"
	TypeStepSkipped        Type = "step_skipped"
	TypeStepReclaimed      Type = "step_reclaimed"
	TypeStepRetryScheduled Type = "step_retry_scheduled"
	TypeStepThrottled      Type = "step_throttled"
	TypeStepSemanticRetry  Type = "step_semantic_retry_scheduled"
	TypeStepDeadLettered   Type = "step_dead_lettered"
	TypeStepCancelled      Type = "step_cancelled"
	TypeStepCollected      Type = "step_collected"
	TypeStepRequeued       Type = "step_requeued"
	TypeStepRevived        Type = "step_revived"

	// Run lifecycle (tickets 2.5/2.6, 5.4, 5.6).
	TypeRunSucceeded  Type = "run_succeeded"
	TypeRunFailed     Type = "run_failed"
	TypeRunResumed    Type = "run_resumed"
	TypeRunParked     Type = "run_parked"
	TypeRunUnparked   Type = "run_unparked"
	TypeRunCancelling Type = "run_cancelling"
	TypeRunCancelled  Type = "run_cancelled"

	// Cost & budget (tickets 10.2–10.5, ADR-012).
	TypeCostUpdated      Type = "cost_updated"
	TypeCostUnknownModel Type = "cost_unknown_model"
	TypeBudgetExceeded   Type = "budget_exceeded"
	TypeRunBudgetUpdated Type = "run_budget_updated"
	TypeModelDowngraded  Type = "model_downgraded"

	// Context & memory (tickets 12.2–12.4, ADR-014).
	TypeBlackboardUpdated Type = "blackboard_updated"
	TypeContextAssembled  Type = "context_assembled"
	TypeContextRevision   Type = "context_revision"

	// Dynamic graph, loops, guards (tickets 13.2, 14.3/14.4, ADR-015/016).
	TypeGraphExpanded  Type = "graph_expanded"
	TypeLoopExhausted  Type = "loop_exhausted"
	TypeLoopNoProgress Type = "loop_no_progress"
	TypeGuardTripped   Type = "guard_tripped"

	// Human-in-the-loop (tickets 15.2–15.5, ADR-017).
	TypeApprovalRequested          Type = "approval_requested"
	TypeApprovalCancelled          Type = "approval_cancelled"
	TypeApprovalDecided            Type = "approval_decided"
	TypeApprovalExpired            Type = "approval_expired"
	TypeApprovalNotified           Type = "approval_notified"
	TypeApprovalNotificationFailed Type = "approval_notification_failed"
)
