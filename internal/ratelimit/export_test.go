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
