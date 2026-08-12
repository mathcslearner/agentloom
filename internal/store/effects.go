package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Side-effect journal statuses (ticket 5.5). The named CHECK constraint in
// migration 0006 is authoritative; these constants mirror it.
const (
	// SideEffectStatusIntent: the effect was about to fire and may have —
	// the recording attempt has not journaled an outcome. A later attempt
	// finding this re-executes (the documented residual at-least-once
	// window; the per-(run, step) idempotency key is what dedupes at the
	// external service).
	SideEffectStatusIntent = "intent"
	// SideEffectStatusDone: the journaled result is the truth; every later
	// attempt short-circuits to it and the effect never fires again.
	SideEffectStatusDone = "done"
)

// SideEffectRepo reads and writes side_effects rows (ticket 5.5). These
// are primitives: the record-intent → execute → record-result protocol
// composing them — including the takeover and first-wins-result rules —
// lives in internal/exec/effects, which runs each phase in its own short
// WithTx transaction, deliberately never holding one across the external
// call. Rows are never deleted while the run exists (ON DELETE CASCADE
// follows the run); a journaled result is immutable by the status guard
// on CompleteSideEffect.
type SideEffectRepo interface {
	// Get returns the effect row, or ErrNotFound.
	Get(ctx context.Context, runID uuid.UUID, stepID, effectID string) (gen.SideEffect, error)
	// GetForUpdate returns the effect row locked FOR UPDATE (intent-phase
	// serialization), or ErrNotFound. Only meaningful inside WithTx.
	GetForUpdate(ctx context.Context, runID uuid.UUID, stepID, effectID string) (gen.SideEffect, error)
	// InsertIntent records a fresh intent. A concurrent insert of the same
	// effect surfaces as a *ConflictError on the primary key.
	InsertIntent(ctx context.Context, arg gen.InsertSideEffectIntentParams) (gen.SideEffect, error)
	// TakeoverIntent re-arms a dangling intent for the current attempt;
	// ErrNotFound when the row is missing or already done.
	TakeoverIntent(ctx context.Context, arg gen.TakeoverSideEffectIntentParams) (gen.SideEffect, error)
	// Complete marks the effect done with its journaled result;
	// ErrNotFound when the row is missing or already done (first writer
	// wins — the caller reads back the stored result).
	Complete(ctx context.Context, arg gen.CompleteSideEffectParams) (gen.SideEffect, error)
	// ListByStep returns the step's journal rows in effect_id order.
	ListByStep(ctx context.Context, runID uuid.UUID, stepID string) ([]gen.SideEffect, error)
}

type sideEffectRepo struct{ q *gen.Queries }

func (r sideEffectRepo) Get(ctx context.Context, runID uuid.UUID, stepID, effectID string) (gen.SideEffect, error) {
	row, err := r.q.GetSideEffect(ctx, gen.GetSideEffectParams{RunID: runID, StepID: stepID, EffectID: effectID})
	return row, wrapErr("get side effect", err)
}

func (r sideEffectRepo) GetForUpdate(ctx context.Context, runID uuid.UUID, stepID, effectID string) (gen.SideEffect, error) {
	row, err := r.q.GetSideEffectForUpdate(ctx, gen.GetSideEffectForUpdateParams{RunID: runID, StepID: stepID, EffectID: effectID})
	return row, wrapErr("get side effect for update", err)
}

func (r sideEffectRepo) InsertIntent(ctx context.Context, arg gen.InsertSideEffectIntentParams) (gen.SideEffect, error) {
	row, err := r.q.InsertSideEffectIntent(ctx, arg)
	return row, wrapErr("insert side-effect intent", err)
}

func (r sideEffectRepo) TakeoverIntent(ctx context.Context, arg gen.TakeoverSideEffectIntentParams) (gen.SideEffect, error) {
	row, err := r.q.TakeoverSideEffectIntent(ctx, arg)
	return row, wrapErr("takeover side-effect intent", err)
}

func (r sideEffectRepo) Complete(ctx context.Context, arg gen.CompleteSideEffectParams) (gen.SideEffect, error) {
	row, err := r.q.CompleteSideEffect(ctx, arg)
	return row, wrapErr("complete side effect", err)
}

func (r sideEffectRepo) ListByStep(ctx context.Context, runID uuid.UUID, stepID string) ([]gen.SideEffect, error) {
	rows, err := r.q.ListSideEffectsByStep(ctx, gen.ListSideEffectsByStepParams{RunID: runID, StepID: stepID})
	return rows, wrapErr("list side effects by step", err)
}
