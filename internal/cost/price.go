package cost

import "math"

// Money is integer nano-USD. See the package doc for why costs are integers.
const (
	// NanoPerUSD is the nano-USD in one US dollar (1e9).
	NanoPerUSD = int64(1_000_000_000)
	// tokensPerMTok is the token count a $/1M-token rate is quoted against.
	tokensPerMTok = int64(1_000_000)
)

// Cost is the attributed cost in nano-USD of one LLM attempt: input tokens
// priced at the input rate plus output tokens at the output rate, per ADR-012.
// Negative token counts (never expected) contribute zero. The result is
// deterministic for given inputs, so 10.2 can sum ledger rows and get exactly
// the run aggregate.
func Cost(inputTokens, outputTokens int64, r Rate) int64 {
	return component(inputTokens, r.InputPerMTok) + component(outputTokens, r.OutputPerMTok)
}

// Estimate is the pre-flight upper-bound cost of an LLM attempt (ADR-012): the
// estimated input tokens priced as input, plus the configured max_tokens
// priced as output — output is capped at max_tokens, so this never
// under-estimates the true output cost. 10.3 checks projected spend against it
// before the call; the post-call re-evaluation with actuals corrects the
// difference (the 9.3 reconciliation shape, applied to money).
func Estimate(inputTokens, maxTokens int64, r Rate) int64 {
	return Cost(inputTokens, maxTokens, r)
}

// ToolCost is the attributed cost in nano-USD of one priced-tool invocation.
func ToolCost(perCallUSD float64) int64 {
	return usdToNano(perCallUSD)
}

// USDToNano converts a USD amount to integer nano-USD, rounding half away
// from zero and clamping negatives to zero. Run instantiation uses it to
// materialize a validated (positive) budget_usd as the integer nano-USD
// budget the claim-time check (10.3) compares against the run's integer
// spend, so budget and spend share one unit and the comparison is exact.
func USDToNano(usd float64) int64 { return usdToNano(usd) }

// component prices one direction: round(tokens × usdPerMTok / 1e6) in
// nano-USD, half-up. The rate is converted to nano-USD-per-MTok first so the
// division is exact integer arithmetic. tokens × nanoPerMTok stays well within
// int64 for realistic attempts (200k tokens at $100/1M ≈ 2e16, versus the
// ~9.2e18 int64 ceiling).
func component(tokens int64, usdPerMTok float64) int64 {
	if tokens <= 0 || usdPerMTok <= 0 {
		return 0
	}
	nanoPerMTok := usdToNano(usdPerMTok)
	return (tokens*nanoPerMTok + tokensPerMTok/2) / tokensPerMTok
}

// usdToNano converts a USD amount to nano-USD, rounding half away from zero.
// Negative amounts (never expected from a validated catalog) clamp to zero.
func usdToNano(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(math.Round(usd * float64(NanoPerUSD)))
}
