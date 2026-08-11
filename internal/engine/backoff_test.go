package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// policyWith builds a resolved policy with the given backoff shape and
// jitter mode; max_attempts/retry_on are irrelevant to retryDelay.
func policyWith(initial, limit string, multiplier float64, jitter dag.JitterMode) dag.ResolvedRetryPolicy {
	return dag.ResolvedRetryPolicy{
		MaxAttempts: 3,
		Backoff:     dag.ResolvedBackoff{Initial: initial, Cap: limit, Multiplier: multiplier},
		Jitter:      jitter,
		RetryOn:     dag.DefaultRetryOn(),
	}
}

func TestRetryDelayExponential(t *testing.T) {
	t.Parallel()

	p := policyWith("1s", "1m", 2, dag.JitterNone)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for n, w := range want {
		got, err := retryDelay(p, n+1, nil)
		if err != nil {
			t.Fatalf("retryDelay(n=%d): %v", n+1, err)
		}
		if got != w {
			t.Errorf("retryDelay(n=%d) = %v, want %v", n+1, got, w)
		}
	}
}

func TestRetryDelayCapClamp(t *testing.T) {
	t.Parallel()

	p := policyWith("1s", "5s", 2, dag.JitterNone)
	// n=4 computes 8s, above the 5s cap.
	got, err := retryDelay(p, 4, nil)
	if err != nil {
		t.Fatalf("retryDelay: %v", err)
	}
	if got != 5*time.Second {
		t.Errorf("retryDelay above cap = %v, want 5s", got)
	}
	// A huge ordinal must not overflow into a negative or tiny delay.
	got, err = retryDelay(p, 5000, nil)
	if err != nil {
		t.Fatalf("retryDelay: %v", err)
	}
	if got != 5*time.Second {
		t.Errorf("retryDelay at overflow-scale ordinal = %v, want 5s", got)
	}
}

func TestRetryDelayConstantBackoff(t *testing.T) {
	t.Parallel()

	p := policyWith("3s", "1m", 1, dag.JitterNone)
	for _, n := range []int{1, 2, 7} {
		got, err := retryDelay(p, n, nil)
		if err != nil {
			t.Fatalf("retryDelay(n=%d): %v", n, err)
		}
		if got != 3*time.Second {
			t.Errorf("retryDelay(multiplier=1, n=%d) = %v, want 3s", n, got)
		}
	}
}

func TestRetryDelayFullJitter(t *testing.T) {
	t.Parallel()

	p := policyWith("4s", "1m", 2, dag.JitterFull)
	// Pinned draws: full jitter is draw × computed.
	for _, tc := range []struct {
		draw float64
		n    int
		want time.Duration
	}{
		{0, 1, 0},
		{0.5, 1, 2 * time.Second},
		{0.5, 2, 4 * time.Second},
		{0.999999, 1, time.Duration(0.999999 * float64(4*time.Second))},
	} {
		got, err := retryDelay(p, tc.n, func() float64 { return tc.draw })
		if err != nil {
			t.Fatalf("retryDelay: %v", err)
		}
		if got != tc.want {
			t.Errorf("retryDelay(draw=%v, n=%d) = %v, want %v", tc.draw, tc.n, got, tc.want)
		}
	}
}

func TestRetryDelayCorruptPolicy(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]dag.ResolvedRetryPolicy{
		"bad initial": policyWith("not-a-duration", "1m", 2, dag.JitterNone),
		"bad cap":     policyWith("1s", "??", 2, dag.JitterNone),
	} {
		if _, err := retryDelay(p, 1, nil); err == nil || !strings.Contains(err.Error(), "corrupt materialized backoff") {
			t.Errorf("%s: retryDelay error = %v, want corrupt-backoff error", name, err)
		}
	}
	if _, err := retryDelay(policyWith("1s", "1m", 2, dag.JitterNone), 0, nil); err == nil {
		t.Error("retryDelay(n=0): expected error, got nil")
	}
}
