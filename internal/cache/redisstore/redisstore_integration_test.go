//go:build integration

package redisstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/cache/redisstore"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
)

const opTimeout = 30 * time.Second

// newTestStore connects to the test Redis (same address discipline as the
// queue and ratelimit suites: AGENTLOOM_TEST_REDIS_ADDR, defaulting to the
// compose stack) and returns a store plus a uniquely prefixed namespace
// deleted on cleanup — the per-test isolation the harness uses.
func newTestStore(tb testing.TB, maxValueBytes int64) (*redisstore.Store, *redis.Client, string) {
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
	prefix := "agentloom-test:" + hex.EncodeToString(b[:]) + ":cache"
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		keys, _ := client.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			client.Del(ctx, keys...) //nolint:errcheck // best-effort cleanup in tests
		}
	})

	st, err := redisstore.New(client, prefix, maxValueBytes)
	if err != nil {
		tb.Fatalf("redisstore.New: %v", err)
	}
	return st, client, prefix
}

var testPlugin = cache.PluginRef{Kind: plugin.KindModelProvider, Name: "mock", Version: "1.0.0"}

// TestRoundTrip: a written value reads back byte-identical; a distinct key
// misses.
func TestRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, _, _ := newTestStore(t, 0)

	val := []byte(`{"output":{"text":"hello"},"usage":{"input_tokens":3,"output_tokens":5}}`)
	if err := st.Set(ctx, testPlugin, "key-a", val, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := st.Get(ctx, testPlugin, "key-a")
	if err != nil || !ok {
		t.Fatalf("Get after Set = ok %v, err %v; want a hit", ok, err)
	}
	if !bytes.Equal(got, val) {
		t.Errorf("Get = %s, want %s", got, val)
	}

	if _, ok, err := st.Get(ctx, testPlugin, "key-missing"); ok || err != nil {
		t.Errorf("Get on absent key = ok %v, err %v; want a clean miss", ok, err)
	}
}

// TestTTLApplied: the stored entry carries the TTL (self-eviction) — probed
// via PTTL rather than by sleeping.
func TestTTLApplied(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, client, prefix := newTestStore(t, 0)

	if err := st.Set(ctx, testPlugin, "key-ttl", []byte(`{}`), 90*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	redisKey := cache.RedisKey(prefix, testPlugin, "key-ttl")
	pttl, err := client.PTTL(ctx, redisKey).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	// A live TTL is positive and no larger than what we set (minus the tiny
	// round-trip since Set); a missing TTL surfaces as a negative duration.
	if pttl <= 0 || pttl > 90*time.Second {
		t.Errorf("PTTL = %v, want (0, 90s]", pttl)
	}
}

// TestOversizeSkips: a value over the cap is not stored and returns the typed
// error; the key stays absent.
func TestOversizeSkips(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, _, _ := newTestStore(t, 32)

	big := bytes.Repeat([]byte("x"), 33)
	err := st.Set(ctx, testPlugin, "key-big", big, time.Minute)
	if err == nil || err.Error() != redisstore.ErrValueTooLarge.Error() {
		t.Fatalf("Set oversize = %v, want ErrValueTooLarge", err)
	}
	if _, ok, _ := st.Get(ctx, testPlugin, "key-big"); ok {
		t.Error("oversize value was stored, want skipped")
	}

	// A value exactly at the cap is stored.
	if err := st.Set(ctx, testPlugin, "key-fit", bytes.Repeat([]byte("y"), 32), time.Minute); err != nil {
		t.Errorf("Set at cap: %v", err)
	}
}

// TestNamespacing: two plugins with the same content key never collide, and
// each keys under its own bust-by-prefix namespace.
func TestNamespacing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, client, prefix := newTestStore(t, 0)

	toolPlugin := cache.PluginRef{Kind: plugin.KindTool, Name: "json_transform", Version: "1.0.0"}
	if err := st.Set(ctx, testPlugin, "shared", []byte(`"a"`), time.Minute); err != nil {
		t.Fatalf("Set provider: %v", err)
	}
	if err := st.Set(ctx, toolPlugin, "shared", []byte(`"b"`), time.Minute); err != nil {
		t.Fatalf("Set tool: %v", err)
	}
	if v, _, _ := st.Get(ctx, testPlugin, "shared"); string(v) != `"a"` {
		t.Errorf("provider entry = %s, want \"a\" (no collision)", v)
	}
	if v, _, _ := st.Get(ctx, toolPlugin, "shared"); string(v) != `"b"` {
		t.Errorf("tool entry = %s, want \"b\" (no collision)", v)
	}
	// The tool's entries live under a bust-able prefix (9.6).
	toolPrefix := prefix + ":v" + itoa(cache.KeySchemaVersion) + ":tool:json_transform:*"
	keys, err := client.Keys(ctx, toolPrefix).Result()
	if err != nil || len(keys) != 1 {
		t.Errorf("keys under %q = %v (err %v), want exactly one", toolPrefix, keys, err)
	}
}

func itoa(n int) string {
	if n == 1 {
		return "1"
	}
	// Only KeySchemaVersion 1 exists; keep the helper trivial.
	return "?"
}
