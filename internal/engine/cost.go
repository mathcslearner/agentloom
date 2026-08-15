package engine

// This file is the ticket 10.2 cost middleware (ADR-012): the layer that
// prices a cost-bearing attempt after it succeeds and hands the store a
// ledger row to write inside the completion transaction. Pricing is a pure
// function of the attempt's resolved resource, its token usage, and the
// pricing catalog effective at completion time — no database reads — so
// complete.go computes the row before the transaction and only the insert +
// aggregate bump run under the run lock.
//
// Attribution follows ADR-012: an llm attempt is tokens × rate at the served
// model; a priced tool is its flat per-call charge; a cache hit (ADR-011) is
// a $0 row whose "saved" figure is the counterfactual would-have-cost; a
// tool with no catalog entry, a step that named no resource, and an attempt
// with no usage all ledger nothing. Post-call pricing never fails a succeeded
// attempt (the money is already spent): an unknown model is priced at the
// catalog fallback with a cost_unknown_model warning event, exactly the
// PolicyEstimate behavior the caller always passes here.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// toolPrefix is the ADR-010/ADR-012 tool resource namespace ("tool:<name>").
const toolPrefix = "tool:"

// toolRateSnapshot is the rate provenance stored on a priced-tool ledger row.
// A tool charge is flat per call, so the rate equals the cost — recording the
// per-call nano-USD is the auditable snapshot (ADR-012: the row's rate, not
// the catalog's current content, is what makes historical spend auditable).
type toolRateSnapshot struct {
	PerCallNanoUSD int64 `json:"per_call_nano_usd"`
}

// priceAttempt computes the cost_ledger row for one succeeded attempt, or nil
// when the attempt ledgers nothing (ADR-012): pricing disabled, no resource
// (not a provider/tool call), an unpriced tool (free), a model attempt with
// no usage, or an unpriceable model on a catalog with no fallback. It reads no
// database — the caller runs the returned row's write inside the completion
// transaction. now selects the effective-dated rate and stamps the row.
func (e *Engine) priceAttempt(ctx context.Context, step gen.RunStep, out exec.Output, now time.Time) *store.AttemptCostArgs {
	if e.pricing == nil {
		return nil // cost ledger disabled
	}
	resource := out.Resource
	if resource == "" {
		return nil // not a cost-bearing attempt (no provider/tool call)
	}
	cacheHit := out.Usage != nil && out.Usage.CacheHit

	if strings.HasPrefix(resource, toolPrefix) {
		nano, ok := e.pricing.ToolPrice(resource, now)
		if !ok {
			return nil // unpriced tool = free (ADR-012 rule 3), no ledger row
		}
		rate, _ := json.Marshal(toolRateSnapshot{PerCallNanoUSD: nano})
		args := &store.AttemptCostArgs{
			RunID: step.RunID, StepID: step.StepID, Attempt: step.AttemptCount,
			Resource: resource, Rate: rate, RateSource: cost.SourceExact.String(),
			CacheHit: cacheHit, Now: now,
		}
		// A cache hit costs $0 now; the counterfactual is what the call would
		// have billed (ADR-012 rule 2).
		if cacheHit {
			args.SavedNanoUSD = nano
		} else {
			args.CostNanoUSD = nano
		}
		return args
	}

	// Model resource. No usage ⇒ no cost row (ADR-012 rule 5) — the llm
	// executor always sets usage on success, so this only guards corrupt or
	// hand-built outputs.
	if out.Usage == nil {
		return nil
	}
	priced, err := e.pricing.PriceModel(resource, now, cost.PolicyEstimate)
	if err != nil {
		// Only reachable when the model is unknown AND the catalog carries no
		// fallback — impossible with the embedded defaults, so a pathological
		// override. Never fail a succeeded attempt over pricing: skip the row.
		log.From(ctx).WarnContext(ctx, "attempt unpriceable (no catalog fallback); recording no cost",
			slog.String("resource", resource), slog.Any("error", err))
		return nil
	}
	rate, _ := json.Marshal(priced.Rate)
	usageJSON, _ := json.Marshal(out.Usage)
	amount := cost.Cost(out.Usage.InputTokens, out.Usage.OutputTokens, priced.Rate)
	args := &store.AttemptCostArgs{
		RunID: step.RunID, StepID: step.StepID, Attempt: step.AttemptCount,
		Resource: resource, Usage: usageJSON, Rate: rate,
		RateSource: priced.Source.String(), CacheHit: cacheHit, Now: now,
	}
	if cacheHit {
		args.SavedNanoUSD = amount
	} else {
		args.CostNanoUSD = amount
	}
	// A fallback-priced (unknown) model warns the operator to add a catalog
	// entry — but only when real money was spent. A cache hit spent nothing,
	// and the original miss already warned, so a hit stays silent.
	if priced.Fallback && !cacheHit {
		if w, werr := json.Marshal(cost.NewUnknownModelWarning(resource, priced.Rate)); werr == nil {
			args.Warning = w
		}
	}
	return args
}
