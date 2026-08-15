package cost_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/cost"
)

func TestCost(t *testing.T) {
	t.Parallel()

	// $3/1M input, $15/1M output. 1,000,000 input + 1,000,000 output should be
	// exactly $3 + $15 = $18 = 18e9 nano-USD.
	r := cost.Rate{InputPerMTok: 3.0, OutputPerMTok: 15.0}
	if got := cost.Cost(1_000_000, 1_000_000, r); got != 18*cost.NanoPerUSD {
		t.Errorf("Cost(1M,1M) = %d, want %d", got, 18*cost.NanoPerUSD)
	}

	// A single input token at $3/1M = 3000 nano-USD exactly.
	if got := cost.Cost(1, 0, r); got != 3000 {
		t.Errorf("Cost(1 input token) = %d, want 3000", got)
	}

	// Output-only.
	if got := cost.Cost(0, 2, r); got != 30_000 {
		t.Errorf("Cost(2 output tokens) = %d, want 30000", got)
	}
}

func TestCostRoundingHalfUp(t *testing.T) {
	t.Parallel()

	// Integer-exact case: $1.5/1M, 1 token = round(1 × 1.5e9 / 1e6) = 1500.
	if got := cost.Cost(1, 0, cost.Rate{InputPerMTok: 1.5}); got != 1500 {
		t.Errorf("Cost(1 token @ $1.5/1M) = %d, want 1500", got)
	}

	// Genuine half case: $0.0015/1M → 1,500,000 nano-USD/MTok; 1 token yields
	// 1.5 nano-USD before rounding, which must round up to 2 (half-up).
	if got := cost.Cost(1, 0, cost.Rate{InputPerMTok: 0.0015}); got != 2 {
		t.Errorf("Cost(1 token @ $0.0015/1M) = %d, want 2 (half rounds up)", got)
	}
}

func TestEstimateIsUpperBound(t *testing.T) {
	t.Parallel()

	r := cost.Rate{InputPerMTok: 3.0, OutputPerMTok: 15.0}
	// Estimate prices max_tokens as output; actual output <= max_tokens, so
	// the estimate is >= the eventual Cost for the same input.
	est := cost.Estimate(1000, 500, r)
	act := cost.Cost(1000, 200, r)
	if est < act {
		t.Errorf("Estimate %d < actual Cost %d — estimate must be an upper bound", est, act)
	}
	// Estimate equals Cost when actual output hits the cap.
	if cost.Estimate(1000, 500, r) != cost.Cost(1000, 500, r) {
		t.Error("Estimate should equal Cost when output == max_tokens")
	}
}

func TestCostZeroAndNegative(t *testing.T) {
	t.Parallel()

	r := cost.Rate{InputPerMTok: 3.0, OutputPerMTok: 15.0}
	if got := cost.Cost(0, 0, r); got != 0 {
		t.Errorf("Cost(0,0) = %d, want 0", got)
	}
	// Negative token counts (never expected) contribute zero, not negative.
	if got := cost.Cost(-5, -5, r); got != 0 {
		t.Errorf("Cost(-5,-5) = %d, want 0", got)
	}
	// A free model costs zero.
	if got := cost.Cost(1_000_000, 1_000_000, cost.Rate{}); got != 0 {
		t.Errorf("Cost with zero rate = %d, want 0", got)
	}
}

func TestToolCost(t *testing.T) {
	t.Parallel()

	if got := cost.ToolCost(0.01); got != 10_000_000 {
		t.Errorf("ToolCost($0.01) = %d, want 10000000", got)
	}
	if got := cost.ToolCost(0); got != 0 {
		t.Errorf("ToolCost($0) = %d, want 0", got)
	}
}

func TestCostSumsExactly(t *testing.T) {
	t.Parallel()

	// The property 10.2 relies on: summing per-attempt integer costs equals a
	// single computation over the totals only because each Cost is an exact
	// integer. Here we assert associativity of the ledger sum for a batch.
	r := cost.Rate{InputPerMTok: 3.0, OutputPerMTok: 15.0}
	attempts := [][2]int64{{100, 50}, {2000, 800}, {17, 3}, {999_999, 12_345}}
	var sum int64
	for _, a := range attempts {
		sum += cost.Cost(a[0], a[1], r)
	}
	// Recompute independently in a different order; integer addition is exact.
	var sum2 int64
	for i := len(attempts) - 1; i >= 0; i-- {
		sum2 += cost.Cost(attempts[i][0], attempts[i][1], r)
	}
	if sum != sum2 {
		t.Errorf("ledger sum order-dependent: %d vs %d", sum, sum2)
	}
}
