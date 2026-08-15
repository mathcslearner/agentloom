package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/api"
)

// fakeCacheServer stubs the two /v1/cache routes, recording the last bust
// request body it received.
func fakeCacheServer(t *testing.T, lastBust *api.CacheBustRequest) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/cache/bust", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s on /v1/cache/bust", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, lastBust); err != nil {
			t.Errorf("decoding bust body %q: %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.CacheBustResponse{Deleted: 5})
	})
	mux.HandleFunc("/v1/cache/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s on /v1/cache/stats", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.CacheStatsResponse{Plugins: []api.CachePluginStat{
			{Kind: "model_provider", Name: "mock", Hits: 3, Misses: 1, Stores: 4, HitRate: 0.75},
			{Kind: "tool", Name: "json_transform", Hits: 0, Misses: 2, Stores: 0, HitRate: 0},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCacheBustSendsNamespace(t *testing.T) {
	t.Parallel()
	var got api.CacheBustRequest
	srv := fakeCacheServer(t, &got)

	out, _, err := runCtl(t, nil, "cache", "bust",
		"--kind", "model_provider", "--name", "mock",
		"--api", srv.URL, "--key", "sk_"+strings.Repeat("a", 43))
	if err != nil {
		t.Fatalf("cache bust: %v", err)
	}
	if got.PluginKind != "model_provider" || got.PluginName != "mock" {
		t.Errorf("bust request = %+v, want model_provider/mock", got)
	}
	if !strings.Contains(out, "busted 5 cache entries") {
		t.Errorf("output %q missing the deleted count", out)
	}
}

func TestCacheBustNoFlagsBustsAll(t *testing.T) {
	t.Parallel()
	var got api.CacheBustRequest
	srv := fakeCacheServer(t, &got)

	if _, _, err := runCtl(t, nil, "cache", "bust",
		"--api", srv.URL, "--key", "sk_"+strings.Repeat("a", 43)); err != nil {
		t.Fatalf("cache bust: %v", err)
	}
	if got.PluginKind != "" || got.PluginName != "" {
		t.Errorf("no-flag bust sent %+v, want the empty (all) selector", got)
	}
}

func TestCacheStatsRendersTable(t *testing.T) {
	t.Parallel()
	srv := fakeCacheServer(t, &api.CacheBustRequest{})

	out, _, err := runCtl(t, nil, "cache", "stats",
		"--api", srv.URL, "--key", "sk_"+strings.Repeat("r", 43))
	if err != nil {
		t.Fatalf("cache stats: %v", err)
	}
	for _, needle := range []string{
		"KIND", "NAME", "HITS", "MISSES", "STORES", "HIT RATE",
		"model_provider", "mock", "75.0%",
		"json_transform", "0.0%",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("stats output %q missing %q", out, needle)
		}
	}
}
