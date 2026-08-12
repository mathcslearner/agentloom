//go:build integration

package ratelimit_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/ratelimit"
)

const opTimeout = 30 * time.Second

// t0 is an arbitrary fixed instant for the injected-clock tests, matching
// the delayed-delivery suite's convention.
var t0 = time.UnixMilli(1_754_000_000_000).UTC()

// newTestLimiter connects to the test Redis (same address discipline as
// queuetest: AGENTLOOM_TEST_REDIS_ADDR, defaulting to the compose stack)
// and returns a limiter plus a uniquely prefixed bucket key deleted on
// cleanup — the same per-test isolation the queue harness uses.
func newTestLimiter(tb testing.TB) (*ratelimit.Limiter, *redis.Client, string) {
	tb.Helper()
	addr := os.Getenv(queuetest.EnvTestRedisAddr)
	if addr == "" {
		addr = "localhost:6379"
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	client, err := queue.Open(ctx, addr)
	if err != nil {
		tb.Fatalf("cannot reach Redis at %s (is the dev stack running? try `make up`): %v", addr, err)
	}
	tb.Cleanup(func() { client.Close() }) //nolint:errcheck // best-effort cleanup in tests

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		tb.Fatalf("generating key prefix: %v", err)
	}
	key := "agentloom-test:" + hex.EncodeToString(b[:]) + ":ratelimit"
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if err := client.Del(ctx, key).Err(); err != nil {
			tb.Errorf("deleting test key: %v", err)
		}
	})
	return ratelimit.New(client), client, key
}

// TestAcquireFreshBucket: an untouched bucket is full (absent key = full),
// a grant decrements, and the key's TTL is re-armed to time-to-full plus
// the safety margin — never less, or an early expiry would over-grant.
func TestAcquireFreshBucket(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, client, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 10, RefillPerSec: 1}

	res, err := l.Acquire(ctx, b, 3)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !res.Allowed || res.Remaining != 7 || res.RetryAfter != 0 {
		t.Errorf("got %+v, want allowed remaining=7 retry=0", res)
	}

	// Time-to-full for 3 missing tokens at 1/s is 3s; the script adds a 1s
	// margin. Expiry must never undercut time-to-full.
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl < 3*time.Second || ttl > 4*time.Second {
		t.Errorf("PTTL = %v, want within [3s, 4s]", ttl)
	}
}

// TestDrainAndDeny: with no refill, a bucket is a fixed quota — grants
// stop exactly at capacity, a denial reports RetryAfterNever, and the
// state never expires (expiring it would silently re-arm the quota).
func TestDrainAndDeny(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, client, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 5, RefillPerSec: 0}

	for i := range 5 {
		res, err := l.Acquire(ctx, b, 1)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if !res.Allowed || res.Remaining != int64(4-i) {
			t.Errorf("Acquire %d: got %+v, want allowed remaining=%d", i, res, 4-i)
		}
	}

	res, err := l.Acquire(ctx, b, 1)
	if err != nil {
		t.Fatalf("Acquire after drain: %v", err)
	}
	if res.Allowed || res.Remaining != 0 || res.RetryAfter != ratelimit.RetryAfterNever {
		t.Errorf("got %+v, want denied remaining=0 retry=never", res)
	}

	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl != -1 { // -1: key exists, no expiry
		t.Errorf("PTTL = %v, want -1 (no expiry on a never-refilling bucket)", ttl)
	}
}

// TestRetryAfterExact drives a drained bucket on the injected clock and
// pins exact refill and retry-after arithmetic at hand-computed points.
func TestRetryAfterExact(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 10, RefillPerSec: 2}

	// Drain the bucket at t0.
	res, _, err := l.AcquireAt(ctx, b, 10, t0)
	if err != nil {
		t.Fatalf("AcquireAt drain: %v", err)
	}
	if !res.Allowed || res.Remaining != 0 {
		t.Fatalf("drain: got %+v, want allowed remaining=0", res)
	}

	// Still at t0: 4 tokens missing at 2/s → wait exactly 2s.
	res, _, err = l.AcquireAt(ctx, b, 4, t0)
	if err != nil {
		t.Fatalf("AcquireAt at t0: %v", err)
	}
	if res.Allowed || res.RetryAfter != 2*time.Second {
		t.Errorf("at t0: got %+v, want denied retry=2s", res)
	}

	// One second later: 2 tokens refilled, still 2 short → wait 1s more.
	res, _, err = l.AcquireAt(ctx, b, 4, t0.Add(time.Second))
	if err != nil {
		t.Fatalf("AcquireAt at t0+1s: %v", err)
	}
	if res.Allowed || res.RetryAfter != time.Second {
		t.Errorf("at t0+1s: got %+v, want denied retry=1s", res)
	}

	// At the promised instant the acquire succeeds with nothing to spare —
	// and the two denials above provably left the balance untouched.
	res, _, err = l.AcquireAt(ctx, b, 4, t0.Add(2*time.Second))
	if err != nil {
		t.Fatalf("AcquireAt at t0+2s: %v", err)
	}
	if !res.Allowed || res.Remaining != 0 {
		t.Errorf("at t0+2s: got %+v, want allowed remaining=0", res)
	}
}

// TestRefillCapsAtCapacity: a long-idle bucket refills to capacity, never
// beyond.
func TestRefillCapsAtCapacity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 10, RefillPerSec: 5}

	if _, _, err := l.AcquireAt(ctx, b, 10, t0); err != nil {
		t.Fatalf("AcquireAt drain: %v", err)
	}
	res, tokens, err := l.AcquireAt(ctx, b, 1, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("AcquireAt after idle: %v", err)
	}
	if !res.Allowed || res.Remaining != 9 || tokens != 9 {
		t.Errorf("got %+v tokens=%v, want allowed remaining=9 tokens=9", res, tokens)
	}
}

// TestBackwardTimeClamped: an acquire whose now is before the stored
// refill timestamp must not mint tokens (elapsed clamps to zero) — Redis
// TIME can step backwards across a failover.
func TestBackwardTimeClamped(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)
	b := ratelimit.Bucket{Key: key, Capacity: 10, RefillPerSec: 1000}

	if _, _, err := l.AcquireAt(ctx, b, 10, t0); err != nil {
		t.Fatalf("AcquireAt drain: %v", err)
	}
	res, tokens, err := l.AcquireAt(ctx, b, 1, t0.Add(-10*time.Second))
	if err != nil {
		t.Fatalf("AcquireAt backwards: %v", err)
	}
	if res.Allowed || tokens != 0 {
		t.Errorf("got %+v tokens=%v, want denied tokens=0 (no refill from a backwards clock)", res, tokens)
	}
}

// TestCapacityShrinkClamps: config lives with the caller, so a shrunk
// capacity must clamp an older, larger stored balance on the next acquire.
func TestCapacityShrinkClamps(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)

	big := ratelimit.Bucket{Key: key, Capacity: 10, RefillPerSec: 0}
	if _, _, err := l.AcquireAt(ctx, big, 1, t0); err != nil { // stored balance: 9
		t.Fatalf("AcquireAt: %v", err)
	}

	small := ratelimit.Bucket{Key: key, Capacity: 5, RefillPerSec: 0}
	res, _, err := l.AcquireAt(ctx, small, 1, t0)
	if err != nil {
		t.Fatalf("AcquireAt shrunk: %v", err)
	}
	if !res.Allowed || res.Remaining != 4 {
		t.Errorf("got %+v, want allowed remaining=4 (9 clamped to 5, minus 1)", res)
	}
}

// TestStressNoOverGrant is acceptance criterion 1: N concurrent clients
// hammering one fixed-quota bucket are granted exactly capacity tokens —
// strict accounting, no over-grant, under -race. Refill is zero so time
// cannot blur the ledger.
func TestStressNoOverGrant(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, _, key := newTestLimiter(t)

	const (
		capacity   = 500
		goroutines = 32
		perClient  = 100 // 3200 attempts against 500 tokens
	)
	b := ratelimit.Bucket{Key: key, Capacity: capacity, RefillPerSec: 0}

	var granted, denied atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perClient {
				res, err := l.Acquire(ctx, b, 1)
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				if res.Allowed {
					granted.Add(1)
				} else {
					denied.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if granted.Load() != capacity {
		t.Errorf("granted %d tokens, want exactly %d", granted.Load(), capacity)
	}
	if want := int64(goroutines*perClient - capacity); denied.Load() != want {
		t.Errorf("denied %d acquires, want exactly %d", denied.Load(), want)
	}

	res, err := l.Acquire(ctx, b, 1)
	if err != nil {
		t.Fatalf("probe Acquire: %v", err)
	}
	if res.Allowed || res.Remaining != 0 {
		t.Errorf("probe after drain: got %+v, want denied remaining=0", res)
	}
}

// TestStressVariableCost is the variable-cost half of criterion 1: with
// racing acquires of mixed sizes, the sum of granted costs never exceeds
// capacity and the stored balance is exactly capacity minus that sum.
func TestStressVariableCost(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	l, client, key := newTestLimiter(t)

	const (
		capacity   = 1000
		goroutines = 16
		perClient  = 50
	)
	b := ratelimit.Bucket{Key: key, Capacity: capacity, RefillPerSec: 0}

	var granted atomic.Int64
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perClient {
				cost := int64(1 + (g*7+i*13)%20) // deterministic spread 1..20
				res, err := l.Acquire(ctx, b, cost)
				if err != nil {
					t.Errorf("Acquire cost %d: %v", cost, err)
					return
				}
				if res.Allowed {
					granted.Add(cost)
				}
			}
		}()
	}
	wg.Wait()

	if granted.Load() > capacity {
		t.Errorf("granted %d tokens total, over capacity %d", granted.Load(), capacity)
	}

	stored, err := client.HGet(ctx, key, "tokens").Result()
	if err != nil {
		t.Fatalf("HGET tokens: %v", err)
	}
	balance, err := strconv.ParseFloat(stored, 64)
	if err != nil {
		t.Fatalf("parsing stored balance %q: %v", stored, err)
	}
	if want := float64(capacity - granted.Load()); math.Abs(balance-want) > 1e-9 {
		t.Errorf("stored balance %v, want %v (capacity %d − granted %d)", balance, want, capacity, granted.Load())
	}
}
