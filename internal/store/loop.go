package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RecordLoopExhausted appends the loop_exhausted event inside the loop source's
// completion transaction (ticket 14.3). It is not a state transition — the
// exhausting completion still runs the ordinary success/failure path around it
// — so it only allocates the run's next monotonic seq and writes the event
// under the seq already advancing in that transaction. It must be called inside
// a WithTx callback, after the fenced SucceedStep, like ExpandRun.
func RecordLoopExhausted(ctx context.Context, q Querier, runID uuid.UUID, ev LoopExhaustedEvent, now time.Time) error {
	const op = "record loop exhausted"
	gq, err := transitionQueries(ctx, q, op, now)
	if err != nil {
		return err
	}
	return appendEvent(ctx, gq, op, runID, ev)
}

// RecordLoopNoProgress appends the loop_no_progress event inside the loop
// source's completion transaction (ticket 14.4), like RecordLoopExhausted.
func RecordLoopNoProgress(ctx context.Context, q Querier, runID uuid.UUID, ev LoopNoProgressEvent, now time.Time) error {
	const op = "record loop no progress"
	gq, err := transitionQueries(ctx, q, op, now)
	if err != nil {
		return err
	}
	return appendEvent(ctx, gq, op, runID, ev)
}

// RecordGuardTripped appends the guard_tripped event (ticket 14.4). It is not a
// state transition — the caller's surrounding transaction performs the halt
// (a permanent dead-letter, or a run cancel) — so it only appends the event
// under the seq already advancing in that transaction. It must be called inside
// a WithTx callback.
func RecordGuardTripped(ctx context.Context, q Querier, runID uuid.UUID, ev GuardTrippedEvent, now time.Time) error {
	const op = "record guard tripped"
	gq, err := transitionQueries(ctx, q, op, now)
	if err != nil {
		return err
	}
	return appendEvent(ctx, gq, op, runID, ev)
}
