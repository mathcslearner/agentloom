package ratelimit

import (
	"context"
	"time"
)

// AcquireAt exposes the injected-clock acquire to tests: the refill-math
// property test and the exact retry-after tests drive the real Lua script
// with fully synthetic time, and read back the exact fractional balance to
// compare against their model. Production code has no path to this —
// Acquire always uses Redis TIME.
func (l *Limiter) AcquireAt(ctx context.Context, b Bucket, cost int64, now time.Time) (Result, float64, error) {
	return l.acquireAt(ctx, b, cost, now)
}

// ParseAcquireReply exposes the script-reply decoder for unit tests.
var ParseAcquireReply = parseAcquireReply

// AdjustAt exposes the injected-clock reconciliation adjust to tests: the
// conservation property test drives the real Lua script with synthetic time,
// interleaving acquires and adjusts, and reads back the exact fractional
// balance to compare against its model. Production code has no path to this —
// Adjust always uses Redis TIME.
func (l *Limiter) AdjustAt(ctx context.Context, b Bucket, delta int64, now time.Time) (int64, float64, error) {
	return l.adjustAt(ctx, b, delta, now)
}

// AcquireDualAt exposes the injected-clock two-key acquire to tests: the
// dual-accounting property and retry-after tests drive the real Lua script
// with synthetic time and read back both exact fractional balances.
// Production code has no path to this — AcquireDual always uses Redis TIME.
func (l *Limiter) AcquireDualAt(ctx context.Context, requests Bucket, reqCost int64, tokens Bucket, tokCost int64, now time.Time) (DualResult, float64, float64, error) {
	return l.acquireDualAt(ctx, requests, reqCost, tokens, tokCost, now)
}
