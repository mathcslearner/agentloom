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

// LoopNoProgressEvent is the loop_no_progress event payload (ticket 14.4,
// ADR-016): a loop's opt-in no-progress guard detected two consecutive
// iterations producing an identical output hash, so it forced the loop to
// terminate. It mirrors LoopExhaustedEvent's "which limit, current, cap" shape
// with the compared-output detail in place of the iteration cap.
type LoopNoProgressEvent struct {
	// LoopSourceStep is the loop edge's source (the completing step's authored
	// id — the `#k` instance suffix dropped so all iterations share it).
	LoopSourceStep string `json:"loop_source_step"`
	// LoopSourceInstance is the specific completing instance (e.g. critique#2).
	LoopSourceInstance string `json:"loop_source_instance"`
	// ComparedStep is the authored id of the step whose output was hashed
	// (empty guard step resolves to the loop source).
	ComparedStep string `json:"compared_step"`
	// Path is the RFC 6901 pointer into that step's output that was hashed
	// (empty means the whole output).
	Path string `json:"path,omitempty"`
	// Iteration is the completing instance's iteration; PrevInstance and
	// CurInstance are the two compared instances (iteration k-1 and k).
	Iteration    int    `json:"iteration"`
	PrevInstance string `json:"prev_instance"`
	CurInstance  string `json:"cur_instance"`
	// Hash is the shared output hash (hex) that matched.
	Hash string `json:"hash"`
	// Policy is the configured no_progress policy ("proceed" | "fail"); Action
	// is what the engine did (the same vocabulary).
	Policy string `json:"policy"`
	Action string `json:"action"`
}

// RecordLoopNoProgress appends the loop_no_progress event inside the loop
// source's completion transaction (ticket 14.4), like RecordLoopExhausted.
func RecordLoopNoProgress(ctx context.Context, q Querier, runID uuid.UUID, event LoopNoProgressEvent, now time.Time) error {
	const op = "record loop no progress"
	gq, err := transitionQueries(ctx, q, op, now)
	if err != nil {
		return err
	}
	return appendEvent(ctx, gq, op, runID, EventLoopNoProgress, event)
}

// GuardTrippedEvent is the guard_tripped event payload (ticket 14.4, ADR-016):
// a run-level guard (an expansion cap, or the wall-clock deadline) halted the
// run. It carries the "which limit, current value, configured cap" contract
// plus the action taken, so the operator sees why a run stopped without
// parsing a free-text reason.
type GuardTrippedEvent struct {
	// Guard names the limit: max_added_steps | max_total_steps |
	// max_expansions | max_depth | max_wall_clock.
	Guard string `json:"guard"`
	// StepID is the step whose completion tripped an expansion cap; empty for a
	// run-wide guard (the wall-clock deadline).
	StepID string `json:"step_id,omitempty"`
	// Current is the value the run reached; Cap is the configured ceiling.
	Current int64 `json:"current"`
	Cap     int64 `json:"cap"`
	// Unit describes Current/Cap: steps | expansions | depth | seconds.
	Unit string `json:"unit"`
	// Action is the halt disposition: fail (dead-letter, run fails) or cancel
	// (the wall-clock deadline settles the run cancelled).
	Action string `json:"action"`
}

// RecordGuardTripped appends the guard_tripped event (ticket 14.4). It is not a
// state transition — the caller's surrounding transaction performs the halt
// (a permanent dead-letter, or a run cancel) — so it only appends the event
// under the seq already advancing in that transaction. It must be called inside
// a WithTx callback.
func RecordGuardTripped(ctx context.Context, q Querier, runID uuid.UUID, event GuardTrippedEvent, now time.Time) error {
	const op = "record guard tripped"
	gq, err := transitionQueries(ctx, q, op, now)
	if err != nil {
		return err
	}
	return appendEvent(ctx, gq, op, runID, EventGuardTripped, event)
}
