//go:build integration

package ratelimit_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/mathcslearner/agentloom/internal/ratelimit"
)

// TestAcquireRefillMathProp is acceptance criterion 2: the refill math,
// property-tested with fake time against the real Lua script. A pure-Go
// model mirrors the script's float64 operations in the same order — Lua
// numbers are IEEE-754 doubles too, and the %.17g state serialization
// round-trips exactly — so every field is compared for exact equality: a
// single ULP of drift or a formatting truncation fails the property.
//
// The generator covers the interesting boundaries deliberately: zero-rate
// buckets (fixed quotas), backwards time steps (the elapsed clamp), zero
// deltas, costs up to exactly capacity, and long idles that hit the
// capacity cap.
func TestAcquireRefillMathProp(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, client, keyBase := newTestLimiter(t)

	iter := 0
	rapid.Check(t, func(rt *rapid.T) {
		iter++
		key := fmt.Sprintf("%s:prop:%d", keyBase, iter)
		defer func() {
			if err := client.Del(ctx, key).Err(); err != nil {
				rt.Errorf("deleting prop key: %v", err)
			}
		}()

		capacity := rapid.Int64Range(1, 1_000_000).Draw(rt, "capacity")
		rate := rapid.OneOf(
			rapid.Just(0.0),
			rapid.Float64Range(0.001, 1e6),
		).Draw(rt, "rate")
		b := ratelimit.Bucket{Key: key, Capacity: capacity, RefillPerSec: rate}

		// The model mirrors the script exactly: absent state means a full
		// bucket with no refill on first touch; afterwards, refill by
		// elapsed µs clamped at zero, capped at capacity.
		var (
			tokens      float64
			tsMicro     int64
			initialized bool
		)
		now := t0

		ops := rapid.IntRange(1, 25).Draw(rt, "ops")
		for i := 0; i < ops; i++ {
			label := fmt.Sprintf("op%d_", i)
			deltaMicro := rapid.OneOf(
				rapid.Just(int64(0)),
				rapid.Int64Range(-5_000_000, 5_000_000),
				rapid.Int64Range(0, 120_000_000),
			).Draw(rt, label+"delta_us")
			now = now.Add(time.Duration(deltaMicro) * time.Microsecond)
			cost := rapid.Int64Range(1, capacity).Draw(rt, label+"cost")

			if !initialized {
				tokens = float64(capacity)
				initialized = true
			} else {
				elapsed := now.UnixMicro() - tsMicro
				if elapsed < 0 {
					elapsed = 0
				}
				tokens = tokens + float64(elapsed)*rate/1e6
				if tokens > float64(capacity) {
					tokens = float64(capacity)
				}
			}
			tsMicro = now.UnixMicro()

			wantAllowed := float64(cost) <= tokens
			wantRetry := ratelimit.RetryAfterNever
			if wantAllowed {
				tokens -= float64(cost)
				wantRetry = 0
			} else if rate > 0 {
				wantRetry = time.Duration(math.Ceil((float64(cost)-tokens)/rate*1e6)) * time.Microsecond
			}

			res, gotTokens, err := l.AcquireAt(ctx, b, cost, now)
			if err != nil {
				rt.Fatalf("op %d: AcquireAt(cost=%d, now=%v): %v", i, cost, now, err)
			}
			if res.Allowed != wantAllowed {
				rt.Fatalf("op %d: Allowed = %v, model says %v (cost=%d model-tokens=%v)",
					i, res.Allowed, wantAllowed, cost, tokens)
			}
			if gotTokens != tokens {
				rt.Fatalf("op %d: tokens = %v, model says %v (diff %g)",
					i, gotTokens, tokens, gotTokens-tokens)
			}
			if want := int64(math.Floor(tokens)); res.Remaining != want {
				rt.Fatalf("op %d: Remaining = %d, model says %d", i, res.Remaining, want)
			}
			if res.RetryAfter != wantRetry {
				rt.Fatalf("op %d: RetryAfter = %v, model says %v", i, res.RetryAfter, wantRetry)
			}
		}
	})
}
