package engine

// Provider-window guardrails (ADR-014, ticket 12.6). A model's context window
// is a hard provider limit: assembled context + the completion's max_tokens
// must fit it, or the provider returns a 400 (context_length_exceeded) after
// the call is already billed. This file turns that into two pre-call
// mechanisms:
//
//   - The window-derived DEFAULT context budget. assembleContext (context.go)
//     defaults a context-bearing step's budget from the model window when the
//     author declared no explicit budget_tokens, so any llm step with a
//     `context` block auto-compacts to fit the window without per-workflow
//     budget authoring.
//
//   - The hard guard. guardWindow runs after the budget/downgrade stage and
//     before the rate limiter, over the FINAL (possibly downgraded) config, for
//     every llm step — with or without a context block. It counts the framed
//     request and fails the step permanently before any provider call if
//     preflight + max_tokens still exceeds the window (compaction absent, a
//     context-less oversize prompt, or a post-downgrade smaller window). It also
//     records the context_utilization metric for every guarded claim.
//
// The window comes from the pricing/model catalog (cost.Catalog.ContextWindow).
// A step whose model has no window entry is unguarded — the ADR-010 "unlimited
// by omission" stance — and guardWindow is a no-op for it.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// windowInfo is a resolved model context window for a claim: the pricing
// resource that owns it, the window in tokens, and the completion's effective
// max_tokens (both needed to compute the default budget and the hard check).
type windowInfo struct {
	resource  string
	window    int
	maxTokens int
}

// resolveWindow resolves the context window for a step's CURRENT config
// (sc.Config), using the executor's CostEstimate hook for the resolved resource
// and max_tokens and the pricing catalog for the window. It returns ok=false —
// the model is unguarded — when pricing is off, the executor is not a
// CostEstimator, the estimate cannot be built (an unresolvable model, which
// Execute will fail anyway), the resource is empty (not cost-bearing), or the
// catalog has no window for the resource. Pure aside from the catalog read; no
// store I/O.
func (e *Engine) resolveWindow(executor exec.Executor, sc exec.StepContext) (windowInfo, bool) {
	if e.pricing == nil {
		return windowInfo{}, false
	}
	ce, ok := executor.(exec.CostEstimator)
	if !ok {
		return windowInfo{}, false
	}
	est, err := ce.CostEstimate(sc)
	if err != nil || est.Resource == "" {
		return windowInfo{}, false
	}
	window, _, found := e.pricing.ContextWindow(est.Resource, e.now())
	if !found {
		return windowInfo{}, false
	}
	return windowInfo{resource: est.Resource, window: int(window), maxTokens: int(est.MaxTokens)}, true
}

// guardWindow is the hard provider-window check (ticket 12.6), run for every
// llm claim after the budget/downgrade stage and before the rate limiter. It
// counts the final framed request and, if preflight + max_tokens exceeds the
// model context window, fails the step permanently before any provider call
// (ADR-014's hard guarantee — overflow is never sent). It returns (handled,
// err): handled=true means the guard settled the step (the returned err is the
// completion result or a transport error to redeliver); handled=false means the
// step is within the window (or unguarded) and execution continues. On a
// within-window claim it records the context_utilization metric.
//
// A context-bearing step reaching here has already been compacted to fit
// window − headroom by assembleContext, so this normally passes for it; the
// guard's real work is context-less oversize prompts and the rare
// post-downgrade smaller-window case, plus recording utilization uniformly.
func (e *Engine) guardWindow(ctx context.Context, step gen.RunStep, executor exec.Executor, sc exec.StepContext, origin store.ClaimOrigin) (bool, error) {
	info, ok := e.resolveWindow(executor, sc)
	if !ok {
		return false, nil // unguarded model: nothing to check
	}
	pc, pcok := executor.(exec.PreflightCounter)
	if !pcok {
		return false, nil // cannot count the request: cannot guard it
	}
	counter := e.stepCounter(executor, sc)
	preflight, err := pc.PreflightTokens(sc, counter)
	if err != nil {
		// A build failure is non-fatal here — Execute lands any real config
		// failure; the guard just declines rather than fabricating a count.
		log.From(ctx).WarnContext(ctx, "context window guard preflight unavailable; skipping guard",
			slog.Any("error", err))
		return false, nil
	}
	framed := preflight + info.maxTokens
	if framed > info.window {
		e.metrics.ContextWindowRejection(info.resource)
		logger := log.From(ctx)
		logger.ErrorContext(ctx, "request exceeds model context window; failing step before provider call",
			slog.String("resource", info.resource),
			slog.Int("preflight_tokens", preflight),
			slog.Int("max_tokens", info.maxTokens),
			slog.Int("context_window", info.window))
		err := fmt.Errorf("context_window_exceeded: resource=%s preflight=%d max_tokens=%d window=%d (compaction absent or insufficient)",
			info.resource, preflight, info.maxTokens, info.window)
		return true, e.completeFailure(ctx, step, exec.Output{}, err, dag.ClassPermanent, origin.RunTrace)
	}
	if info.window > 0 {
		e.metrics.ContextUtilization(info.resource, float64(framed)/float64(info.window))
	}
	return false, nil
}

// windowDefaultBudget derives the window-defaulted context budget for a
// context-bearing step (ticket 12.6): the pure DefaultBudget over the resolved
// window and max_tokens. It returns ok=false when the model is unguarded (no
// window) or max_tokens alone already fills the window (no room for context —
// the caller then fails the step permanently, since no compaction can help).
// The bool distinguishes "no window default available" (fall back to 12.4's
// explicit-budget-only behavior) from "window leaves no room" (a permanent
// failure), which the caller separates by also checking whether a window was
// resolved.
func (e *Engine) windowDefaultBudget(info windowInfo, ok bool) (budget int, hasDefault bool, windowKnown bool) {
	if !ok {
		return 0, false, false
	}
	b, fits := contextmgr.DefaultBudget(info.window, info.maxTokens)
	return b, fits, true
}
