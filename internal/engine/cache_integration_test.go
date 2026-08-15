//go:build integration

package engine_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/cache/redisstore"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 9.5's headline e2e: the response-cache middleware ahead of the rate
// limiter. Two runs of an identical temperature=0 llm step share a cache
// entry — the second run is a hit that skips both the provider and the
// limiter — while a temperature>0 step bypasses by default and a read_write
// opt-in overrides that, all proven against a real Redis store and the
// offline mock provider.

// countingProvider wraps a provider and counts Chat calls — the probe
// proving a cache hit never reaches the provider. It delegates Manifest so
// routing (mock/<model>) and the cache-key plugin identity are unchanged.
type countingProvider struct {
	inner llm.Provider
	calls atomic.Int64
}

func (c *countingProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	c.calls.Add(1)
	return c.inner.Chat(ctx, req)
}
func (c *countingProvider) Manifest() plugin.Manifest { return c.inner.Manifest() }

// grantingLimiter always grants and counts acquires — the probe proving a
// cache hit skips the limiter entirely (zero acquires on the hit).
type grantingLimiter struct{ acquires atomic.Int64 }

func (l *grantingLimiter) Acquire(context.Context, string, int64) (resource.Decision, error) {
	l.acquires.Add(1)
	return resource.Decision{Limited: true, Allowed: true, Resource: "mock:sim-1"}, nil
}
func (l *grantingLimiter) Reconcile(context.Context, string, int64, int64) error { return nil }

// newCacheStore builds a redisstore over the harness Redis with a unique,
// cleaned-up prefix (per-test isolation).
func newCacheStore(t *testing.T, client *redis.Client) *redisstore.Store {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("prefix: %v", err)
	}
	prefix := "agentloom-test:" + hex.EncodeToString(b[:]) + ":cache"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		keys, _ := client.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			client.Del(ctx, keys...) //nolint:errcheck // best-effort
		}
	})
	st, err := redisstore.New(client, prefix, 0)
	if err != nil {
		t.Fatalf("redisstore.New: %v", err)
	}
	return st
}

// cacheEngineFixture wires a store, harness, dispatcher, counting mock
// provider, granting limiter, and cache store into one engine, and returns a
// runner that submits a definition and waits for success.
type cacheFixture struct {
	s          *store.Store
	h          *queuetest.Harness
	prov       *countingProvider
	limiter    *grantingLimiter
	cacheStore *redisstore.Store
	// mreg is the worker Prometheus registry carrying the engine_cache_*
	// counters — the 9.6 reconciliation source of truth.
	mreg *prometheus.Registry
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	mock, err := llm.NewMock(llm.MockConfig{})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	prov := &countingProvider{inner: mock}
	providers, err := llm.NewRegistry(prov)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	limiter := &grantingLimiter{}
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(providers))
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	cacheStore := newCacheStore(t, h.Client())
	// A real worker metrics registry so the engine records engine_cache_*
	// counters the 9.6 stats endpoint reconciles against (ticket 9.6).
	mreg := metrics.NewRegistry(metrics.ServiceWorker)
	wm := metrics.NewWorkerMetrics(mreg)
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "cache-worker",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithResourceLimiter(limiter),
		engine.WithMetrics(wm),
		engine.WithResponseCache(cacheStore, time.Hour))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("cache-worker", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return &cacheFixture{s: s, h: h, prov: prov, limiter: limiter, cacheStore: cacheStore, mreg: mreg}
}

// runToSuccess submits one run of def and waits for it to complete.
func (f *cacheFixture) runToSuccess(t *testing.T, def *dag.Definition) uuid.UUID {
	t.Helper()
	res, err := f.s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	waitRun(t, f.s, res.Run.ID, store.RunStatusSucceeded)
	return res.Run.ID
}

const cacheStepID = "gen"

// deterministic temperature=0 step (default read_write).
func cacheDefTemp0(t *testing.T) *dag.Definition {
	return replaceCache(t, `,"temperature":0`, "")
}

// non-deterministic temperature>0 step (default bypass).
func cacheDefTempHi(t *testing.T) *dag.Definition {
	return replaceCache(t, `,"temperature":0.7`, "")
}

// temperature>0 with an explicit read_write opt-in.
func cacheDefTempHiOptIn(t *testing.T) *dag.Definition {
	return replaceCache(t, `,"temperature":0.7`, `,"cache":{"mode":"read_write"}`)
}

// temperature=0 with an explicit mode off.
func cacheDefTemp0Off(t *testing.T) *dag.Definition {
	return replaceCache(t, `,"temperature":0`, `,"cache":{"mode":"off"}`)
}

// replaceCache builds a definition with the given config tail and step-level
// cache block.
func replaceCache(t *testing.T, cfgTail, cacheBlock string) *dag.Definition {
	t.Helper()
	body := `{"model":"mock/sim-1","prompt":"summarize turtles"` + cfgTail + `}`
	step := `{"id": "` + cacheStepID + `", "type": "llm", "config": ` + body
	if cacheBlock != "" {
		step += cacheBlock
	}
	step += `}`
	return mustDecode(t, `{
		"schema_version": 1,
		"name": "cache-test",
		"steps": [`+step+`],
		"edges": []
	}`)
}

// TestResponseCacheHit is 9.5's exit criterion: two identical temperature=0
// llm runs — the second is a hit, so the provider is called once and the
// limiter acquired once total, and the hit's attempt records counterfactual
// usage marked cache_hit.
func TestResponseCacheHit(t *testing.T) {
	t.Parallel()
	f := newCacheFixture(t)

	run1 := f.runToSuccess(t, cacheDefTemp0(t))
	run2 := f.runToSuccess(t, cacheDefTemp0(t))
	f.h.WaitQuiescent(t.Context())

	if got := f.prov.calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (the second run is a cache hit)", got)
	}
	if got := f.limiter.acquires.Load(); got != 1 {
		t.Errorf("limiter acquires = %d, want 1 (a hit skips the limiter)", got)
	}

	// The miss recorded real usage (no cache_hit); the hit recorded the same
	// counts marked counterfactual.
	miss := latestAttempt(t, f.s, run1, cacheStepID)
	hit := latestAttempt(t, f.s, run2, cacheStepID)
	mu := mustUsage(t, miss.Usage)
	hu := mustUsage(t, hit.Usage)
	if mu.CacheHit {
		t.Error("first run (miss) attempt marked cache_hit, want real usage")
	}
	if !hu.CacheHit {
		t.Error("second run (hit) attempt not marked cache_hit")
	}
	if hu.InputTokens != mu.InputTokens || hu.OutputTokens != mu.OutputTokens {
		t.Errorf("hit usage %+v != miss usage %+v — counterfactual counts must match", hu, mu)
	}

	// The served output is byte-identical to the miss's output.
	o1, _ := f.s.Steps().Get(t.Context(), run1, cacheStepID)
	o2, _ := f.s.Steps().Get(t.Context(), run2, cacheStepID)
	if !jsonEqual(t, o1.Output, o2.Output) {
		t.Errorf("hit output %s != miss output %s", o2.Output, o1.Output)
	}
}

// TestResponseCacheTemperatureBypass: a temperature>0 step is not cached by
// default (two provider calls), but a read_write opt-in makes the second a
// hit (one call).
func TestResponseCacheTemperatureBypass(t *testing.T) {
	t.Parallel()

	t.Run("default bypass", func(t *testing.T) {
		t.Parallel()
		f := newCacheFixture(t)
		f.runToSuccess(t, cacheDefTempHi(t))
		f.runToSuccess(t, cacheDefTempHi(t))
		f.h.WaitQuiescent(t.Context())
		if got := f.prov.calls.Load(); got != 2 {
			t.Errorf("provider calls = %d, want 2 (temperature>0 is not cached by default)", got)
		}
	})

	t.Run("read_write opt-in", func(t *testing.T) {
		t.Parallel()
		f := newCacheFixture(t)
		run1 := f.runToSuccess(t, cacheDefTempHiOptIn(t))
		run2 := f.runToSuccess(t, cacheDefTempHiOptIn(t))
		f.h.WaitQuiescent(t.Context())
		if got := f.prov.calls.Load(); got != 1 {
			t.Errorf("provider calls = %d, want 1 (read_write opts a temperature>0 step in)", got)
		}
		if !mustUsage(t, latestAttempt(t, f.s, run2, cacheStepID).Usage).CacheHit {
			t.Error("opted-in second run not marked cache_hit")
		}
		_ = run1
	})
}

// TestResponseCacheModeOff: an explicit mode off opts a default-cached
// (temperature=0) step out — two provider calls, no hit.
func TestResponseCacheModeOff(t *testing.T) {
	t.Parallel()
	f := newCacheFixture(t)
	f.runToSuccess(t, cacheDefTemp0Off(t))
	f.runToSuccess(t, cacheDefTemp0Off(t))
	f.h.WaitQuiescent(t.Context())
	if got := f.prov.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (mode off disables caching)", got)
	}
}

// mustUsage decodes an attempt's usage JSON.
func mustUsage(t *testing.T, raw json.RawMessage) exec.Usage {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("attempt has no usage")
	}
	var u exec.Usage
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("unmarshaling usage %s: %v", raw, err)
	}
	return u
}
