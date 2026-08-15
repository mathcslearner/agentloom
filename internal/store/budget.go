package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// BudgetExceededEvent is the budget_exceeded event payload (ticket 10.3,
// ADR-012): which limit a claim's projected spend crossed, the numbers that
// made the projection, and the action taken. Money fields are integer
// nano-USD; the token fields carry the projection for a step_tokens limit.
type BudgetExceededEvent struct {
	StepID    string `json:"step_id"`
	AttemptNo int32  `json:"attempt"`
	// Resource is the ADR-010/ADR-012 resource the claim would bill to
	// ("mock:sim-1", "tool:paid_search"); empty when unknown pre-flight.
	Resource string `json:"resource,omitempty"`
	// Limit is which cap was crossed: run | step_usd | step_tokens.
	Limit string `json:"limit"`
	// Action is what the engine did: park | fail.
	Action string `json:"action"`
	// SpentNanoUSD / EstimateNanoUSD / ProjectedNanoUSD / BudgetNanoUSD
	// describe a USD projection (run or step_usd limits).
	SpentNanoUSD     int64 `json:"spent_nano_usd,omitempty"`
	EstimateNanoUSD  int64 `json:"estimate_nano_usd,omitempty"`
	ProjectedNanoUSD int64 `json:"projected_nano_usd,omitempty"`
	BudgetNanoUSD    int64 `json:"budget_nano_usd,omitempty"`
	// ProjectedTokens / MaxTokens describe a step_tokens projection.
	ProjectedTokens int64 `json:"projected_tokens,omitempty"`
	MaxTokens       int64 `json:"max_tokens,omitempty"`
}

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
	args.Event.AttemptNo = step.AttemptCount
	args.Event.Action = "park"
	errPayload, _ := json.Marshal(args.Event) // small fixed struct; cannot fail
	if err := finishAttempt(ctx, gq, op, step, AttemptOutcomeBudgetExceeded, errPayload, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventBudgetExceeded, args.Event); err != nil {
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
		if err := appendEvent(ctx, gq, op, args.RunID, EventRunParked, runReasonPayload{Reason: reason}); err != nil {
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
	if err := appendEvent(ctx, gq, op, args.RunID, EventRunBudgetUpdated, runBudgetPayload{
		PreviousNanoUSD: previous, BudgetNanoUSD: args.BudgetNanoUSD,
	}); err != nil {
		return gen.Run{}, err
	}
	log.From(ctx).InfoContext(ctx, "run budget updated", log.RunID(args.RunID.String()))
	return run, nil
}

// runBudgetPayload is the run_budget_updated event body.
type runBudgetPayload struct {
	PreviousNanoUSD int64 `json:"previous_nano_usd"`
	BudgetNanoUSD   int64 `json:"budget_nano_usd"`
}
