package engine

// Run guards & termination policies (ticket 14.4, ADR-016). Run-level guards
// halt a run with a typed disposition and an explanatory event carrying "which
// limit, current value, configured cap". Three guard families already enforce
// themselves elsewhere and keep their richer events — the budget check
// (budget_exceeded, park|fail), the iteration cap (loop_exhausted, proceed|fail),
// and the no-progress detector (loop_no_progress, in loop.go). This file adds:
//
//   - the claim-time wall-clock guard: a run past its materialized deadline is
//     cancelled before the claimed step executes, so a runaway loop halts at the
//     next claim without waiting for the reconciler's periodic sweep (which stays
//     the safety net for crashed workers);
//   - guard_tripped events for the expansion caps (max_added_steps /
//     max_total_steps / max_expansions / max_depth), which previously carried
//     only a dead-letter reason string, rendered from the structured breaches the
//     store's rejection reports.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// guardDeadline enforces the run's wall-clock deadline at claim time (ticket
// 14.4). When the claimed run is at or past its materialized deadline_at, it
// cancels the run (reason deadline_exceeded — the same disposition the 5.6
// reconciler applies) with a guard_tripped event, then settles this claimed
// running step gracefully as cancelled. It returns handled=true when the guard
// tripped (the caller returns immediately, executing nothing), false to proceed.
//
// A run with no deadline, or one not yet expired, is a no-op — the common case,
// a single pointer/time compare on the fast path. The reconciler remains the
// safety net: it cancels a deadline-exceeded run whose workers are all idle or
// dead, emitting the same event.
func (e *Engine) guardDeadline(ctx context.Context, step gen.RunStep, origin store.ClaimOrigin) (bool, error) {
	if origin.DeadlineAt == nil {
		return false, nil
	}
	now := e.now()
	if now.Before(*origin.DeadlineAt) {
		return false, nil
	}
	logger := log.From(ctx)
	var startedAt time.Time
	if origin.StartedAt != nil {
		startedAt = *origin.StartedAt
	}
	ev := deadlineGuardEvent(startedAt, *origin.DeadlineAt, now)

	// Cancel the run + record the guard event in one transaction. A typed
	// conflict means the run already moved to cancelling/terminal (a concurrent
	// claim of a sibling step, or the reconciler, tripped it first) — that
	// cancel already recorded the guard event, so drop this one and fall through
	// to settle our step. A transport error redelivers; the reconciler heals.
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if _, cerr := cancelRunTx(ctx, q, step.RunID, store.RunCancelReasonDeadlineExceeded, now); cerr != nil {
			var te *store.TransitionError
			if errors.As(cerr, &te) {
				return nil
			}
			return cerr
		}
		return store.RecordGuardTripped(ctx, q, step.RunID, ev, now)
	})
	if txErr != nil {
		logger.ErrorContext(ctx, "wall-clock guard: cancel transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return true, txErr
	}
	logger.WarnContext(ctx, "run wall-clock deadline exceeded; cancelling run",
		slog.String("guard", ev.Guard), slog.Int64("elapsed_seconds", ev.Current), slog.Int64("cap_seconds", ev.Cap))
	// Settle this claimed running step: completeFailure sees the run cancelling
	// under the run lock and settles the step `cancelled` (never judged, ADR-006
	// row 8), and the rollup finalizes the now-cancelled run.
	return true, e.completeFailure(ctx, step, exec.Output{},
		fmt.Errorf("max_wall_clock: run wall-clock deadline exceeded"), dag.ClassPermanent, origin.RunTrace)
}

// deadlineGuardEvent builds the guard_tripped payload for a wall-clock halt
// (ticket 14.4): the elapsed seconds (current) versus the configured
// max_wall_clock (cap = deadline − start), both whole seconds. A run with no
// recorded start (should not happen for a deadline-exceeded run) reports zeros
// rather than a nonsensical huge cap.
func deadlineGuardEvent(startedAt, deadlineAt, now time.Time) store.GuardTrippedEvent {
	ev := store.GuardTrippedEvent{Guard: "max_wall_clock", Unit: "seconds", Action: "cancel"}
	if !startedAt.IsZero() {
		ev.Current = int64(math.Round(now.Sub(startedAt).Seconds()))
		ev.Cap = int64(math.Round(deadlineAt.Sub(startedAt).Seconds()))
	}
	return ev
}

// recordExpansionCapGuards appends one guard_tripped event per structured cap
// breach a rejected expansion reported (ticket 14.4), in its own short
// transaction. Called on the permanent cap-rejection route (an expansion whose
// origin's completion transaction already rolled back), before completeFailure
// dead-letters the origin — so the guard event, with the exact limit / current
// / cap, precedes the dead-letter. A record failure is logged, not fatal: the
// dead-letter (with its descriptive reason) is the load-bearing halt record.
func (e *Engine) recordExpansionCapGuards(ctx context.Context, runID uuid.UUID, stepID string, breaches []dag.CapBreach) {
	if len(breaches) == 0 {
		return
	}
	now := e.now()
	err := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		for _, b := range breaches {
			ev := store.GuardTrippedEvent{
				Guard:   b.Limit,
				StepID:  stepID,
				Current: int64(b.Current),
				Cap:     int64(b.Cap),
				Unit:    capBreachUnit(b.Limit),
				Action:  "fail",
			}
			if rerr := store.RecordGuardTripped(ctx, q, runID, ev, now); rerr != nil {
				return rerr
			}
		}
		return nil
	})
	if err != nil {
		log.From(ctx).WarnContext(ctx, "recording expansion-cap guard event(s) failed; dead-letter is the halt record",
			slog.Any("error", err))
	}
}

// capBreachUnit maps an expansion-cap limit name to its guard-event unit.
func capBreachUnit(limit string) string {
	switch limit {
	case "max_expansions":
		return "expansions"
	case "max_depth":
		return "depth"
	default:
		return "steps"
	}
}
