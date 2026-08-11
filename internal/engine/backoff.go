package engine

// Backoff computation (ticket 5.2, ADR-006 "Retry policy"): pure — the
// only nondeterminism is the injected jitter source, so fake-clock tests
// pin timings exactly with jitter "none" or a fixed rand.

import (
	"fmt"
	"math"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// retryDelay computes the delay before budget-eligible retry n (1-based:
// the delay after the n-th counted failure) under the given effective
// policy: min(cap, initial × multiplier^(n−1)), then jittered — full
// jitter draws uniformly from [0, computed] (AWS full jitter; jitterRand
// supplies the [0,1) draw), none returns the computed delay exactly.
//
// The policy's durations were validated parseable at submit time
// (authored) or are parseable by construction (defaults), so a parse
// error here means the materialized policy is corrupt — the caller treats
// it like any other corrupt stored content.
func retryDelay(policy dag.ResolvedRetryPolicy, n int, jitterRand func() float64) (time.Duration, error) {
	initial, err := time.ParseDuration(policy.Backoff.Initial)
	if err != nil {
		return 0, fmt.Errorf("engine: corrupt materialized backoff initial %q: %w", policy.Backoff.Initial, err)
	}
	limit, err := time.ParseDuration(policy.Backoff.Cap)
	if err != nil {
		return 0, fmt.Errorf("engine: corrupt materialized backoff cap %q: %w", policy.Backoff.Cap, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("engine: retry ordinal %d — must be ≥ 1", n)
	}
	computed := float64(initial) * math.Pow(policy.Backoff.Multiplier, float64(n-1))
	if !(computed < float64(limit)) { // NaN/+Inf and overflow land on the cap
		computed = float64(limit)
	}
	switch policy.Jitter {
	case dag.JitterNone:
		return time.Duration(computed), nil
	default: // full jitter, the engine default
		return time.Duration(jitterRand() * computed), nil
	}
}
