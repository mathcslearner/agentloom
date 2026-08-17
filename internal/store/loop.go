package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// LoopExhaustedEvent is the loop_exhausted event payload (ticket 14.3,
// ADR-016): a marked loop edge reached its max_iterations bound while its
// condition still signaled "iterate again". It records which loop, the
// iteration reached versus the configured cap, the condition, and the
// termination policy with the action taken — the "which limit, current value,
// configured cap" a guard event must explain (14.4).
type LoopExhaustedEvent struct {
	// LoopSourceStep is the loop edge's source (the completing step's authored
	// id — the `#k` instance suffix is dropped so all iterations share it).
	LoopSourceStep string `json:"loop_source_step"`
	// LoopSourceInstance is the specific completing instance (e.g. critique#2).
	LoopSourceInstance string `json:"loop_source_instance"`
	// BodyEntry is the loop edge's target (the body entry cloned per iteration).
	BodyEntry string `json:"body_entry"`
	// Iteration is the iteration the completing instance carried (its current
	// value); MaxIterations is the configured cap.
	Iteration     int `json:"iteration"`
	MaxIterations int `json:"max_iterations"`
	// Condition is the loop-continuation predicate (for the audit).
	Condition string `json:"condition"`
	// Policy is the configured on_exhausted policy ("proceed" | "fail").
	Policy string `json:"policy"`
	// Action is what the engine did: "proceed" (routed to the loop source's
	// normal outgoing edges) or "fail" (failed the run).
	Action string `json:"action"`
}

// RecordLoopExhausted appends the loop_exhausted event inside the loop source's
// completion transaction (ticket 14.3). It is not a state transition — the
// exhausting completion still runs the ordinary success/failure path around it
// — so it only allocates the run's next monotonic seq and writes the event
// under the seq already advancing in that transaction. It must be called inside
// a WithTx callback, after the fenced SucceedStep, like ExpandRun.
func RecordLoopExhausted(ctx context.Context, q Querier, runID uuid.UUID, event LoopExhaustedEvent, now time.Time) error {
	const op = "record loop exhausted"
	gq, err := transitionQueries(ctx, q, op, now)
	if err != nil {
		return err
	}
	return appendEvent(ctx, gq, op, runID, EventLoopExhausted, event)
}
