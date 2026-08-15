//go:build integration

package resource_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
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
