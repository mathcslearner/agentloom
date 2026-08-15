package engine

// This file is the ticket 10.4 model-downgrade decision (ADR-012): when a
// budgeted llm step carries a model_fallbacks chain, the claim-time budget
// check routes the claim to a cheaper model as the run approaches its budget,
// rather than parking or failing at the primary model's price. A downgrade is
// evaluated at the same scheduling point as the budget check (single-shot, no
// re-entry): budgetCheck prices the primary and each fallback tier, picks the
// appropriate cheaper tier, rewrites the rendered config's model, records a
// model_downgraded event, and lets the caller re-key the response cache and
// proceed. Because the cache key, resource limiter, cost estimate, and the
// provider call all derive from the config's model, that one rewrite
// re-targets the whole pipeline — a different model is a different cache key,
// asserted at the exec layer.
//
// Two triggers, per ADR-012's "Budget semantics":
//
//   - Soft (threshold): once the run's spend reaches a fallback's
//     at_budget_fraction, claims proactively route to that tier — even if the
//     primary would still fit — to conserve budget.
//   - Hard (projection): if the primary's projected spend (spend + estimate)
//     would exceed a budget, route to the least-aggressive fallback that fits,
//     avoiding a park.
//
// A tier is only chosen if it is priceable and its projection fits every
// budget: the engine never downgrades to a model it would immediately have to
// park on. When no fitting tier exists, the chain is exhausted and budgetCheck
// falls through to the ordinary park/fail action on the primary.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// downgradeCandidate is one priced fallback tier the decision considers.
type downgradeCandidate struct {
	model    string
	resource string
	nano     int64
	// priceable is false when the tier's model cannot be resolved or priced
	// (an unknown model under the fail-closed policy, a corrupt rewrite): the
	// decision never routes to it.
	priceable bool
	// fits is true when the tier's projected spend is within every budget.
	fits bool
	// thresholdMet is true when the run's spend has reached this tier's
	// at_budget_fraction soft trigger.
	thresholdMet bool
}

// selectDowngrade prices the primary and each fallback tier and picks the
// cheaper model to route to, or reports chose=false to fall through to the
// ordinary park/fail routing. It does no IO — the step's prior cost was read
// once by budgetCheck and threaded in — so it can be a plain method; the
// pure tier-selection is factored into chooseDowngrade for direct testing.
//
// A primary that cannot be priced (unknown model under fail-closed) yields no
// downgrade: budgetDecide then lands the primary's permanent failure, so the
// fail-closed judgment lives in one place.
func (e *Engine) selectDowngrade(ctx context.Context, dg exec.ModelDowngrader, estimator exec.CostEstimator, sc exec.StepContext, origin store.ClaimOrigin, primaryEst exec.CostEstimate, hasStepCap bool, stepPriorNano, stepCapNano int64, now time.Time) (json.RawMessage, store.ModelDowngradedEvent, bool) {
	var zero store.ModelDowngradedEvent
	current, chain, err := dg.ModelFallbacks(sc)
	if err != nil || len(chain) == 0 {
		return nil, zero, false
	}
	primaryNano, perr := e.estimateNanoUSD(primaryEst, now)
	if perr != nil {
		// The primary is unpriceable (fail-closed unknown model, or a
		// pathological catalog). Leave the primary's fate to budgetDecide.
		return nil, zero, false
	}

	fits := func(nano int64) bool {
		if origin.BudgetNanoUSD != nil && origin.SpentNanoUSD+nano > *origin.BudgetNanoUSD {
			return false
		}
		if hasStepCap && stepPriorNano+nano > stepCapNano {
			return false
		}
		return true
	}
	primaryFits := fits(primaryNano)

	var frac float64
	haveFrac := origin.BudgetNanoUSD != nil && *origin.BudgetNanoUSD > 0
	if haveFrac {
		frac = float64(origin.SpentNanoUSD) / float64(*origin.BudgetNanoUSD)
	}

	cands := make([]downgradeCandidate, len(chain))
	for j, f := range chain {
		c := downgradeCandidate{model: f.Model}
		if raw, werr := dg.WithModel(sc, f.Model); werr == nil {
			scc := sc
			scc.Config = raw
			if est, eerr := estimator.CostEstimate(scc); eerr == nil && est.Resource != "" {
				if nano, nerr := e.estimateNanoUSD(est, now); nerr == nil {
					c.priceable = true
					c.resource = est.Resource
					c.nano = nano
					c.fits = fits(nano)
				}
			}
		}
		if !c.priceable {
			log.From(ctx).DebugContext(ctx, "fallback model unpriceable; skipping tier",
				slog.String("model", f.Model))
		}
		if haveFrac && f.AtBudgetFraction != nil && frac >= *f.AtBudgetFraction {
			c.thresholdMet = true
		}
		cands[j] = c
	}

	idx, trigger := chooseDowngrade(cands, primaryFits)
	if idx < 0 {
		return nil, zero, false
	}
	target := cands[idx]
	raw, werr := dg.WithModel(sc, target.model)
	if werr != nil {
		// The tier priced cleanly a moment ago, so this cannot normally fail;
		// if it does, do not downgrade.
		return nil, zero, false
	}

	event := store.ModelDowngradedEvent{
		FromModel:           current,
		ToModel:             target.model,
		FromResource:        primaryEst.Resource,
		ToResource:          target.resource,
		Trigger:             trigger,
		SpentNanoUSD:        origin.SpentNanoUSD,
		FromEstimateNanoUSD: primaryNano,
		ToEstimateNanoUSD:   target.nano,
	}
	if origin.BudgetNanoUSD != nil {
		event.BudgetNanoUSD = *origin.BudgetNanoUSD
	}
	if trigger == store.DowngradeTriggerThreshold && chain[idx].AtBudgetFraction != nil {
		event.ThresholdFraction = *chain[idx].AtBudgetFraction
	}
	if trigger == store.DowngradeTriggerProjection {
		if origin.BudgetNanoUSD != nil {
			event.Limit = store.BudgetLimitRun
		} else {
			event.Limit = store.BudgetLimitStepUSD
		}
	}
	return raw, event, true
}

// chooseDowngrade is the pure tier selection over pre-priced candidates (the
// authored, cheapening chain) and whether the primary model fits its budgets.
// It returns the chain index to route to and the trigger, or (-1, "") for no
// downgrade. See the file header for the two triggers; the invariant is that a
// chosen tier is always priceable and fits.
func chooseDowngrade(cands []downgradeCandidate, primaryFits bool) (int, string) {
	// Soft trigger: the deepest (cheapest) tier whose threshold the run's
	// spend has met, if that tier is priceable and fits. Thresholds are
	// validated non-decreasing along the chain, so a met threshold forms a
	// prefix; the deepest met tier is the cheapest one authorized proactively.
	deepestMet := -1
	for j := len(cands) - 1; j >= 0; j-- {
		if cands[j].thresholdMet {
			deepestMet = j
			break
		}
	}
	if deepestMet >= 0 && cands[deepestMet].priceable && cands[deepestMet].fits {
		return deepestMet, store.DowngradeTriggerThreshold
	}

	// Otherwise a downgrade is warranted only if the primary does not fit (the
	// hard trigger) or a threshold fired but its named tier was unusable (a
	// rescue to a fitting tier). In either case route to the least-aggressive
	// (most expensive, first in authored order) tier that is priceable and
	// fits — a smaller quality drop than jumping straight to the cheapest.
	if !primaryFits || deepestMet >= 0 {
		for j := 0; j < len(cands); j++ {
			if cands[j].priceable && cands[j].fits {
				if deepestMet >= 0 {
					return j, store.DowngradeTriggerThreshold
				}
				return j, store.DowngradeTriggerProjection
			}
		}
	}
	return -1, ""
}

// recordDowngrade appends the model_downgraded event for a routed claim in its
// own short transaction (ticket 10.4). A downgrade is not a state transition —
// no status changes, and the used model becomes durable on the attempt's
// cost-ledger resource — so this only fences on the caller's live claim
// (RecordModelDowngrade rejects a zombie) and appends the event. A fenced
// caller surfaces a *store.TransitionError so budgetCheck abandons like any
// other fenced write; a transport error redelivers.
func (e *Engine) recordDowngrade(ctx context.Context, step gen.RunStep, event store.ModelDowngradedEvent) error {
	now := e.now()
	claimID := uuid.Nil
	if step.ClaimID != nil {
		claimID = *step.ClaimID
	}
	txCtx, span := e.tracer.Start(ctx, "step.downgrade")
	err := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		return store.RecordModelDowngrade(ctx, q, store.RecordModelDowngradeArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: claimID, Event: event, Now: now,
		})
	})
	endTxSpan(span, err)
	return err
}
