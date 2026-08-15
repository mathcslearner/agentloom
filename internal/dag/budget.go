package dag

// This file is the definition-contract half of ADR-012's budget semantics:
// the run-level spend budget and its exceed policy, and the optional
// per-step budget caps an author may carry on the step envelope. The
// runtime half — the claim-time projection check, park/fail enforcement,
// the cost ledger — lives in internal/cost, internal/engine, and the store
// (M10.2–10.4); this package only defines and validates the authored
// fields.

// BudgetPolicy is the run-level action taken when a claim's projected spend
// would exceed the run's `budget_usd` (ADR-012 "Budget semantics"). An
// absent `on_budget_exceeded` means BudgetPark — the resumable default.
type BudgetPolicy string

// The budget-exceed policies (ADR-012). `downgrade` (routing the claim to a
// cheaper model in the step's fallback chain) is ticket 10.4 and joins this
// vocabulary then; 10.3 ships the two terminal actions.
const (
	// BudgetPark parks the run with reason `budget_exceeded` (ADR-006's M5.6
	// park primitive: no lease or worker slot held while parked), resumable
	// by raising the budget (PATCH /v1/runs/{id}/budget) and unparking. The
	// default.
	BudgetPark BudgetPolicy = "park"

	// BudgetFail dead-letters the over-budget step permanently, so the run's
	// on_failure policy disposes of it (fail the run, or write off the
	// branch and continue).
	BudgetFail BudgetPolicy = "fail"
)

// budgetPolicies enumerates the valid BudgetPolicy values in documentation
// order.
var budgetPolicies = []BudgetPolicy{BudgetPark, BudgetFail}

// StepBudget is a step's authored budget caps (ADR-012); nil on the step
// envelope means the `budget` key was absent (no per-step cap — the run
// budget still applies). Uniform across step types, so it lives on the step
// envelope, not in the per-type config — like retry, timeout, and cache. At
// least one cap must be set when the block is present (Validate rejects an
// empty block).
type StepBudget struct {
	// MaxUSD caps this step's own cumulative cost across all its attempts;
	// nil means the key was absent (no per-step USD cap). A claim whose
	// projected spend would push the step's total over the cap is refused
	// pre-flight. In USD (converted to nano-USD at instantiation).
	MaxUSD *float64 `json:"max_usd,omitempty"`

	// MaxTokens caps the projected request size (estimated input tokens plus
	// the configured output ceiling) for an llm step; zero means the key was
	// absent. A request whose projection exceeds it is never sent — the
	// oversized-request guard. Applies only to llm steps (Validate rejects it
	// elsewhere: a cap that can never fire is an authoring mistake). Distinct
	// from the per-type `config.max_tokens`, which is the provider's output
	// ceiling — this bounds the whole projected request.
	MaxTokens int `json:"max_tokens,omitempty"`
}
