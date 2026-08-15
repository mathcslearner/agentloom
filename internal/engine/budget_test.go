package engine

import (
	"context"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// i64 returns a pointer to an int64 literal, for the nullable budget columns.
func i64(v int64) *int64 { return &v }

// TestBudgetDecide covers the pure claim-time budget decision on the paths
// that touch no database (run budget, step max_tokens, unknown-model
// fail-closed, free tool). The step max_usd path reads the ledger and is
// covered by the integration suite.
func TestBudgetDecide(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	cat, err := cost.Default()
	if err != nil {
		t.Fatalf("cost.Default: %v", err)
	}

	// The mock estimate the cases budget around.
	priced, err := cat.PriceModel("mock:sim-1", now, cost.PolicyEstimate)
	if err != nil {
		t.Fatalf("PriceModel: %v", err)
	}
	estNano := cost.Estimate(1000, 1000, priced.Rate)
	if estNano <= 0 {
		t.Fatalf("estimate is not positive: %d", estNano)
	}
	modelEst := exec.CostEstimate{Resource: "mock:sim-1", InputTokens: 1000, MaxTokens: 1000}

	cases := []struct {
		name       string
		est        exec.CostEstimate
		stepBudget *dag.StepBudget
		origin     store.ClaimOrigin
		policy     cost.UnknownModelPolicy
		want       budgetAction
		wantLimit  string
	}{
		{
			name:   "run budget proceed when projection fits",
			est:    modelEst,
			origin: store.ClaimOrigin{BudgetNanoUSD: i64(estNano + 1000), SpentNanoUSD: 0, OnBudgetExceeded: string(dag.BudgetPark)},
			want:   budgetProceed,
		},
		{
			name:      "run budget park when projection exceeds",
			est:       modelEst,
			origin:    store.ClaimOrigin{BudgetNanoUSD: i64(estNano - 1), SpentNanoUSD: 0, OnBudgetExceeded: string(dag.BudgetPark)},
			want:      budgetPark,
			wantLimit: store.BudgetLimitRun,
		},
		{
			name:      "run budget fail under fail policy",
			est:       modelEst,
			origin:    store.ClaimOrigin{BudgetNanoUSD: i64(estNano - 1), SpentNanoUSD: 0, OnBudgetExceeded: string(dag.BudgetFail)},
			want:      budgetFail,
			wantLimit: store.BudgetLimitRun,
		},
		{
			name:       "step max_tokens fail on oversized request",
			est:        modelEst,
			stepBudget: &dag.StepBudget{MaxTokens: 10},
			origin:     store.ClaimOrigin{}, // unbudgeted run
			want:       budgetFail,
			wantLimit:  store.BudgetLimitStepTokens,
		},
		{
			name:      "unknown model fail-closed",
			est:       exec.CostEstimate{Resource: "ghost:model", InputTokens: 100, MaxTokens: 100},
			origin:    store.ClaimOrigin{BudgetNanoUSD: i64(1_000_000_000)},
			policy:    cost.PolicyFail,
			want:      budgetFail,
			wantLimit: store.BudgetLimitRun,
		},
		{
			name:   "unknown model proceeds under estimate policy within budget",
			est:    exec.CostEstimate{Resource: "ghost:model", InputTokens: 1, MaxTokens: 1},
			origin: store.ClaimOrigin{BudgetNanoUSD: i64(1_000_000_000), OnBudgetExceeded: string(dag.BudgetPark)},
			policy: cost.PolicyEstimate,
			want:   budgetProceed,
		},
		{
			name:   "free tool proceeds even under a tiny budget",
			est:    exec.CostEstimate{Resource: "tool:json_transform"},
			origin: store.ClaimOrigin{BudgetNanoUSD: i64(1), OnBudgetExceeded: string(dag.BudgetPark)},
			want:   budgetProceed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{pricing: cat, unknownModelPolicy: tc.policy, now: func() time.Time { return now }}
			dec, err := e.budgetDecide(context.Background(), gen.RunStep{}, tc.est, tc.stepBudget, tc.origin, now)
			if err != nil {
				t.Fatalf("budgetDecide: %v", err)
			}
			if dec.action != tc.want {
				t.Errorf("action = %d, want %d", dec.action, tc.want)
			}
			if tc.wantLimit != "" && dec.event.Limit != tc.wantLimit {
				t.Errorf("limit = %q, want %q", dec.event.Limit, tc.wantLimit)
			}
		})
	}
}
