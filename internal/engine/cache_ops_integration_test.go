//go:build integration

package engine_test

// Ticket 9.6's headline e2e: the cache stats endpoint reconciles against the
// worker fleet's Prometheus counters, and an admin bust forces the next
// identical step to miss and re-execute. It reuses 9.5's cache fixture (a
// real Redis store, the counting mock provider) and stands a real API
// handler over the same store and the same cache store — so the numbers the
// operator reads through GET /v1/cache/stats are exactly the numbers the
// engine recorded, and POST /v1/cache/bust removes the entry the engine
// wrote.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
)

// cacheOpsAPI stands a real API handler over the fixture's store and cache
// store, authenticated by a fresh root key. It returns the server and the
// bearer.
func cacheOpsAPI(t *testing.T, f *cacheFixture) (*httptest.Server, string) {
	t.Helper()
	rootKey := "sk_" + base64.RawURLEncoding.EncodeToString(randomBytes(t, 32))
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	h, err := api.New(f.s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{},
		api.WithCacheOps(f.cacheStore))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, rootKey
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("entropy: %v", err)
	}
	return b
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestCacheStatsReconcileAndBust is the ticket's acceptance: the stats
// endpoint matches the Prometheus counters, and a bust makes the next run a
// miss.
func TestCacheStatsReconcileAndBust(t *testing.T) {
	t.Parallel()
	f := newCacheFixture(t)

	// Miss + store, then a hit: engine_cache counters land at 1/1/1 for
	// model_provider:mock.
	f.runToSuccess(t, cacheDefTemp0(t))
	f.runToSuccess(t, cacheDefTemp0(t))
	f.h.WaitQuiescent(t.Context())
	if got := f.prov.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 before bust", got)
	}

	const label = "model_provider:mock"
	promHits := counterValue(t, f.mreg, "engine_cache_hits_total", map[string]string{"plugin": label})
	promMisses := counterValue(t, f.mreg, "engine_cache_misses_total", map[string]string{"plugin": label})
	promStores := counterValue(t, f.mreg, "engine_cache_stores_total", map[string]string{"plugin": label})
	if promHits != 1 || promMisses != 1 || promStores != 1 {
		t.Fatalf("prometheus counters = hits %v misses %v stores %v, want 1/1/1", promHits, promMisses, promStores)
	}

	srv, rootKey := cacheOpsAPI(t, f)

	// The stats endpoint's numbers equal the Prometheus counters exactly —
	// the reconciliation the ticket demands.
	var stats api.CacheStatsResponse
	if res := doCacheReq(t, srv, http.MethodGet, "/v1/cache/stats", rootKey, nil, &stats); res != http.StatusOK {
		t.Fatalf("GET /v1/cache/stats = %d, want 200", res)
	}
	mock := findStat(t, stats, "model_provider", "mock")
	if float64(mock.Hits) != promHits || float64(mock.Misses) != promMisses || float64(mock.Stores) != promStores {
		t.Errorf("stats endpoint = %+v, does not match prometheus hits %v misses %v stores %v",
			mock, promHits, promMisses, promStores)
	}
	if mock.HitRate != 0.5 {
		t.Errorf("hit rate = %v, want 0.5 (1 hit / 2 lookups)", mock.HitRate)
	}

	// Bust the provider's entries through the admin endpoint.
	var bust api.CacheBustResponse
	body := []byte(`{"plugin_kind":"model_provider","plugin_name":"mock"}`)
	if res := doCacheReq(t, srv, http.MethodPost, "/v1/cache/bust", rootKey, body, &bust); res != http.StatusOK {
		t.Fatalf("POST /v1/cache/bust = %d, want 200", res)
	}
	if bust.Deleted < 1 {
		t.Errorf("bust deleted %d, want ≥ 1 (the cached entry)", bust.Deleted)
	}

	// The next identical run must miss and call the provider again — proof
	// the bust removed the entry.
	f.runToSuccess(t, cacheDefTemp0(t))
	f.h.WaitQuiescent(t.Context())
	if got := f.prov.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d after bust + rerun, want 2 (the bust forced a miss)", got)
	}

	// The stats endpoint reflects the new miss + store (a bust never zeroes
	// the cumulative counters, only removes entries).
	var after api.CacheStatsResponse
	if res := doCacheReq(t, srv, http.MethodGet, "/v1/cache/stats", rootKey, nil, &after); res != http.StatusOK {
		t.Fatalf("GET /v1/cache/stats (after) = %d, want 200", res)
	}
	m := findStat(t, after, "model_provider", "mock")
	if m.Misses != 2 || m.Stores != 2 {
		t.Errorf("stats after bust+rerun = misses %d stores %d, want 2/2 (counters survive the bust)", m.Misses, m.Stores)
	}
}

// findStat returns the plugin's stat row, failing if absent.
func findStat(t *testing.T, resp api.CacheStatsResponse, kind, name string) api.CachePluginStat {
	t.Helper()
	for _, s := range resp.Plugins {
		if s.Kind == kind && s.Name == name {
			return s
		}
	}
	t.Fatalf("stats has no %s/%s: %+v", kind, name, resp.Plugins)
	return api.CachePluginStat{}
}

// doCacheReq runs one authenticated request, decoding a JSON body into out
// (may be nil) and returning the status code.
func doCacheReq(t *testing.T, srv *httptest.Server, method, path, bearer string, body []byte, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close() //nolint:errcheck // read-side close
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decoding %s %s: %v", method, path, err)
		}
	}
	return res.StatusCode
}
