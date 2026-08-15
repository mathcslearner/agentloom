package engine

// This file is the ticket 10.3 budget middleware (ADR-012): the claim-time
// check that projects a cost-bearing step's pre-flight cost and compares it
// against the run's spend budget and the step's own caps before any money is
// spent. It sits in execute() after the cache read (a hit is $0, never
// budget-gated) and before the rate limiter (parking after a limiter acquire
// would strand debited tokens 9.3 only reconciles on success) — so the
// budget decision is at the same scheduling point as the readiness/claim
// decision, which is what makes cost enforcement a scheduling feature rather
// than a reporting afterthought.
//
// The decision is a pure function of the claim-time run spend (carried on the
// origin, read under the run lock the claim already took — exact for a
// sequential chain, and conservative-low for concurrent fan-out, so it never
// parks spuriously), the pre-flight estimate the executor's CostEstimator
// projects, the pricing catalog, and the materialized budgets. Actions:
// proceed, park the run (reason budget_exceeded, resumable), or fail the step
// permanently (a cap it can never satisfy, or the fail policy). A step
// already in flight is never killed by a budget crossing — the decision is
// pre-execution only; the 10.2 ledger's post-call actuals correct the
// upper-bound estimate for the next claim.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// budgetAction is what the budget check decided to do with a claim.
type budgetAction int

const (
	// budgetProceed: the projected spend is within every budget — run the step.
	budgetProceed budgetAction = iota
	// budgetPark: the projected spend exceeds the run budget under the park
	// policy — release the step and park the run, resumable by raising the
	// budget and unparking.
	budgetPark
	// budgetFail: a permanent budget violation — an unpriceable model under
	// fail-closed, a step cap the request can never satisfy, or the run
	// budget under the fail policy. Dead-letter the step permanently.
	budgetFail
)

// budgetDecision is the pure result of the claim-time budget check: the
// action plus the projection detail recorded on the budget_exceeded event
// (park) or the dead-letter error (fail).
type budgetDecision struct {
	action budgetAction
	// reason is a human message for the fail path's dead-letter error.
	reason string
	// event carries the projection detail (only populated when the action is
	// not proceed).
	event store.BudgetExceededEvent
}

// budgetCheck enforces the run and step budgets before a cost-bearing step
// executes (ticket 10.3, ADR-012), and — when the step carries a
// model_fallbacks chain — routes the claim to a cheaper model as the run
// approaches its budget (ticket 10.4). It returns:
//
//   - handled=true, err: a terminal action was taken (the run parked, or the
//     step failed) and the delivery is done (err is the Handle/ACK result).
//   - handled=false, newConfig!=nil: the claim was downgraded to a cheaper
//     model; the caller re-keys the response cache on newConfig and proceeds.
//   - handled=false, newConfig=nil: proceed on the current model unchanged.
//
// The middleware is a no-op — handled=false, no reads — when pricing is
// disabled, the executor is not a CostEstimator, or the run is unbudgeted and
// the step carries no caps. Otherwise it projects the estimate, prices it,
// tries a downgrade, and routes.
func (e *Engine) budgetCheck(ctx context.Context, step gen.RunStep, executor exec.Executor, sc exec.StepContext, origin store.ClaimOrigin) (bool, json.RawMessage, error) {
	if e.pricing == nil {
		return false, nil, nil // budget enforcement needs a catalog to price against
	}
	estimator, ok := executor.(exec.CostEstimator)
	if !ok {
		return false, nil, nil // not a cost-bearing step
	}
	stepBudget, sberr := decodeStepBudget(step.BudgetPolicy)
	if sberr != nil {
		// A corrupt materialized budget policy is a deterministic stored-state
		// failure (ADR-006 taxonomy row 7 in spirit) — fail permanent rather
		// than run unbudgeted.
		log.From(ctx).ErrorContext(ctx, "corrupt materialized budget policy; recording step failure",
			slog.Any("error", sberr))
		return true, nil, e.completeFailure(ctx, step,
			exec.Permanentf("corrupt budget policy: %v", sberr), dag.ClassPermanent, origin.RunTrace)
	}
	if origin.BudgetNanoUSD == nil && stepBudget == nil {
		return false, nil, nil // unbudgeted run, uncapped step: nothing to enforce
	}

	est, err := estimator.CostEstimate(sc)
	if err != nil {
		// The estimate could not be computed (unresolvable model, corrupt
		// config). Defer to Execute's classified failure — the routing
		// judgment lives in one place (the ResourceClaim/CacheBinding
		// convention).
		log.From(ctx).DebugContext(ctx, "cost estimate unavailable; skipping budget check",
			slog.Any("error", err))
		return false, nil, nil
	}
	if est.Resource == "" {
		return false, nil, nil // executor reported no cost-bearing resource
	}

	now := e.now()

	// Step-level max_tokens (model-independent): the whole projected request
	// (input estimate + output ceiling) must fit under the cap, or the
	// oversized request is never sent. A cheaper model cannot rescue an
	// oversized request, so this is checked before any downgrade. Validation
	// restricts this cap to llm steps.
	projectedTokens := est.InputTokens + est.MaxTokens
	if stepBudget != nil && stepBudget.MaxTokens > 0 && projectedTokens > int64(stepBudget.MaxTokens) {
		log.From(ctx).WarnContext(ctx, "step request exceeds max_tokens; recording permanent failure",
			slog.String("resource", est.Resource))
		return true, nil, e.completeFailure(ctx, step,
			exec.Permanentf("budget_exceeded: projected %d tokens exceeds step max_tokens %d", projectedTokens, stepBudget.MaxTokens),
			dag.ClassPermanent, origin.RunTrace)
	}

	// Read the step's cumulative cost once (the only IO), shared by the
	// downgrade fits-check and the step max_usd cap below.
	hasStepCap := stepBudget != nil && stepBudget.MaxUSD != nil
	var stepPriorNano, stepCapNano int64
	if hasStepCap {
		var rerr error
		stepPriorNano, rerr = e.store.Ledger().SumByStep(ctx, step.RunID, step.StepID)
		if rerr != nil {
			return false, nil, fmt.Errorf("engine: budget check: reading step cost: %w", rerr)
		}
		stepCapNano = usdToNano(*stepBudget.MaxUSD)
	}

	// Model downgrade (ticket 10.4): if the executor supports it and the step
	// carries a fallback chain, route to a cheaper model that fits before
	// parking or failing. Only fires when a strictly-cheaper, budget-fitting
	// tier exists; otherwise falls through to the ordinary park/fail routing.
	if dg, ok := executor.(exec.ModelDowngrader); ok {
		newCfg, event, chose := e.selectDowngrade(ctx, dg, estimator, sc, origin, est, hasStepCap, stepPriorNano, stepCapNano, now)
		if chose {
			if rerr := e.recordDowngrade(ctx, step, event); rerr != nil {
				var fenced *store.TransitionError
				if errors.As(rerr, &fenced) {
					return true, nil, e.abandonFenced(ctx, step, fenced, rerr)
				}
				// A transport error means the downgrade was not recorded and
				// nothing was decided: redeliver (handled=true carries the
				// error up so the entry is not acked), rather than silently
				// proceeding on the primary model.
				return true, nil, rerr
			}
			log.From(ctx).InfoContext(ctx, "model downgraded for budget",
				slog.String("from_model", event.FromModel),
				slog.String("to_model", event.ToModel),
				slog.String("trigger", event.Trigger))
			return false, newCfg, nil
		}
	}

	decision := e.budgetDecide(ctx, est, origin, hasStepCap, stepPriorNano, stepCapNano, now)
	switch decision.action {
	case budgetProceed:
		return false, nil, nil
	case budgetPark:
		return true, nil, e.completeBudgetPark(ctx, step, decision.event)
	case budgetFail:
		log.From(ctx).WarnContext(ctx, "step exceeds budget; recording permanent failure",
			slog.String("limit", decision.event.Limit),
			slog.String("resource", est.Resource))
		return true, nil, e.completeFailure(ctx, step,
			exec.Permanentf("%s", decision.reason), dag.ClassPermanent, origin.RunTrace)
	default:
		return false, nil, nil
	}
}

// budgetDecide is the pure park/fail/proceed decision on the current model
// (reached only when no downgrade applied): price the estimate, check the
// step max_usd cap (permanent on violation — a cap the request can never
// satisfy), then the run budget (park or fail per the run's disposition). The
// step's cumulative cost was read once by budgetCheck and threaded in, so
// this does no IO. max_tokens is checked upstream (model-independent).
func (e *Engine) budgetDecide(ctx context.Context, est exec.CostEstimate, origin store.ClaimOrigin, hasStepCap bool, stepPriorNano, stepCapNano int64, now time.Time) budgetDecision {
	// Price the estimate. An unpriced tool is free (estimate 0); an unknown
	// model under fail-closed fails permanent before any spend.
	estimateNano, perr := e.estimateNanoUSD(est, now)
	if perr != nil {
		var unknown *cost.UnknownModelError
		if errors.As(perr, &unknown) {
			return budgetDecision{
				action: budgetFail,
				reason: fmt.Sprintf("budget_exceeded: model %q is not in the pricing catalog and the unknown-model policy is fail-closed", unknown.Model),
				event: store.BudgetExceededEvent{
					Resource: est.Resource, Limit: store.BudgetLimitRun,
				},
			}
		}
		// Any other pricing error (a pathological catalog with no fallback):
		// do not fail a step over pricing infrastructure — proceed unbudgeted
		// for this step, logging the gap.
		log.From(ctx).WarnContext(ctx, "budget estimate unpriceable; proceeding without a budget check for this step",
			slog.String("resource", est.Resource), slog.Any("error", perr))
		return budgetDecision{action: budgetProceed}
	}

	// Step-level max_usd: the step's cumulative cost so far plus this
	// attempt's estimate must fit under the cap. A step whose retries would
	// push its own total over the cap fails permanent.
	if hasStepCap {
		projected := stepPriorNano + estimateNano
		if projected > stepCapNano {
			return budgetDecision{
				action: budgetFail,
				reason: fmt.Sprintf("budget_exceeded: step projected spend %s exceeds step max_usd %s", nanoStr(projected), nanoStr(stepCapNano)),
				event: store.BudgetExceededEvent{
					Resource: est.Resource, Limit: store.BudgetLimitStepUSD,
					SpentNanoUSD: stepPriorNano, EstimateNanoUSD: estimateNano,
					ProjectedNanoUSD: projected, BudgetNanoUSD: stepCapNano,
				},
			}
		}
	}

	// Run-level budget: projected run spend (spend so far + this estimate)
	// against the run budget. A $0 estimate can never exceed a budget, so a
	// free step never triggers a park (and never bounces unpark→park).
	if origin.BudgetNanoUSD != nil && estimateNano > 0 {
		projected := origin.SpentNanoUSD + estimateNano
		if projected > *origin.BudgetNanoUSD {
			event := store.BudgetExceededEvent{
				Resource: est.Resource, Limit: store.BudgetLimitRun,
				SpentNanoUSD: origin.SpentNanoUSD, EstimateNanoUSD: estimateNano,
				ProjectedNanoUSD: projected, BudgetNanoUSD: *origin.BudgetNanoUSD,
			}
			if origin.OnBudgetExceeded == string(dag.BudgetFail) {
				return budgetDecision{
					action: budgetFail,
					reason: fmt.Sprintf("budget_exceeded: projected run spend %s exceeds budget %s", nanoStr(projected), nanoStr(*origin.BudgetNanoUSD)),
					event:  event,
				}
			}
			return budgetDecision{action: budgetPark, event: event}
		}
	}
	return budgetDecision{action: budgetProceed}
}

// estimateNanoUSD prices a pre-flight estimate in nano-USD. A tool resource is
// its flat catalog rate (0 when unpriced/free); a model resource is the
// upper-bound Estimate at the resolved rate, honoring the engine's
// unknown-model policy (fail-closed returns *cost.UnknownModelError).
func (e *Engine) estimateNanoUSD(est exec.CostEstimate, now time.Time) (int64, error) {
	if strings.HasPrefix(est.Resource, toolPrefix) {
		nano, ok := e.pricing.ToolPrice(est.Resource, now)
		if !ok {
			return 0, nil // unpriced tool is free
		}
		return nano, nil
	}
	priced, err := e.pricing.PriceModel(est.Resource, now, e.unknownModelPolicy)
	if err != nil {
		return 0, err
	}
	return cost.Estimate(est.InputTokens, est.MaxTokens, priced.Rate), nil
}

// completeBudgetPark releases an over-budget step back to ready and parks the
// run (ticket 10.3), following the throttle route's shape: on a cancelling
// run the park is abandoned and the step settles as cancelled (the run must
// converge, not wait for a budget raise); otherwise BudgetParkStep releases
// the step, records the budget_exceeded event, and parks the run in one
// transaction. The released step needs no re-dispatch here (unpark re-outboxes
// it), so unlike the throttle route there is no delayed envelope to trace. The
// return follows the Handle/ACK contract: nil = committed, non-nil = redeliver.
func (e *Engine) completeBudgetPark(ctx context.Context, step gen.RunStep, event store.BudgetExceededEvent) error {
	logger := log.From(ctx)
	now := e.now()
	cancelSettled := false
	var terminalRun *gen.Run
	var fenced *store.TransitionError
	txCtx, txSpan := e.tracer.Start(ctx, "step.completion")
	txErr := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		status, serr := store.LockRunStatus(ctx, q, step.RunID, now)
		if serr != nil {
			return serr
		}
		if status == store.RunStatusCancelling {
			if _, cerr := store.CancelRunningStep(ctx, q, store.CancelRunningStepArgs{
				RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
				Reason: store.CancelReasonRunCancelled, Error: nil, Now: now,
			}); cerr != nil {
				errors.As(cerr, &fenced)
				return cerr
			}
			cancelSettled = true
			var rerr error
			terminalRun, rerr = attemptCancelRollup(ctx, q, step.RunID, now)
			return rerr
		}
		if _, err := store.BudgetParkStep(ctx, q, store.BudgetParkStepArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
			Event: event, Now: now,
		}); err != nil {
			errors.As(err, &fenced)
			return err
		}
		return nil
	})
	endTxSpan(txSpan, txErr)
	if txErr != nil {
		if fenced != nil {
			return e.abandonFenced(ctx, step, fenced, txErr)
		}
		logger.ErrorContext(ctx, "budget-park transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return txErr
	}
	if cancelSettled {
		logger.InfoContext(ctx, "step over budget on a cancelling run — settled as cancelled",
			slog.Bool("run_cancelled", terminalRun != nil))
		if terminalRun != nil {
			e.recordRunCompleted(terminalRun.Status, terminalRun.StartedAt, now)
		}
		return nil
	}
	logger.InfoContext(ctx, "run parked: budget exceeded",
		slog.String("resource", event.Resource),
		slog.String("limit", event.Limit),
		slog.Int64("projected_nano_usd", event.ProjectedNanoUSD),
		slog.Int64("budget_nano_usd", event.BudgetNanoUSD))
	return nil
}

// decodeStepBudget decodes a step's materialized budget policy (nil = no
// `budget` block was authored).
func decodeStepBudget(raw json.RawMessage) (*dag.StepBudget, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var b dag.StepBudget
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// usdToNano converts a USD cap to integer nano-USD (half away from zero),
// matching store.usdToNanoUSD and cost.USDToNano — the caps a run was
// instantiated with are already nano, so this only handles the per-step USD
// caps read from budget_policy.
func usdToNano(usd float64) int64 { return cost.USDToNano(usd) }

// nanoStr renders a nano-USD amount as a USD string for a human error message.
func nanoStr(nano int64) string {
	whole := nano / cost.NanoPerUSD
	frac := nano % cost.NanoPerUSD
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("$%d.%09d", whole, frac)
}
