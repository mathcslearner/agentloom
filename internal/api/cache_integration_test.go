//go:build integration

package api_test

// Ticket 9.6: the response-cache ops surface — POST /v1/cache/bust and
// GET /v1/cache/stats. Auth/rate-limit coverage rides the route tables'
// matrix (auth_routes_integration_test.go); this suite covers the behavior:
// the bust namespace selector maps onto the store, the audit log carries the
// actor key id, the stats payload projects the store's counters, the request
// validation, and the 503 when the cache is not wired. The seam is a fake
// CacheOps, so no Redis is needed here — the store-reconciliation-against-
// Prometheus criterion is the engine-layer headline test (ticket 9.6).

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// fakeCacheOps is an in-memory CacheOps: it records the bust matches it was
// asked to run and returns canned results, so the handler's request→store
// mapping and audit logging can be asserted without Redis.
type fakeCacheOps struct {
	mu      sync.Mutex
	busts   []cache.BustMatch
	deleted int64
	stats   []cache.PluginStats
	bustErr error
}

func (f *fakeCacheOps) Bust(_ context.Context, m cache.BustMatch) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busts = append(f.busts, m)
	return f.deleted, f.bustErr
}

func (f *fakeCacheOps) Stats(context.Context) ([]cache.PluginStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, nil
}

func (f *fakeCacheOps) lastBust(t *testing.T) cache.BustMatch {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.busts) == 0 {
		t.Fatal("no bust recorded")
	}
	return f.busts[len(f.busts)-1]
}

// cacheOpsServer boots the API with a root key, a captured log stream, and a
// wired CacheOps. A nil ops leaves the routes answering 503.
func cacheOpsServer(t *testing.T, rootKey string, ops api.CacheOps) (*httptest.Server, *syncBuffer) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t))
	logs := &syncBuffer{}
	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, logs)
	opts := []api.Option{}
	if ops != nil {
		opts = append(opts, api.WithCacheOps(ops))
	}
	h, err := api.New(s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{}, opts...)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, logs
}

// TestCacheBustNamespaceSelectors: each request shape maps onto the matching
// store BustMatch, and the deleted count is echoed back.
func TestCacheBustNamespaceSelectors(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	ops := &fakeCacheOps{deleted: 7}
	srv, _ := cacheOpsServer(t, rootKey, ops)

	tests := []struct {
		name string
		body string
		want cache.BustMatch
	}{
		{"all", `{}`, cache.BustMatch{}},
		{"kind only", `{"plugin_kind":"tool"}`, cache.BustMatch{Kind: plugin.KindTool}},
		{"kind and name", `{"plugin_kind":"model_provider","plugin_name":"mock"}`, cache.BustMatch{Kind: plugin.KindModelProvider, Name: "mock"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp api.CacheBustResponse
			res := doAuth(t, srv, http.MethodPost, "/v1/cache/bust", rootKey, []byte(tt.body), &resp)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("bust = %d, want 200", res.StatusCode)
			}
			if resp.Deleted != 7 {
				t.Errorf("deleted = %d, want 7", resp.Deleted)
			}
			if got := ops.lastBust(t); got != tt.want {
				t.Errorf("store bust = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestCacheBustEmptyBody: an entirely absent body busts everything (the body
// is optional).
func TestCacheBustEmptyBody(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	ops := &fakeCacheOps{deleted: 3}
	srv, _ := cacheOpsServer(t, rootKey, ops)

	var resp api.CacheBustResponse
	res := doAuth(t, srv, http.MethodPost, "/v1/cache/bust", rootKey, nil, &resp)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bust with no body = %d, want 200", res.StatusCode)
	}
	if got := ops.lastBust(t); !got.All() {
		t.Errorf("empty body busted %+v, want all", got)
	}
}

// TestCacheBustAuditLog: a successful bust logs the actor key id, the
// namespace, and the deleted count — the ADR-007 audit requirement.
func TestCacheBustAuditLog(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	ops := &fakeCacheOps{deleted: 12}
	srv, logs := cacheOpsServer(t, rootKey, ops)

	// Authenticate as a named admin key (not root) so the audited actor is a
	// real key id.
	admin := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "ops", Scopes: []string{"admin"}})

	res := doAuth(t, srv, http.MethodPost, "/v1/cache/bust", admin.Key,
		[]byte(`{"plugin_kind":"model_provider","plugin_name":"mock"}`), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bust = %d, want 200", res.StatusCode)
	}

	// Find the audit line and assert its fields.
	var audit map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == "cache bust" {
			audit = m
		}
	}
	if audit == nil {
		t.Fatalf("no 'cache bust' audit line in logs:\n%s", logs.String())
	}
	if audit["action"] != "cache_bust" {
		t.Errorf("audit action = %v, want cache_bust", audit["action"])
	}
	if audit["key_id"] != admin.ID {
		t.Errorf("audit key_id = %v, want the acting key id %s", audit["key_id"], admin.ID)
	}
	if audit["plugin_kind"] != "model_provider" || audit["plugin_name"] != "mock" {
		t.Errorf("audit namespace = kind %v name %v, want model_provider/mock", audit["plugin_kind"], audit["plugin_name"])
	}
	if audit["deleted"] != float64(12) {
		t.Errorf("audit deleted = %v, want 12", audit["deleted"])
	}
}

// TestCacheBustBadRequest: a name without a kind, an unknown kind, and an
// unknown body field are all 400s.
func TestCacheBustBadRequest(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	ops := &fakeCacheOps{}
	srv, _ := cacheOpsServer(t, rootKey, ops)

	for name, body := range map[string]string{
		"name without kind":  `{"plugin_name":"mock"}`,
		"unknown kind":       `{"plugin_kind":"executor"}`,
		"non-cacheable kind": `{"plugin_kind":"validator"}`,
		"unknown field":      `{"nope":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			var envelope api.ErrorBody
			res := doAuth(t, srv, http.MethodPost, "/v1/cache/bust", rootKey, []byte(body), &envelope)
			if res.StatusCode != http.StatusBadRequest || envelope.Error.Code != api.ErrCodeInvalidRequest {
				t.Errorf("bust %q = %d/%q, want 400/invalid_request", body, res.StatusCode, envelope.Error.Code)
			}
		})
	}
	ops.mu.Lock()
	defer ops.mu.Unlock()
	if len(ops.busts) != 0 {
		t.Errorf("a rejected request reached the store: %+v", ops.busts)
	}
}

// TestCacheStats: the store's per-plugin counters project onto the response
// with the hit rate carried through.
func TestCacheStats(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	ops := &fakeCacheOps{
		stats: []cache.PluginStats{
			cache.NewPluginStats(cache.PluginRef{Kind: plugin.KindModelProvider, Name: "mock"}, 3, 1, 4),
			cache.NewPluginStats(cache.PluginRef{Kind: plugin.KindTool, Name: "json_transform"}, 0, 5, 0),
		},
	}
	srv, _ := cacheOpsServer(t, rootKey, ops)

	var resp api.CacheStatsResponse
	res := doAuth(t, srv, http.MethodGet, "/v1/cache/stats", rootKey, nil, &resp)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stats = %d, want 200", res.StatusCode)
	}
	if len(resp.Plugins) != 2 {
		t.Fatalf("stats returned %d plugins, want 2", len(resp.Plugins))
	}
	mp := resp.Plugins[0]
	if mp.Kind != "model_provider" || mp.Name != "mock" || mp.Hits != 3 || mp.Misses != 1 || mp.Stores != 4 {
		t.Errorf("model_provider stat = %+v", mp)
	}
	if mp.HitRate != 0.75 {
		t.Errorf("model_provider hit rate = %v, want 0.75", mp.HitRate)
	}
	if tool := resp.Plugins[1]; tool.HitRate != 0 {
		t.Errorf("tool hit rate = %v, want 0 (no hits)", tool.HitRate)
	}
}

// TestCacheOpsUnavailable: with no CacheOps wired, both routes answer 503
// cache_unavailable (past the auth gate — the routes exist, the surface does
// not).
func TestCacheOpsUnavailable(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	srv, _ := cacheOpsServer(t, rootKey, nil)

	for _, probe := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/cache/bust", `{}`},
		{http.MethodGet, "/v1/cache/stats", ""},
	} {
		var envelope api.ErrorBody
		var body []byte
		if probe.body != "" {
			body = []byte(probe.body)
		}
		res := doAuth(t, srv, probe.method, probe.path, rootKey, body, &envelope)
		if res.StatusCode != http.StatusServiceUnavailable || envelope.Error.Code != api.ErrCodeCacheUnavailable {
			t.Errorf("%s %s unwired = %d/%q, want 503/cache_unavailable",
				probe.method, probe.path, res.StatusCode, envelope.Error.Code)
		}
	}
}
