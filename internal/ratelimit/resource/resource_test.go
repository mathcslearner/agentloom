package resource_test

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/limits"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
)

// TestNewRequiresClient rejects a nil Redis client.
func TestNewRequiresClient(t *testing.T) {
	t.Parallel()
	if _, err := resource.New(nil, nil, ""); err == nil {
		t.Fatal("New(nil client): want error, got nil")
	}
}

// TestAcquireUnlimitedSkipsRedis proves the unknown-resource policy without a
// server: the client is pointed at an unroutable address that would error on
// any command, so a successful unlimited Acquire proves no Redis call was
// made.
func TestAcquireUnlimitedSkipsRedis(t *testing.T) {
	t.Parallel()
	// Lazy client: no connection until a command runs. Any command would
	// fail (bogus address, tiny timeout), so reaching Redis is observable as
	// an error.
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 1})
	t.Cleanup(func() { client.Close() }) //nolint:errcheck // best-effort

	set, err := limits.Parse([]byte(`{"resources":[{"name":"anthropic:claude-sonnet-5","requests":{"per_minute":60}}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	lim, err := resource.New(client, set, "rl:test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// An unconfigured resource never touches Redis.
	dec, err := lim.Acquire(context.Background(), "mock:sim-1", 500)
	if err != nil {
		t.Fatalf("Acquire unknown resource: %v (should not reach Redis)", err)
	}
	if dec.Limited || !dec.Allowed {
		t.Fatalf("unknown resource: %+v, want unlimited/allowed", dec)
	}

	// A tokens-only resource with a zero-token claim also meters nothing.
	set2, _ := limits.Parse([]byte(`{"resources":[{"name":"mock:sim-1","tokens":{"per_minute":100}}]}`))
	lim2, _ := resource.New(client, set2, "rl:test")
	dec, err = lim2.Acquire(context.Background(), "mock:sim-1", 0)
	if err != nil {
		t.Fatalf("Acquire tokens-only zero-estimate: %v (should not reach Redis)", err)
	}
	if dec.Limited || !dec.Allowed {
		t.Fatalf("tokens-only zero-estimate: %+v, want unlimited", dec)
	}
}

// TestNilSetIsAllUnlimited: a nil Set (no configured limits) leaves every
// resource unlimited.
func TestNilSetIsAllUnlimited(t *testing.T) {
	t.Parallel()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 1})
	t.Cleanup(func() { client.Close() }) //nolint:errcheck // best-effort
	lim, err := resource.New(client, nil, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dec, err := lim.Acquire(context.Background(), "anything:here", 10)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if dec.Limited || !dec.Allowed {
		t.Fatalf("nil set: %+v, want unlimited", dec)
	}
}
