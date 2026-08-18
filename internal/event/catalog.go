package event

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Entry is one row of the event catalog (ADR-018): a type, whether it is
// step-scoped, the ticket that introduced it, a one-line doc, and a factory that
// mints a fresh pointer payload of the type (for typed decode and schema
// reflection). The catalog is the single registry the schema generator, the
// decode path, and the completeness test all read.
type Entry struct {
	Type       Type
	StepScoped bool
	Since      string
	Doc        string
	factory    func() Payload
}

// New returns a fresh pointer payload of the entry's type, ready to unmarshal
// JSON into.
func (e Entry) New() Payload { return e.factory() }

// entry builds a catalog Entry, deriving both the Type and StepScoped from the
// sample payload the factory mints — so neither the type nor the scope flag can
// drift from the struct. The factory returns a fresh pointer payload (methods
// are on value receivers, so a pointer satisfies Payload / StepScoped).
func entry(factory func() Payload, since, doc string) Entry {
	sample := factory()
	_, scoped := sample.(StepScoped)
	return Entry{
		Type:       sample.EventType(),
		StepScoped: scoped,
		Since:      since,
		Doc:        doc,
		factory:    factory,
	}
}

// Catalog is the ordered event catalog (ADR-018), documentation order matching
// the type vocabulary in event.go. Every Type constant appears exactly once and
// every entry's payload implements Payload; TestCatalogComplete enforces both.
var Catalog = []Entry{
	entry(func() Payload { return &RunCreated{} }, "2.5", "Run instantiated: name and total step count."),

	entry(func() Payload { return &StepReady{} }, "2.5", "Step became dispatchable."),
	entry(func() Payload { return &StepClaimed{} }, "2.6", "Worker claimed a step (fresh fence)."),
	entry(func() Payload { return &StepSucceeded{} }, "2.6", "Step attempt completed successfully."),
	entry(func() Payload { return &StepFailed{} }, "2.6", "Retired resting-failure event (pre-5.4 rows only)."),
	entry(func() Payload { return &StepSkipped{} }, "2.6", "Step skipped: incoming edge did not fire."),
	entry(func() Payload { return &StepReclaimed{} }, "4.5", "Lease-expiry takeover strands the displaced holder's attempt."),
	entry(func() Payload { return &StepRetryScheduled{} }, "5.2", "Classified-retryable failure routed to retrying with backoff."),
	entry(func() Payload { return &StepThrottled{} }, "9.2", "Rate-limit denial deferred the step without executing it."),
	entry(func() Payload { return &StepSemanticRetry{} }, "11.4", "Output-validation failure routed to a feedback-augmented re-attempt."),
	entry(func() Payload { return &StepDeadLettered{} }, "5.4", "Step reached terminal failure (DLQ)."),
	entry(func() Payload { return &StepCancelled{} }, "5.4", "Step written off — can never become ready."),
	entry(func() Payload { return &StepCollected{} }, "13.4b", "Map instance failed under collect_errors and was tolerated."),
	entry(func() Payload { return &StepRequeued{} }, "5.4", "Requeue reset a dead-lettered step to ready."),
	entry(func() Payload { return &StepRevived{} }, "5.4", "Requeue made a written-off step's readiness possible again."),

	entry(func() Payload { return &RunSucceeded{} }, "2.6", "Run reached terminal success."),
	entry(func() Payload { return &RunFailed{} }, "2.6", "Run reached terminal failure."),
	entry(func() Payload { return &RunResumed{} }, "5.4", "Requeue re-opened a failed run."),
	entry(func() Payload { return &RunParked{} }, "5.6", "Dispatch paused with a typed reason."),
	entry(func() Payload { return &RunUnparked{} }, "5.6", "Dispatch resumed by the unpark op."),
	entry(func() Payload { return &RunCancelling{} }, "5.6", "Cancel requested — the quiescing state."),
	entry(func() Payload { return &RunCancelled{} }, "5.6", "Cancel finalized — every step terminal."),

	entry(func() Payload { return &CostUpdated{} }, "10.5", "One cost-bearing attempt's charge plus the run's running totals."),
	entry(func() Payload { return &CostUnknownModel{} }, "10.2", "A cost-bearing attempt's model had no catalog entry (fallback-priced)."),
	entry(func() Payload { return &BudgetExceeded{} }, "10.3", "A claim's projected spend crossed a budget limit (park / fail)."),
	entry(func() Payload { return &RunBudgetUpdated{} }, "10.3", "A PATCH …/budget raised the run's spend budget."),
	entry(func() Payload { return &ModelDowngraded{} }, "10.4", "The budget check routed an llm step to a cheaper fallback model."),

	entry(func() Payload { return &BlackboardUpdated{} }, "12.2", "A run-scoped blackboard key gained a new version."),
	entry(func() Payload { return &ContextAssembled{} }, "12.3", "An llm step's pre-execution context-assembly manifest."),
	entry(func() Payload { return &ContextRevision{} }, "12.4", "One deterministic compaction strategy shrank an over-budget assembly."),

	entry(func() Payload { return &GraphExpanded{} }, "13.2", "A planner/map/loop expansion spliced steps and edges into the graph."),
	entry(func() Payload { return &LoopExhausted{} }, "14.3", "A marked loop edge reached its max_iterations bound."),
	entry(func() Payload { return &LoopNoProgress{} }, "14.4", "A loop's no-progress guard fired on identical consecutive output."),
	entry(func() Payload { return &GuardTripped{} }, "14.4", "A run-level guard (expansion cap / wall-clock) halted the run."),

	entry(func() Payload { return &ApprovalRequested{} }, "15.2", "A human_approval step parked and wrote a pending approval."),
	entry(func() Payload { return &ApprovalCancelled{} }, "15.2", "A pending approval was cancelled with its run."),
	entry(func() Payload { return &ApprovalDecided{} }, "15.3", "A pending approval was decided through the arbiter CAS."),
	entry(func() Payload { return &ApprovalExpired{} }, "15.4", "A pending approval's timeout fired and its policy was applied."),
	entry(func() Payload { return &ApprovalNotified{} }, "15.5", "An approval notification was delivered to the webhook."),
	entry(func() Payload { return &ApprovalNotificationFailed{} }, "15.5", "An approval notification could not be delivered (warning)."),
}

// catalogByType indexes Catalog for O(1) lookup; built once at init.
var catalogByType = func() map[Type]Entry {
	m := make(map[Type]Entry, len(Catalog))
	for _, e := range Catalog {
		m[e.Type] = e
	}
	return m
}()

// Lookup returns the catalog entry for a type.
func Lookup(t Type) (Entry, bool) {
	e, ok := catalogByType[t]
	return e, ok
}

// Decode unmarshals a raw event payload into its typed value using the catalog.
// It returns the concrete payload (a pointer) or an error for an unknown type or
// malformed JSON. The store read adapter and the WS/TS-facing tooling use it to
// turn a stored (type, payload) row into a typed envelope.
func Decode(t Type, raw json.RawMessage) (Payload, error) {
	e, ok := Lookup(t)
	if !ok {
		return nil, fmt.Errorf("event: unknown type %q", t)
	}
	p := e.New()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, p); err != nil {
			return nil, fmt.Errorf("event: decoding %q payload: %w", t, err)
		}
	}
	return p, nil
}

// DecodeEnvelope turns a stored event row into a typed envelope: it decodes the
// payload by type and lifts the step id. It is the read counterpart of the
// store's typed append — the WS backfill and firehose (M16.2–16.4) project
// stored rows through it.
func DecodeEnvelope(runID uuid.UUID, seq int64, ts time.Time, t Type, raw json.RawMessage) (Envelope, error) {
	p, err := Decode(t, raw)
	if err != nil {
		return Envelope{}, err
	}
	return NewEnvelope(runID, seq, ts, p), nil
}
