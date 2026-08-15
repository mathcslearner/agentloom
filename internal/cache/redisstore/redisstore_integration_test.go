//go:build integration

package redisstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
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

var toolPlugin = cache.PluginRef{Kind: plugin.KindTool, Name: "json_transform", Version: "1.0.0"}

// TestStatsCounters: Get/Set increment the per-plugin counters exactly, and
// Stats reads them back with a computed hit rate. A hit, a miss, and a store
// each bump their own field; a plugin never invoked has no stats row.
func TestStatsCounters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, _, _ := newTestStore(t, 0)

	// Two stores, one hit, two misses for the model provider.
	if err := st.Set(ctx, testPlugin, "a", []byte(`"a"`), time.Minute); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if err := st.Set(ctx, testPlugin, "b", []byte(`"b"`), time.Minute); err != nil {
		t.Fatalf("Set b: %v", err)
	}
	if _, _, err := st.Get(ctx, testPlugin, "a"); err != nil { // hit
		t.Fatalf("Get a: %v", err)
	}
	if _, _, err := st.Get(ctx, testPlugin, "missing-1"); err != nil { // miss
		t.Fatalf("Get missing-1: %v", err)
	}
	if _, _, err := st.Get(ctx, testPlugin, "missing-2"); err != nil { // miss
		t.Fatalf("Get missing-2: %v", err)
	}
	// A different plugin gets exactly one store — proving per-plugin isolation.
	if err := st.Set(ctx, toolPlugin, "c", []byte(`"c"`), time.Minute); err != nil {
		t.Fatalf("Set tool c: %v", err)
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	byName := map[string]cache.PluginStats{}
	for _, s := range stats {
		byName[string(s.Kind)+":"+s.Name] = s
	}
	if len(byName) != 2 {
		t.Fatalf("Stats returned %d plugins, want 2: %+v", len(byName), stats)
	}

	mp := byName["model_provider:mock"]
	if mp.Hits != 1 || mp.Misses != 2 || mp.Stores != 2 {
		t.Errorf("model_provider counters = hits %d misses %d stores %d; want 1/2/2", mp.Hits, mp.Misses, mp.Stores)
	}
	if mp.HitRate != 1.0/3.0 {
		t.Errorf("model_provider hit rate = %v, want 1/3", mp.HitRate)
	}
	tool := byName["tool:json_transform"]
	if tool.Hits != 0 || tool.Misses != 0 || tool.Stores != 1 {
		t.Errorf("tool counters = hits %d misses %d stores %d; want 0/0/1", tool.Hits, tool.Misses, tool.Stores)
	}
	if tool.HitRate != 0 {
		t.Errorf("tool hit rate = %v, want 0 (no lookups)", tool.HitRate)
	}

	// Sorted by (kind, name): model_provider before tool.
	if len(stats) == 2 && (stats[0].Kind != plugin.KindModelProvider || stats[1].Kind != plugin.KindTool) {
		t.Errorf("Stats not sorted by kind: %+v", stats)
	}
}

// TestBustGranularity: bust one concrete plugin, one whole kind, then
// everything — each leaving the non-matching entries and all stats intact.
func TestBustGranularity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, client, prefix := newTestStore(t, 0)

	retrPlugin := cache.PluginRef{Kind: plugin.KindRetriever, Name: "pg_fulltext", Version: "1.0.0"}
	toolB := cache.PluginRef{Kind: plugin.KindTool, Name: "http_request", Version: "1.0.0"}

	seed := func(p cache.PluginRef, n int) {
		for i := 0; i < n; i++ {
			if err := st.Set(ctx, p, "k"+strconv.Itoa(i), []byte(`"v"`), time.Hour); err != nil {
				t.Fatalf("seed Set: %v", err)
			}
		}
	}
	seed(testPlugin, 3) // model_provider:mock
	seed(toolPlugin, 4) // tool:json_transform
	seed(toolB, 2)      // tool:http_request
	seed(retrPlugin, 5) // retriever:pg_fulltext

	countEntries := func(p cache.PluginRef) int {
		pat, err := cache.BustPattern(prefix, cache.BustMatch{Kind: p.Kind, Name: p.Name})
		if err != nil {
			t.Fatalf("BustPattern: %v", err)
		}
		keys, err := client.Keys(ctx, pat).Result()
		if err != nil {
			t.Fatalf("Keys: %v", err)
		}
		return len(keys)
	}

	// Bust one concrete plugin (json_transform): only its 4 entries go.
	if n, err := st.Bust(ctx, cache.BustMatch{Kind: plugin.KindTool, Name: "json_transform"}); err != nil || n != 4 {
		t.Fatalf("Bust json_transform = %d, %v; want 4, nil", n, err)
	}
	if got := countEntries(toolPlugin); got != 0 {
		t.Errorf("json_transform entries after bust = %d, want 0", got)
	}
	if got := countEntries(toolB); got != 2 {
		t.Errorf("http_request entries after tool-name bust = %d, want 2 (untouched)", got)
	}

	// Bust the whole tool kind: http_request's 2 remaining entries go; the
	// retriever and provider are untouched.
	if n, err := st.Bust(ctx, cache.BustMatch{Kind: plugin.KindTool}); err != nil || n != 2 {
		t.Fatalf("Bust tool kind = %d, %v; want 2, nil", n, err)
	}
	if got := countEntries(retrPlugin); got != 5 {
		t.Errorf("retriever entries after tool-kind bust = %d, want 5 (untouched)", got)
	}
	if got := countEntries(testPlugin); got != 3 {
		t.Errorf("provider entries after tool-kind bust = %d, want 3 (untouched)", got)
	}

	// Stats survived every bust (the 14 seed Sets are all still counted).
	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	var totalStores int64
	for _, s := range stats {
		totalStores += s.Stores
	}
	if totalStores != 14 {
		t.Errorf("total stores after busts = %d, want 14 (stats survive busts)", totalStores)
	}

	// Bust everything: the remaining provider + retriever entries (8) go.
	if n, err := st.Bust(ctx, cache.BustMatch{}); err != nil || n != 8 {
		t.Fatalf("Bust all = %d, %v; want 8, nil", n, err)
	}
	if got := countEntries(retrPlugin) + countEntries(testPlugin); got != 0 {
		t.Errorf("entries after bust-all = %d, want 0", got)
	}
	// Stats still present after bust-all.
	if stats, err := st.Stats(ctx); err != nil || len(stats) == 0 {
		t.Errorf("Stats after bust-all = %d rows, %v; want the counters to survive", len(stats), err)
	}
}

// TestBustUnderLoad: with thousands of entries across two namespaces and
// concurrent reads/writes running throughout, busting one namespace removes
// exactly its keys, never errors the concurrent operations, and leaves the
// other namespace intact (acceptance: "bust removes matching keys without
// blocking Redis, verified under load"). Non-blocking holds by construction
// (SCAN + UNLINK); this proves correctness under concurrency.
func TestBustUnderLoad(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, client, prefix := newTestStore(t, 0)

	const seeded = 3000
	victim := testPlugin // model_provider:mock — the namespace we bust
	keeper := toolPlugin // tool:json_transform — must survive intact

	// Seed both namespaces fast with a raw pipeline (cache.RedisKey is the
	// same layout the store writes), so the test spends its time on the
	// concurrent bust, not setup.
	seedRaw := func(p cache.PluginRef, n int) {
		pipe := client.Pipeline()
		for i := 0; i < n; i++ {
			key := cache.RedisKey(prefix, p, fmt.Sprintf("load-%06d", i))
			pipe.Set(ctx, key, "v", time.Hour)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			t.Fatalf("seed pipeline: %v", err)
		}
	}
	seedRaw(victim, seeded)
	seedRaw(keeper, seeded)

	// Concurrent load on the keeper namespace throughout the bust: readers
	// and writers that must all succeed while the victim namespace is torn
	// down underneath them.
	var (
		stop    atomic.Bool
		opErr   atomic.Value // first error from any concurrent op
		opCount atomic.Int64
		wg      sync.WaitGroup
	)
	recordErr := func(err error) {
		if err != nil {
			opErr.CompareAndSwap(nil, err)
		}
	}
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			i := 0
			for !stop.Load() {
				k := fmt.Sprintf("live-%d-%d", id, i)
				if err := st.Set(ctx, keeper, k, []byte(`"x"`), time.Minute); err != nil {
					recordErr(fmt.Errorf("concurrent Set: %w", err))
					return
				}
				if _, _, err := st.Get(ctx, keeper, k); err != nil {
					recordErr(fmt.Errorf("concurrent Get: %w", err))
					return
				}
				opCount.Add(1)
				i++
			}
		}(w)
	}

	deleted, err := st.Bust(ctx, cache.BustMatch{Kind: victim.Kind, Name: victim.Name})
	stop.Store(true)
	wg.Wait()

	if err != nil {
		t.Fatalf("Bust under load: %v", err)
	}
	if e := opErr.Load(); e != nil {
		t.Fatalf("a concurrent operation failed during bust: %v", e)
	}
	if opCount.Load() == 0 {
		t.Fatal("no concurrent operations ran during the bust — the test proved nothing")
	}
	if deleted != seeded {
		t.Errorf("Bust deleted %d, want %d seeded victim keys", deleted, seeded)
	}

	// The victim namespace is empty; the keeper namespace kept all its seed
	// keys (the concurrent live-* writes only add to it).
	victimPat, _ := cache.BustPattern(prefix, cache.BustMatch{Kind: victim.Kind, Name: victim.Name})
	if keys, err := client.Keys(ctx, victimPat).Result(); err != nil || len(keys) != 0 {
		t.Errorf("victim namespace after bust = %d keys, %v; want 0", len(keys), err)
	}
	keeperPat, _ := cache.BustPattern(prefix, cache.BustMatch{Kind: keeper.Kind, Name: keeper.Name})
	if keys, err := client.Keys(ctx, keeperPat).Result(); err != nil || len(keys) < seeded {
		t.Errorf("keeper namespace after bust = %d keys, %v; want ≥ %d (untouched)", len(keys), err, seeded)
	}
}
