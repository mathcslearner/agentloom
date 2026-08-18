package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Budget limit kinds, recorded on the budget_exceeded event's `limit` field
// so a consumer knows which cap the projection crossed.
const (
	// BudgetLimitRun is the run-level budget_usd.
	BudgetLimitRun = "run"
	// BudgetLimitStepUSD is a step's max_usd cap.
	BudgetLimitStepUSD = "step_usd"
	// BudgetLimitStepTokens is a step's max_tokens cap.
	BudgetLimitStepTokens = "step_tokens"
)

// BudgetParkStepArgs are the inputs to BudgetParkStep.
type BudgetParkStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller.
	ClaimID uuid.UUID
	// Event is the projection detail recorded on the budget_exceeded event.
	Event BudgetExceededEvent
	// Now is the injected current time. Required.
	Now time.Time
}

// BudgetParkStep enforces the run-budget park action at claim time (ticket
// 10.3, ADR-012): the worker's claim projected spend over the run budget, so
// it releases its own step back to ready (running → ready, claim cleared —
// the BudgetParkRunStep CAS, fenced on the caller's claim so a zombie's
// release is rejected), closes the attempt with the administrative outcome
// `budget_exceeded` (never a counted class — the step never ran), appends the
// budget_exceeded event, and parks the run with reason budget_exceeded so
// unpark re-dispatches the released step. All in one transaction under the
// run lock.
//
// Concurrent fan-out is handled by parking only when the run is still running
// under the lock: a sibling step's worker that parked first leaves the run
// `parked`, so this call releases its own step but skips a redundant re-park
// (no duplicate run_parked event). A cancelling or terminal run is likewise
// left un-parked — the released step then settles through the cancel sweep or
// is inert on a terminal run.
func BudgetParkStep(ctx context.Context, q Querier, args BudgetParkStepArgs) (gen.RunStep, error) {
	const op = "budget park step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	run, err := lockRun(ctx, gq, op, args.RunID)
	if err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.BudgetParkRunStep(ctx, gen.BudgetParkRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusReady, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	args.Event.StepID = args.StepID
	args.Event.Attempt = step.AttemptCount
	args.Event.Action = "park"
	errPayload, _ := json.Marshal(args.Event) // small fixed struct; cannot fail
	if err := finishAttempt(ctx, gq, op, step, AttemptOutcomeBudgetExceeded, errPayload, nil, nil, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, args.Event); err != nil {
		return gen.RunStep{}, err
	}
	// Park the run only if it is still running under the lock. A sibling
	// budget park may have parked it first (status parked) — then the run is
	// already paused and re-parking would double-emit run_parked; a cancel
	// may have moved it to cancelling — then parking is wrong. The lock makes
	// this observation stable within the transaction.
	if run.Status == RunStatusRunning {
		reason := ParkReasonBudgetExceeded
		if _, perr := gq.ParkRun(ctx, gen.ParkRunParams{RunID: args.RunID, Reason: &reason}); perr != nil {
			return gen.RunStep{}, wrapErr(op+": park run", perr)
		}
		if err := appendEvent(ctx, gq, op, args.RunID, event.RunParked{Reason: reason}); err != nil {
			return gen.RunStep{}, err
		}
	}
	log.From(ctx).InfoContext(ctx, "step released and run parked for budget",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// SetRunBudgetArgs are the inputs to SetRunBudget.
type SetRunBudgetArgs struct {
	RunID uuid.UUID
	// BudgetNanoUSD is the new spend budget in integer nano-USD.
	BudgetNanoUSD int64
	// Now is the injected current time. Required.
	Now time.Time
}

// SetRunBudget raises (or sets) a run's spend budget (ticket 10.3: PATCH
// /v1/runs/{id}/budget). Guarded to non-terminal runs — a settled run's
// budget is immutable — with the run_budget_updated event carrying the
// previous and new budget. Raising the budget of a run parked for
// budget_exceeded and then unparking is the documented resume path.
func SetRunBudget(ctx context.Context, q Querier, args SetRunBudgetArgs) (gen.Run, error) {
	const op = "set run budget"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Run{}, err
	}
	locked, err := lockRun(ctx, gq, op, args.RunID)
	if err != nil {
		return gen.Run{}, err
	}
	var previous int64
	if locked.BudgetNanoUsd != nil {
		previous = *locked.BudgetNanoUsd
	}
	run, err := gq.SetRunBudget(ctx, gen.SetRunBudgetParams{
		RunID: args.RunID, BudgetNanoUsd: args.BudgetNanoUSD,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The guard excludes terminal runs (succeeded/failed/cancelled); the
		// run exists (lockRun found it), so a no-match means a terminal run.
		return gen.Run{}, fmt.Errorf("store: %s: %w", op, &TransitionError{
			Entity: "run", RunID: args.RunID, From: locked.Status, To: locked.Status,
			Reason: ConflictWrongStatus,
		})
	}
	if err != nil {
		return gen.Run{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, event.RunBudgetUpdated{
		PreviousNanoUSD: previous, BudgetNanoUSD: args.BudgetNanoUSD,
	}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).InfoContext(ctx, "run budget updated", log.RunID(args.RunID.String()))
	return run, nil
}

// Model-downgrade trigger kinds, recorded on the model_downgraded event's
// `trigger` field (ticket 10.4, ADR-012).
const (
	// DowngradeTriggerThreshold: the run's spend crossed the fallback's
	// at_budget_fraction soft threshold.
	DowngradeTriggerThreshold = "budget_threshold"
	// DowngradeTriggerProjection: the current model's projected spend
	// (spend + estimate) would exceed a budget, so the claim was routed to a
	// cheaper model that fits — the hard trigger.
	DowngradeTriggerProjection = "budget_projection"
)

// RecordModelDowngradeArgs are the inputs to RecordModelDowngrade.
type RecordModelDowngradeArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller; the
	// record is rejected if the step is no longer running under it (a zombie
	// must not record a misleading downgrade).
	ClaimID uuid.UUID
	// Event is the projection detail recorded on the model_downgraded event.
	Event ModelDowngradedEvent
	// Now is the injected current time. Required.
	Now time.Time
}

// RecordModelDowngrade appends the model_downgraded event for a claim the
// budget middleware routed to a cheaper model (ticket 10.4, ADR-012). It is
// not a state transition — no status changes and the used model becomes
// durable on the attempt's cost-ledger resource — so this only fences the
// record on the caller's live claim (the step must still be running under
// it) and appends the event under the run lock. A fenced caller gets a
// *TransitionError so the engine abandons like any other fenced write.
func RecordModelDowngrade(ctx context.Context, q Querier, args RecordModelDowngradeArgs) error {
	const op = "record model downgrade"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return err
	}
	step, err := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.StepID})
	if err != nil {
		return wrapErr(op, err)
	}
	if step.Status != StepStatusRunning || step.ClaimID == nil || *step.ClaimID != args.ClaimID {
		return stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusRunning, claim: &args.ClaimID,
		})
	}
	args.Event.StepID = args.StepID
	args.Event.Attempt = step.AttemptCount
	if err := appendEvent(ctx, gq, op, args.RunID, args.Event); err != nil {
		return err
	}
	log.From(ctx).InfoContext(ctx, "model downgraded for budget",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return nil
}
