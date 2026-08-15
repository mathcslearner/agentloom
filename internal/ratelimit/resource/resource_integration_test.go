//go:build integration

package resource_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/limits"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
)

const opTimeout = 30 * time.Second

// newTestClient connects to the test Redis and returns a client plus a
// unique key prefix flushed on cleanup (the whole namespace, so every bucket
// key under it is removed).
func newTestClient(tb testing.TB) (*redis.Client, string) {
	tb.Helper()
	addr := os.Getenv(queuetest.EnvTestRedisAddr)
	if addr == "" {
		addr = "localhost:6379"
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, err := queue.Open(ctx, addr)
	if err != nil {
		tb.Fatalf("cannot reach Redis at %s (try `make up`): %v", addr, err)
	}
	tb.Cleanup(func() { client.Close() }) //nolint:errcheck // best-effort
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		tb.Fatalf("prefix: %v", err)
	}
	prefix := "agentloom-test:" + hex.EncodeToString(b[:])
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		keys, _ := client.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			client.Del(ctx, keys...) //nolint:errcheck // best-effort
		}
	})
	return client, prefix
}

func mustSet(tb testing.TB, doc string) *limits.Set {
	tb.Helper()
	set, err := limits.Parse([]byte(doc))
	if err != nil {
		tb.Fatalf("limits.Parse: %v", err)
	}
	return set
}

// TestAcquireUnlimitedResourceSkipsRedis: an unconfigured resource is
// unlimited (ADR-010) and touches no Redis.
func TestAcquireUnlimitedResourceSkipsRedis(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	set := mustSet(t, `{"resources":[{"name":"anthropic:claude-sonnet-5","requests":{"per_minute":60}}]}`)
	lim, err := resource.New(client, set, prefix)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dec, err := lim.Acquire(ctx, "mock:sim-1", 100)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if dec.Limited || !dec.Allowed {
		t.Fatalf("unknown resource: limited=%v allowed=%v (want unlimited/allowed)", dec.Limited, dec.Allowed)
	}
	// No bucket key should have been written.
	if keys, _ := client.Keys(ctx, prefix+"*").Result(); len(keys) != 0 {
		t.Fatalf("unlimited resource wrote Redis keys: %v", keys)
	}
}

// TestAcquireExactAndWildcard resolves exact-first then the provider
// wildcard, and both meter the same request bucket.
func TestAcquireExactAndWildcard(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	// openai:* wildcard with a burst of 1 so the second call denies.
	set := mustSet(t, `{"resources":[{"name":"openai:*","requests":{"per_minute":60,"burst":1}}]}`)
	lim, _ := resource.New(client, set, prefix)

	dec, err := lim.Acquire(ctx, "openai:gpt-4o", 0)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !dec.Limited || !dec.Allowed || dec.Resource != "openai:*" {
		t.Fatalf("first: %+v (want limited/allowed/openai:*)", dec)
	}
	// Second call on the same wildcard-resolved bucket denies.
	dec, err = lim.Acquire(ctx, "openai:gpt-4o", 0)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if dec.Allowed || dec.Bucket != "requests" {
		t.Fatalf("second: %+v (want denied on requests)", dec)
	}
}

// TestAcquireDualDimension: a resource with both dimensions and a positive
// estimate acquires both buckets; exhausting tokens denies on the token
// bucket while requests still has room.
func TestAcquireDualDimension(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	set := mustSet(t, `{"resources":[{"name":"anthropic:claude-sonnet-5","requests":{"per_minute":6000,"burst":100},"tokens":{"per_minute":600,"burst":100}}]}`)
	lim, _ := resource.New(client, set, prefix)

	// Spend 90 tokens: allowed.
	if dec, err := lim.Acquire(ctx, "anthropic:claude-sonnet-5", 90); err != nil || !dec.Allowed {
		t.Fatalf("first: allowed=%v err=%v", dec.Allowed, err)
	}
	// Ask for 90 more: tokens has only ~10 left, requests plenty → deny tokens.
	dec, err := lim.Acquire(ctx, "anthropic:claude-sonnet-5", 90)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if dec.Allowed || dec.Bucket != "tokens" {
		t.Fatalf("second: %+v (want denied on tokens)", dec)
	}
	if dec.RetryAfter <= 0 {
		t.Fatalf("expected a positive retry_after, got %v", dec.RetryAfter)
	}
}

// TestAcquireTokensOnlyWithZeroEstimateIsUnlimited: a tokens-only resource
// with a zero-token claim (a tool claim shape) meters nothing.
func TestAcquireTokensOnlyWithZeroEstimateIsUnlimited(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	set := mustSet(t, `{"resources":[{"name":"anthropic:claude-sonnet-5","tokens":{"per_minute":600}}]}`)
	lim, _ := resource.New(client, set, prefix)

	dec, err := lim.Acquire(ctx, "anthropic:claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if dec.Limited || !dec.Allowed {
		t.Fatalf("tokens-only zero-estimate: %+v (want unlimited)", dec)
	}
}

// TestAcquireToolRequestsOnly: a tool resource (estTokens 0) governs only
// its requests bucket.
func TestAcquireToolRequestsOnly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	set := mustSet(t, `{"resources":[{"name":"tool:http_request","requests":{"per_minute":60,"burst":1}}]}`)
	lim, _ := resource.New(client, set, prefix)

	if dec, err := lim.Acquire(ctx, "tool:http_request", 0); err != nil || !dec.Allowed {
		t.Fatalf("first: allowed=%v err=%v", dec.Allowed, err)
	}
	dec, err := lim.Acquire(ctx, "tool:http_request", 0)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if dec.Allowed || dec.Bucket != "requests" {
		t.Fatalf("second: %+v (want denied on requests)", dec)
	}
}

// TestReconcileDebitsTokenBucketOnly is 9.3's core: after a metered acquire,
// Reconcile corrects the token bucket by actual − estimate, and touches only
// the token dimension — the requests bucket's cost of 1 is exact. An
// under-estimate (positive delta) debits the shortfall; the requests balance
// is provably unchanged.
func TestReconcileDebitsTokenBucketOnly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	// No refill (per-minute rate is required, but a large burst and a short
	// test window make refill negligible; assert with a margin below).
	set := mustSet(t, `{"resources":[{"name":"anthropic:claude-sonnet-5","requests":{"per_minute":6000,"burst":100},"tokens":{"per_minute":60,"burst":1000}}]}`)
	lim, _ := resource.New(client, set, prefix)

	// Estimate 100 debited: tokens ~900 left, requests 99 left.
	if dec, err := lim.Acquire(ctx, "anthropic:claude-sonnet-5", 100); err != nil || !dec.Allowed || !dec.TokensMetered {
		t.Fatalf("acquire: %+v err=%v (want allowed, tokens metered)", dec, err)
	}
	// The call actually used 250 tokens: reconcile the +150 shortfall.
	if err := lim.Reconcile(ctx, "anthropic:claude-sonnet-5", 100, 250); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The requests bucket is untouched by reconciliation: two spent of 100,
	// so a burst-100 requests bucket still has ~98 — a 90-cost token acquire
	// must be denied on TOKENS (≈750 estimate-corrected balance can't take a
	// 900 cost), proving the debit landed on tokens.
	//
	// Read the token balance directly to pin the correction exactly.
	tokKey := prefix + ":anthropic:claude-sonnet-5:tokens"
	bal, err := client.HGet(ctx, tokKey, "tokens").Result()
	if err != nil {
		t.Fatalf("HGet token balance: %v", err)
	}
	// Started at 1000, −100 (estimate) −150 (reconcile) = 750, plus a tiny
	// refill over the test's wall-clock. Assert within a small margin.
	got := parseFloat(t, bal)
	if got < 750 || got > 752 {
		t.Fatalf("token balance after reconcile = %v, want ≈750 (1000 − 100 − 150)", got)
	}

	// The requests bucket balance is exactly what the one acquire left it.
	reqKey := prefix + ":anthropic:claude-sonnet-5:requests"
	rbal, err := client.HGet(ctx, reqKey, "tokens").Result()
	if err != nil {
		t.Fatalf("HGet request balance: %v", err)
	}
	if r := parseFloat(t, rbal); r < 99 || r > 100 {
		t.Fatalf("request balance moved during token reconcile = %v, want ≈99", r)
	}
}

// TestReconcileRefundReturnsTokens: an over-estimate (negative delta) refunds
// the difference so a later call that would have throttled can proceed.
func TestReconcileRefundReturnsTokens(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	set := mustSet(t, `{"resources":[{"name":"mock:sim-1","tokens":{"per_minute":60,"burst":100}}]}`)
	lim, _ := resource.New(client, set, prefix)

	// Estimate 90 debited: ~10 tokens left. A second 90-token call would deny.
	if dec, err := lim.Acquire(ctx, "mock:sim-1", 90); err != nil || !dec.Allowed {
		t.Fatalf("acquire: %+v err=%v", dec, err)
	}
	// The call actually used 20: refund 70 (delta = 20 − 90 = −70) → ~80 left.
	if err := lim.Reconcile(ctx, "mock:sim-1", 90, 20); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Now a 70-token call fits where it would not have before the refund.
	dec, err := lim.Acquire(ctx, "mock:sim-1", 70)
	if err != nil {
		t.Fatalf("Acquire after refund: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("call denied after refund: %+v (refund did not return tokens)", dec)
	}
}

// TestReconcileWildcardResolves: reconciliation resolves the same wildcard the
// acquire did, correcting the shared bucket.
func TestReconcileWildcardResolves(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	set := mustSet(t, `{"resources":[{"name":"openai:*","tokens":{"per_minute":60,"burst":1000}}]}`)
	lim, _ := resource.New(client, set, prefix)

	if dec, err := lim.Acquire(ctx, "openai:gpt-4o", 100); err != nil || !dec.Allowed || dec.Resource != "openai:*" {
		t.Fatalf("acquire: %+v err=%v", dec, err)
	}
	if err := lim.Reconcile(ctx, "openai:gpt-4o", 100, 300); err != nil {
		t.Fatalf("Reconcile via wildcard: %v", err)
	}
	// The bucket is keyed by the wildcard resource name.
	tokKey := prefix + ":openai:*:tokens"
	bal, err := client.HGet(ctx, tokKey, "tokens").Result()
	if err != nil {
		t.Fatalf("HGet: %v", err)
	}
	if got := parseFloat(t, bal); got < 700 || got > 702 {
		t.Fatalf("wildcard token balance = %v, want ≈700 (1000 − 100 estimate − 200 reconcile)", got)
	}
}

// parseFloat is a tiny helper for reading %.17g bucket balances back.
func parseFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parse balance %q: %v", s, err)
	}
	return f
}

// TestAcquireCostExceedsCapacity surfaces the typed error the middleware
// perm-fails on.
func TestAcquireCostExceedsCapacity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, prefix := newTestClient(t)
	set := mustSet(t, `{"resources":[{"name":"anthropic:claude-sonnet-5","tokens":{"per_minute":600,"burst":100}}]}`)
	lim, _ := resource.New(client, set, prefix)

	_, err := lim.Acquire(ctx, "anthropic:claude-sonnet-5", 101)
	if err == nil || !errors.Is(err, resource.ErrCostExceedsCapacity) {
		t.Fatalf("want ErrCostExceedsCapacity, got %v", err)
	}
}
