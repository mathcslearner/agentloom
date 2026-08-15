//go:build integration

package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/ratelimit"
)

// TestAdjustRefundAddsTokens: a negative delta (over-estimate — the estimate
// exceeded the actual usage) refunds the difference to the bucket.
func TestAdjustRefundAddsTokens(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 1000, RefillPerSec: 0}

	// Debit the estimate (250), then reconcile with a refund of 100 (actual
	// was 150): delta = 150 − 250 = −100.
	if res, _, err := l.AcquireAt(ctx, b, 250, t0); err != nil || !res.Allowed || res.Remaining != 750 {
		t.Fatalf("priming acquire: %+v err=%v", res, err)
	}
	rem, tokens, err := l.AdjustAt(ctx, b, -100, t0)
	if err != nil {
		t.Fatalf("AdjustAt: %v", err)
	}
	if rem != 850 || tokens != 850 {
		t.Fatalf("after refund: remaining=%d tokens=%v, want 850", rem, tokens)
	}
}

// TestAdjustRefundClampsAtCapacity: a refund can never make a bucket fuller
// than full — the clamp mirrors the refill clamp.
func TestAdjustRefundClampsAtCapacity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 100, RefillPerSec: 0}

	// Debit 10, then refund 50 — only 10 was actually spent, so the bucket
	// clamps at capacity rather than overshooting to 140.
	if _, _, err := l.AcquireAt(ctx, b, 10, t0); err != nil {
		t.Fatalf("priming acquire: %v", err)
	}
	rem, tokens, err := l.AdjustAt(ctx, b, -50, t0)
	if err != nil {
		t.Fatalf("AdjustAt: %v", err)
	}
	if rem != 100 || tokens != 100 {
		t.Fatalf("after over-refund: remaining=%d tokens=%v, want capacity 100", rem, tokens)
	}
}

// TestAdjustDebitGoesNegativeAndThrottles is the core enforcement property
// (ADR-010, 9.3): a positive delta (under-estimate — actual exceeded the
// estimate) drives the balance negative, unclamped, and the debt makes
// subsequent acquires throttle with a correctly-grown retry_after until refill
// pays it back. This is what stops a biased-low estimator drifting the fleet
// past the provider's real budget.
func TestAdjustDebitGoesNegativeAndThrottles(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 10, RefillPerSec: 2}

	// Estimate 4 debited at t0, but the call actually used 10: delta = +6.
	if res, _, err := l.AcquireAt(ctx, b, 4, t0); err != nil || !res.Allowed || res.Remaining != 6 {
		t.Fatalf("priming acquire: %+v err=%v", res, err)
	}
	rem, tokens, err := l.AdjustAt(ctx, b, 6, t0)
	if err != nil {
		t.Fatalf("AdjustAt: %v", err)
	}
	// 6 − 6 = 0 balance; now push it into debt with another under-estimate.
	if rem != 0 || tokens != 0 {
		t.Fatalf("after first debit: remaining=%d tokens=%v, want 0", rem, tokens)
	}
	rem, tokens, err = l.AdjustAt(ctx, b, 6, t0)
	if err != nil {
		t.Fatalf("AdjustAt debt: %v", err)
	}
	if rem != -6 || tokens != -6 {
		t.Fatalf("after second debit: remaining=%d tokens=%v, want −6 (debt)", rem, tokens)
	}

	// The debt throttles: a cost-1 acquire is denied, and retry_after is the
	// time to refill from −6 to +1 — 7 tokens at 2/s = 3.5s.
	res, _, err := l.AcquireAt(ctx, b, 1, t0)
	if err != nil {
		t.Fatalf("AcquireAt in debt: %v", err)
	}
	if res.Allowed {
		t.Fatal("acquire granted while the bucket is in debt")
	}
	if res.RetryAfter != 3500*time.Millisecond {
		t.Fatalf("retry_after in debt = %v, want 3.5s", res.RetryAfter)
	}

	// After the debt is refilled away (3.5s later) the acquire succeeds.
	res, _, err = l.AcquireAt(ctx, b, 1, t0.Add(3500*time.Millisecond))
	if err != nil {
		t.Fatalf("AcquireAt after refill: %v", err)
	}
	if !res.Allowed || res.Remaining != 0 {
		t.Fatalf("after debt refilled: %+v, want allowed remaining=0", res)
	}
}

// TestAdjustRefillsBeforeApplying: the adjust refills the bucket to `now`
// first, exactly like an acquire, so a delta applied after an idle period
// operates on the refilled balance.
func TestAdjustRefillsBeforeApplying(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 100, RefillPerSec: 5}

	// Drain to 0 at t0, then adjust +10 (debit) at t0+4s: 20 tokens refilled
	// first (5/s × 4s), minus the 10 debit → 10.
	if _, _, err := l.AcquireAt(ctx, b, 100, t0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	rem, tokens, err := l.AdjustAt(ctx, b, 10, t0.Add(4*time.Second))
	if err != nil {
		t.Fatalf("AdjustAt: %v", err)
	}
	if rem != 10 || tokens != 10 {
		t.Fatalf("refill-then-debit: remaining=%d tokens=%v, want 10", rem, tokens)
	}
}

// TestAdjustZeroDeltaOnlyAdvancesClock: a zero delta is a valid no-op that
// only refills to `now` and rewrites state — the middleware skips these, but
// the library must not reject them.
func TestAdjustZeroDeltaOnlyAdvancesClock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 50, RefillPerSec: 0}

	if _, _, err := l.AcquireAt(ctx, b, 20, t0); err != nil {
		t.Fatalf("priming acquire: %v", err)
	}
	rem, tokens, err := l.AdjustAt(ctx, b, 0, t0)
	if err != nil {
		t.Fatalf("AdjustAt: %v", err)
	}
	if rem != 30 || tokens != 30 {
		t.Fatalf("zero-delta adjust changed the balance: remaining=%d tokens=%v, want 30", rem, tokens)
	}
}

// TestAdjustRateZeroPersists: a never-refilling bucket keeps its state without
// expiry after an adjust — expiring it would silently re-arm the quota.
func TestAdjustRateZeroPersists(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, client, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 100, RefillPerSec: 0}

	if _, _, err := l.AcquireAt(ctx, b, 50, t0); err != nil {
		t.Fatalf("priming acquire: %v", err)
	}
	if _, _, err := l.AdjustAt(ctx, b, 10, t0); err != nil {
		t.Fatalf("AdjustAt: %v", err)
	}
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl != -1 { // -1: key exists, no expiry
		t.Errorf("PTTL = %v, want -1 (no expiry on a never-refilling bucket)", ttl)
	}
}

// TestAdjustDebtTTLOutlivesTimeToFull: a bucket driven into debt keeps its
// state until it would have refilled back to full — an early expiry (absent
// key = full) would erase the debt, letting the fleet escape the correction.
func TestAdjustDebtTTLOutlivesTimeToFull(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, client, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 10, RefillPerSec: 1}

	// Drain to 0, then debit 5 into a −5 balance: time-to-full is now 15
	// tokens at 1/s = 15s, so the TTL must be at least 15s (plus the margin).
	if _, _, err := l.AcquireAt(ctx, b, 10, t0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, _, err := l.AdjustAt(ctx, b, 5, t0); err != nil {
		t.Fatalf("AdjustAt: %v", err)
	}
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl < 15*time.Second || ttl > 16*time.Second {
		t.Errorf("PTTL = %v, want within [15s, 16s] (debt must outlive time-to-full)", ttl)
	}
}

// TestAdjustValidatesBucket: Adjust rejects an ill-formed bucket, but — unlike
// Acquire — imposes no positive-cost constraint, since a reconciliation delta
// is signed.
func TestAdjustValidatesBucket(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)

	if _, err := l.Adjust(ctx, ratelimit.Bucket{Key: "", Capacity: 10, RefillPerSec: 1}, 5); err == nil {
		t.Error("Adjust accepted an empty key")
	}
	if _, err := l.Adjust(ctx, ratelimit.Bucket{Key: key, Capacity: 0, RefillPerSec: 1}, 5); err == nil {
		t.Error("Adjust accepted a non-positive capacity")
	}
}
