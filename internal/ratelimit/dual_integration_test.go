//go:build integration

package ratelimit_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/ratelimit"
)

// dualKeys returns two uniquely prefixed bucket keys (requests, tokens) off
// the same per-test isolation prefix, both deleted on cleanup.
func dualKeys(tb testing.TB) (*ratelimit.Limiter, string, string) {
	tb.Helper()
	l, _, prefix := newTestLimiter(tb)
	return l, prefix + ":requests", prefix + ":tokens"
}

// TestAcquireDualBothGrant: when both buckets have room the acquire grants
// and debits both, exactly.
func TestAcquireDualBothGrant(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, reqKey, tokKey := dualKeys(t)
	req := ratelimit.Bucket{Key: reqKey, Capacity: 10, RefillPerSec: 0}
	tok := ratelimit.Bucket{Key: tokKey, Capacity: 1000, RefillPerSec: 0}

	res, tokReq, tokTok, err := l.AcquireDualAt(ctx, req, 1, tok, 250, t0)
	if err != nil {
		t.Fatalf("AcquireDual: %v", err)
	}
	if !res.Allowed || res.Denied != ratelimit.DeniedNone {
		t.Fatalf("want allowed/none, got allowed=%v denied=%v", res.Allowed, res.Denied)
	}
	if res.RemainingRequests != 9 || res.RemainingTokens != 750 {
		t.Fatalf("balances: req=%d tok=%d (want 9, 750)", res.RemainingRequests, res.RemainingTokens)
	}
	if tokReq != 9 || tokTok != 750 {
		t.Fatalf("exact balances: req=%v tok=%v (want 9, 750)", tokReq, tokTok)
	}
}

// TestAcquireDualTokenDenialLeavesRequestLedgerUntouched is the drift
// property the two-key script exists for (ADR-010): a denial on the token
// dimension must NOT debit the request bucket, or a steady-state
// token-limited workload would leak a request token on every denial and the
// two ledgers would diverge.
func TestAcquireDualTokenDenialLeavesRequestLedgerUntouched(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, reqKey, tokKey := dualKeys(t)
	req := ratelimit.Bucket{Key: reqKey, Capacity: 100, RefillPerSec: 0}
	tok := ratelimit.Bucket{Key: tokKey, Capacity: 5, RefillPerSec: 0}

	// Drain the token bucket to zero (request goes to 99).
	if res, _, _, err := l.AcquireDualAt(ctx, req, 1, tok, 5, t0); err != nil || !res.Allowed {
		t.Fatalf("priming acquire: allowed=%v err=%v", res.Allowed, err)
	}
	// Now the tokens bucket cannot admit cost 1, but requests can.
	res, tokReq, tokTok, err := l.AcquireDualAt(ctx, req, 1, tok, 1, t0)
	if err != nil {
		t.Fatalf("AcquireDual: %v", err)
	}
	if res.Allowed {
		t.Fatal("expected denial (token bucket drained)")
	}
	if res.Denied != ratelimit.DeniedTokens {
		t.Fatalf("denied=%v, want tokens", res.Denied)
	}
	if res.RemainingRequests != 99 {
		t.Fatalf("request ledger debited on a token denial: remaining=%d, want 99", res.RemainingRequests)
	}
	if tokReq != 99 || tokTok != 0 {
		t.Fatalf("exact balances req=%v tok=%v (want 99, 0)", tokReq, tokTok)
	}
}

// TestAcquireDualRetryAfterMax: when both dimensions deny, retry_after is the
// max of the two refill deadlines — the earliest moment BOTH can admit.
func TestAcquireDualRetryAfterMax(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, reqKey, tokKey := dualKeys(t)
	// requests refill 1/s (slow), tokens refill 2/s (fast).
	req := ratelimit.Bucket{Key: reqKey, Capacity: 10, RefillPerSec: 1}
	tok := ratelimit.Bucket{Key: tokKey, Capacity: 10, RefillPerSec: 2}

	// Drain both.
	if res, _, _, err := l.AcquireDualAt(ctx, req, 10, tok, 10, t0); err != nil || !res.Allowed {
		t.Fatalf("priming: allowed=%v err=%v", res.Allowed, err)
	}
	// Ask for 5 of each at the same instant: requests need 5s, tokens 2.5s.
	res, _, _, err := l.AcquireDualAt(ctx, req, 5, tok, 5, t0)
	if err != nil {
		t.Fatalf("AcquireDual: %v", err)
	}
	if res.Allowed || res.Denied != ratelimit.DeniedBoth {
		t.Fatalf("want both-denied, got allowed=%v denied=%v", res.Allowed, res.Denied)
	}
	if res.RetryAfter != 5*time.Second {
		t.Fatalf("retry_after=%v, want 5s (max of 5s requests / 2.5s tokens)", res.RetryAfter)
	}
}

// TestAcquireDualNeverRefill: a denial on a rate-zero bucket reports
// RetryAfterNever — the middleware perm-fails these (ADR-010 wait-vs-never).
func TestAcquireDualNeverRefill(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, reqKey, tokKey := dualKeys(t)
	req := ratelimit.Bucket{Key: reqKey, Capacity: 1, RefillPerSec: 0}
	tok := ratelimit.Bucket{Key: tokKey, Capacity: 1000, RefillPerSec: 1000}

	if res, _, _, err := l.AcquireDualAt(ctx, req, 1, tok, 1, t0); err != nil || !res.Allowed {
		t.Fatalf("priming: allowed=%v err=%v", res.Allowed, err)
	}
	res, _, _, err := l.AcquireDualAt(ctx, req, 1, tok, 1, t0)
	if err != nil {
		t.Fatalf("AcquireDual: %v", err)
	}
	if res.Allowed || res.Denied != ratelimit.DeniedRequests {
		t.Fatalf("want requests-denied, got allowed=%v denied=%v", res.Allowed, res.Denied)
	}
	if res.RetryAfter != ratelimit.RetryAfterNever {
		t.Fatalf("retry_after=%v, want RetryAfterNever", res.RetryAfter)
	}
}

// TestAcquireDualCostExceedsCapacity: an estimate larger than either bucket
// is a typed error before Redis, not a denial (ADR-010 perm-fails these).
func TestAcquireDualCostExceedsCapacity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, reqKey, tokKey := dualKeys(t)
	req := ratelimit.Bucket{Key: reqKey, Capacity: 10, RefillPerSec: 1}
	tok := ratelimit.Bucket{Key: tokKey, Capacity: 100, RefillPerSec: 1}

	_, _, _, err := l.AcquireDualAt(ctx, req, 1, tok, 101, t0)
	if err == nil || !errors.Is(err, ratelimit.ErrCostExceedsCapacity) {
		t.Fatalf("want ErrCostExceedsCapacity for oversized token cost, got %v", err)
	}
}

// TestAcquireDualAllOrNothing stresses the atomicity under concurrency: with
// the token bucket the tighter dimension, exactly its capacity of acquires
// grant, and the request bucket is debited by exactly that many — never more
// (no leaked request tokens on the denials).
func TestAcquireDualAllOrNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, reqKey, tokKey := dualKeys(t)
	const capacity = 50
	const goroutines = 200
	// rate 0 so Redis TIME never refills — the outcome is deterministic.
	req := ratelimit.Bucket{Key: reqKey, Capacity: 1000, RefillPerSec: 0}
	tok := ratelimit.Bucket{Key: tokKey, Capacity: capacity, RefillPerSec: 0}

	var grants atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := l.AcquireDual(ctx, req, 1, tok, 1)
			if err != nil {
				t.Errorf("AcquireDual: %v", err)
				return
			}
			if res.Allowed {
				grants.Add(1)
			}
		}()
	}
	wg.Wait()
	if grants.Load() != capacity {
		t.Fatalf("grants=%d, want exactly %d (token capacity)", grants.Load(), capacity)
	}
	// The request bucket must be debited by exactly the grant count — proof
	// the denials debited neither bucket.
	final, err := l.AcquireDual(ctx, req, 1, ratelimit.Bucket{Key: tokKey, Capacity: capacity, RefillPerSec: 1e9}, 1)
	if err != nil {
		t.Fatalf("final probe: %v", err)
	}
	// After `capacity` grants the request bucket holds 1000-capacity; the
	// probe (token bucket force-refilled) grants and debits one more.
	if want := int64(1000 - capacity - 1); final.RemainingRequests != want {
		t.Fatalf("request remaining=%d, want %d (no leaked tokens on denials)", final.RemainingRequests, want)
	}
}
