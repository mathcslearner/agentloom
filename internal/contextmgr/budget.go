package contextmgr

// Provider-window budget arithmetic (ADR-014, ticket 12.6). A context-bearing
// llm step may declare an explicit `budget_tokens`, but even when it does not,
// the model's context window (from the pricing/model catalog) implies a default
// budget: the assembled context plus the completion's max_tokens plus a safety
// headroom must fit the window, or the provider rejects the request. These pure
// functions turn a window into a budget the compaction pipeline (12.4/12.5)
// compacts down to, so any llm step gets window-safety for free — no
// per-workflow budget authoring required.
//
// The budget is over the WHOLE framed request (the same count the assembly
// records and the guardrail checks), so "assembled ≤ budget" and "assembled +
// max_tokens ≤ window" are the same arithmetic. Keeping this a pure leaf (no
// engine, no store) lets the engine and tests share one definition of the
// default budget.

// HeadroomFraction is the fraction of the model context window reserved as a
// safety margin when defaulting a step's context budget from the window
// (ADR-014). It absorbs framing-estimate error and the Anthropic tokenizer
// calibration slop: the counted total is an estimate, and a single under-count
// against the real window is a hard provider 400, so the default budget stays a
// little under the window. An author who wants the full window sets
// budget_tokens explicitly (still capped at the window, see EffectiveBudget).
const HeadroomFraction = 0.05

// HeadroomFloorTokens is the minimum headroom (in tokens) reserved regardless
// of window size, so a small window still keeps a usable safety margin.
const HeadroomFloorTokens = 64

// Headroom returns the safety-margin token count for a given context window:
// max(⌈HeadroomFraction × window⌉, HeadroomFloorTokens), never exceeding the
// window itself.
func Headroom(window int) int {
	if window <= 0 {
		return 0
	}
	h := int(float64(window)*HeadroomFraction + 0.999999)
	if h < HeadroomFloorTokens {
		h = HeadroomFloorTokens
	}
	if h > window {
		h = window
	}
	return h
}

// DefaultBudget derives the default context budget from a model context window
// and the step's completion max_tokens (ADR-014, ticket 12.6):
//
//	budget = window − max_tokens − Headroom(window)
//
// ok is false when max_tokens plus the headroom already meet or exceed the
// window: there is no room for any assembled context, so the step cannot fit
// the window under any compaction and must fail before the call. maxTokens ≤ 0
// is treated as 0 (the caller resolves the step's effective max_tokens first).
func DefaultBudget(window, maxTokens int) (int, bool) {
	if window <= 0 {
		return 0, false
	}
	if maxTokens < 0 {
		maxTokens = 0
	}
	budget := window - maxTokens - Headroom(window)
	if budget <= 0 {
		return 0, false
	}
	return budget, true
}

// BudgetSource names how a step's effective context budget was chosen, for the
// context_assembled audit event.
type BudgetSource string

const (
	// BudgetSourceNone means no budget is in force — no explicit budget and no
	// window to default from. Compaction is inert; only 12.4's explicit-budget
	// path or the window guardrail can constrain the request, and neither
	// applies. (12.3 behavior.)
	BudgetSourceNone BudgetSource = ""
	// BudgetSourceExplicit means the author's budget_tokens is in force
	// unchanged (it is at or below the window default, or no window is known).
	BudgetSourceExplicit BudgetSource = "explicit"
	// BudgetSourceWindow means the budget was defaulted from the model context
	// window (no explicit budget_tokens).
	BudgetSourceWindow BudgetSource = "window"
	// BudgetSourceExplicitCapped means the author's budget_tokens exceeded the
	// window default and was tightened down to it — an explicit budget may
	// tighten but never loosen the provider-window guarantee.
	BudgetSourceExplicitCapped BudgetSource = "explicit_capped"
)

// EffectiveBudget combines an author's explicit budget with a window-derived
// default into the budget actually enforced, and reports which won (ADR-014,
// ticket 12.6). The rule: an explicit budget may only *tighten* the window
// default, never loosen it — compacting to a budget looser than the window
// would still overflow the provider. So:
//
//   - no explicit, no window default → no budget (ok=false, source none)
//   - no explicit, window default → the default (source window)
//   - explicit ≤ default (or no window default) → explicit (source explicit)
//   - explicit > default → the default (source explicit_capped)
//
// explicit is the authored *int (nil = absent). hasDefault/defaultBudget come
// from DefaultBudget. When hasDefault is false and explicit is nil, ok is false.
func EffectiveBudget(explicit *int, defaultBudget int, hasDefault bool) (int, BudgetSource, bool) {
	switch {
	case explicit == nil && !hasDefault:
		return 0, BudgetSourceNone, false
	case explicit == nil:
		return defaultBudget, BudgetSourceWindow, true
	case !hasDefault:
		return *explicit, BudgetSourceExplicit, true
	case *explicit <= defaultBudget:
		return *explicit, BudgetSourceExplicit, true
	default:
		return defaultBudget, BudgetSourceExplicitCapped, true
	}
}
