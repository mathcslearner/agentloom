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
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/validate"
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
		w := event.CostUnknownModelFrom(cost.NewUnknownModelWarning(resource, priced.Rate))
		args.Warning = &w
	}
	return args
}

// priceOverheads computes the cost_ledger overhead rows for a step's judge
// validators (ticket 11.5, ADR-012 rule 4). An llm_judge's provider call is
// attributed to the SERVING step, flagged overhead: true, so a breakdown can
// separate productive spend from validation machinery. It reads the judge
// token accounting off the verdict's per-validator results (the validator
// reports it; the engine ledgers it — the validator never touches the ledger)
// and prices each against the pricing catalog, exactly like priceAttempt but
// with Overhead set and a distinct entry key per chain position. Returns nil
// when pricing is off, there is no verdict, or no result carries usage. Pure
// (no database reads) — the caller writes the rows inside the completion
// transaction.
func (e *Engine) priceOverheads(ctx context.Context, step gen.RunStep, verdict *validate.Verdict, now time.Time) []store.AttemptCostArgs {
	if e.pricing == nil || verdict == nil {
		return nil
	}
	var rows []store.AttemptCostArgs
	for i, r := range verdict.Results {
		if r.Usage == nil || r.Usage.Resource == "" {
			continue
		}
		if row := e.priceOverhead(ctx, step, store.JudgeEntry(i), *r.Usage, now); row != nil {
			rows = append(rows, *row)
		}
	}
	return rows
}

// priceOverhead prices one judge call as an overhead row on the serving
// step's attempt (ticket 11.5). It always prices under PolicyEstimate — post-
// call pricing never fails a completed step over an unknown model (the money
// is already spent) — so an unknown judge model is priced at the catalog
// fallback with a cost_unknown_model warning, exactly like priceAttempt.
// Returns nil only when the model is unpriceable on a catalog with no
// fallback (a pathological override).
func (e *Engine) priceOverhead(ctx context.Context, step gen.RunStep, entry string, u validate.ValidatorUsage, now time.Time) *store.AttemptCostArgs {
	priced, err := e.pricing.PriceModel(u.Resource, now, cost.PolicyEstimate)
	if err != nil {
		log.From(ctx).WarnContext(ctx, "judge overhead unpriceable (no catalog fallback); recording no cost",
			slog.String("resource", u.Resource), slog.Any("error", err))
		return nil
	}
	rate, _ := json.Marshal(priced.Rate)
	usageJSON, _ := json.Marshal(exec.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens})
	amount := cost.Cost(u.InputTokens, u.OutputTokens, priced.Rate)
	args := &store.AttemptCostArgs{
		RunID: step.RunID, StepID: step.StepID, Attempt: step.AttemptCount,
		Entry: entry, Resource: u.Resource, Usage: usageJSON, Rate: rate,
		RateSource: priced.Source.String(), Overhead: true,
		CostNanoUSD: amount, Now: now,
	}
	if priced.Fallback {
		w := event.CostUnknownModelFrom(cost.NewUnknownModelWarning(u.Resource, priced.Rate))
		args.Warning = &w
	}
	return args
}

// priceSummaryOverheads computes the cost_ledger overhead rows for a step's
// summarize compaction calls (ticket 12.5, ADR-012 rule 4). A summarizer's
// provider call is attributed to the SERVING step, flagged overhead: true,
// under a compaction:<i> entry (the 0016-reserved same-attempt slot). A
// cache-served summary spent nothing, so it records only its counterfactual
// saved figure (like a cached provider call). It reads no database — the caller
// writes the rows inside the fenced context-record transaction, before any
// provider call, so a summary's spend is metered even if the step later fails.
// Returns nil when pricing is off or no summary carries usage.
func (e *Engine) priceSummaryOverheads(ctx context.Context, step gen.RunStep, summaries []contextmgr.SummaryAction, now time.Time) []store.AttemptCostArgs {
	if e.pricing == nil {
		return nil
	}
	var rows []store.AttemptCostArgs
	for i, s := range summaries {
		if s.Resource == "" {
			continue
		}
		priced, err := e.pricing.PriceModel(s.Resource, now, cost.PolicyEstimate)
		if err != nil {
			log.From(ctx).WarnContext(ctx, "summary overhead unpriceable (no catalog fallback); recording no cost",
				slog.String("resource", s.Resource), slog.Any("error", err))
			continue
		}
		rate, _ := json.Marshal(priced.Rate)
		usageJSON, _ := json.Marshal(exec.Usage{InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, CacheHit: s.CacheHit})
		amount := cost.Cost(s.InputTokens, s.OutputTokens, priced.Rate)
		args := store.AttemptCostArgs{
			RunID: step.RunID, StepID: step.StepID, Attempt: step.AttemptCount,
			Entry: store.CompactionEntry(i), Resource: s.Resource, Usage: usageJSON, Rate: rate,
			RateSource: priced.Source.String(), Overhead: true, CacheHit: s.CacheHit, Now: now,
		}
		// A cache hit spent nothing (the original summarization already
		// billed): meter only the counterfactual saved figure, and stay silent
		// on an unknown model (the miss already warned).
		if s.CacheHit {
			args.SavedNanoUSD = amount
		} else {
			args.CostNanoUSD = amount
			if priced.Fallback {
				w := event.CostUnknownModelFrom(cost.NewUnknownModelWarning(s.Resource, priced.Rate))
				args.Warning = &w
			}
		}
		rows = append(rows, args)
	}
	return rows
}

// recordCost emits the cost metrics for one priced attempt (ticket 10.5),
// post-commit and off the completion transaction. A cache hit spent nothing,
// so it records only its counterfactual savings; a productive call records its
// spend and the tokens it billed. Everything is labeled by the resolved
// resource alone (bounded to the pricing catalog, never run/step) per ADR-008.
func (e *Engine) recordCost(args store.AttemptCostArgs) {
	if args.CacheHit {
		e.metrics.CostSaved(args.Resource, args.SavedNanoUSD)
		return
	}
	e.metrics.CostSpent(args.Resource, args.CostNanoUSD)
	if len(args.Usage) > 0 {
		var u exec.Usage
		if json.Unmarshal(args.Usage, &u) == nil {
			e.metrics.CostTokens(args.Resource, u.InputTokens, u.OutputTokens)
		}
	}
}

// recordCostRows records the post-commit cost metrics for a productive
// attempt row (may be nil) and any judge overhead rows (ticket 11.5) — a
// convenience over recordCost for the completion paths that meter a
// productive charge plus overhead in one place.
func (e *Engine) recordCostRows(costRow *store.AttemptCostArgs, overheadRows []store.AttemptCostArgs) {
	if costRow != nil {
		e.recordCost(*costRow)
	}
	for _, oh := range overheadRows {
		e.recordCost(oh)
	}
}

// applyCostRows writes a productive attempt cost row (may be nil) and any
// judge overhead rows inside the current completion transaction (ticket
// 11.5). Called after the fenced step transition, under the run lock, exactly
// like completeSuccess's ledgering — so a fenced completion (which returns
// before this) never ledgers.
func applyCostRows(ctx context.Context, q store.Querier, costRow *store.AttemptCostArgs, overheadRows []store.AttemptCostArgs) error {
	if costRow != nil {
		if _, err := store.ApplyAttemptCost(ctx, q, *costRow); err != nil {
			return err
		}
	}
	for _, oh := range overheadRows {
		if _, err := store.ApplyAttemptCost(ctx, q, oh); err != nil {
			return err
		}
	}
	return nil
}

// validatorErrorUsage extracts a judge call's billed token usage from a
// validation-stage transport error (ticket 11.5): an llm_judge that billed a
// provider call but then produced a malformed answer under `on_error: fail`
// carries its usage on the *validate.Error so the engine can meter it as
// overhead even though the stage failed. Returns nil for every other error
// (a provider availability failure billed nothing; a mechanical executor
// failure is not a *validate.Error at all).
func validatorErrorUsage(err error) *validate.ValidatorUsage {
	var ve *validate.Error
	if errors.As(err, &ve) {
		return ve.Usage
	}
	return nil
}
