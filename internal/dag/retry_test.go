package dag_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// defaultResolved is the all-defaults effective policy every merge case
// starts from.
func defaultResolved() dag.ResolvedRetryPolicy {
	return dag.ResolvedRetryPolicy{
		MaxAttempts: dag.DefaultRetryMaxAttempts,
		Backoff: dag.ResolvedBackoff{
			Initial:    dag.DefaultBackoffInitial,
			Cap:        dag.DefaultBackoffCap,
			Multiplier: dag.DefaultBackoffMultiplier,
		},
		Jitter:  dag.DefaultRetryJitter,
		RetryOn: dag.DefaultRetryOn(),
	}
}

func TestResolveRetryPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     *dag.RetryPolicy
		mutate func(*dag.ResolvedRetryPolicy)
	}{
		{"nil policy is all defaults", nil, func(*dag.ResolvedRetryPolicy) {}},
		{"empty block is all defaults", &dag.RetryPolicy{}, func(*dag.ResolvedRetryPolicy) {}},
		{
			"partial: max_attempts only",
			&dag.RetryPolicy{MaxAttempts: 5},
			func(r *dag.ResolvedRetryPolicy) { r.MaxAttempts = 5 },
		},
		{
			"backoff block without multiplier keeps default multiplier",
			&dag.RetryPolicy{Backoff: &dag.BackoffSpec{Initial: "500ms", Cap: "10s"}},
			func(r *dag.ResolvedRetryPolicy) {
				r.Backoff.Initial = "500ms"
				r.Backoff.Cap = "10s"
			},
		},
		{
			"full explicit block overrides everything",
			&dag.RetryPolicy{
				MaxAttempts: 7,
				Backoff:     &dag.BackoffSpec{Initial: "2s", Cap: "5m", Multiplier: 3},
				Jitter:      dag.JitterNone,
				RetryOn:     []dag.ErrorClass{dag.ClassTransient},
			},
			func(r *dag.ResolvedRetryPolicy) {
				r.MaxAttempts = 7
				r.Backoff = dag.ResolvedBackoff{Initial: "2s", Cap: "5m", Multiplier: 3}
				r.Jitter = dag.JitterNone
				r.RetryOn = []dag.ErrorClass{dag.ClassTransient}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := defaultResolved()
			tc.mutate(&want)
			got := dag.ResolveRetryPolicy(tc.in)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ResolveRetryPolicy = %+v, want %+v", got, want)
			}
		})
	}
}

// TestResolveRetryPolicyOwnsRetryOn pins that the resolved policy does not
// alias the authored slice — materialization must not be mutable from the
// definition it came from.
func TestResolveRetryPolicyOwnsRetryOn(t *testing.T) {
	t.Parallel()

	authored := []dag.ErrorClass{dag.ClassTransient}
	r := dag.ResolveRetryPolicy(&dag.RetryPolicy{RetryOn: authored})
	authored[0] = dag.ClassPermanent
	if r.RetryOn[0] != dag.ClassTransient {
		t.Error("resolved RetryOn aliases the authored slice")
	}
}

// TestResolvedRetryPolicyRoundTrip pins the materialized JSON shape the
// engine decodes from run_steps.retry_policy.
func TestResolvedRetryPolicyRoundTrip(t *testing.T) {
	t.Parallel()

	want := defaultResolved()
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got dag.ResolvedRetryPolicy
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestResolvedRetryPolicyRetries(t *testing.T) {
	t.Parallel()

	r := defaultResolved()
	if !r.Retries(dag.ClassTransient) || !r.Retries(dag.ClassTimeout) {
		t.Error("default policy must retry transient and timeout")
	}
	if r.Retries(dag.ClassPermanent) || r.Retries(dag.ClassCancelled) || r.Retries(dag.ClassValidationFailed) {
		t.Error("default policy must not retry permanent/cancelled/validation_failed")
	}
}
