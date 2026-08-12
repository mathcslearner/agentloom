//go:build integration

package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/ratelimit"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 7.2's API-side coverage: the request counter/histogram and the
// 6.4 rate-limit seam recorded through the real router onto a real
// Prometheus registry — route patterns as labels (never raw paths),
// unmatched requests collapsed to one bounded series.

// denyReadAcquirer allows every bucket except a per-key read-class one —
// enough script to produce both an allowed and a denied decision without
// Redis.
type denyReadAcquirer struct{}

func (denyReadAcquirer) Acquire(_ context.Context, b ratelimit.Bucket, _ int64) (ratelimit.Result, error) {
	if strings.HasSuffix(b.Key, ":read") {
		return ratelimit.Result{Allowed: false, Remaining: 0, RetryAfter: time.Second}, nil
	}
	return ratelimit.Result{Allowed: true, Remaining: 1}, nil
}

// counterSample returns the counter value for the exact label subset, 0
// when the series does not exist.
func counterSample(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
	metric:
		for _, m := range fam.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			for k, v := range want {
				if labels[k] != v {
					continue metric
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

func histogramSampleCount(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
	metric:
		for _, m := range fam.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			for k, v := range want {
				if labels[k] != v {
					continue metric
				}
			}
			return m.GetHistogram().GetSampleCount()
		}
	}
	return 0
}

// TestRequestAndRateLimitMetrics drives the real router: a 200, a 429
// from the denying acquirer, a 401, and a 404 must each land in
// engine_api_requests_total under the chi route pattern (or "unmatched"),
// with the rate-limit decision counters mirroring the bucket outcomes.
func TestRequestAndRateLimitMetrics(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	rootKey := mintTestKey(t)
	reg := prometheus.NewRegistry()
	am := metrics.NewAPIMetrics(reg)
	h, err := api.New(s, func() time.Time { return testNow }, nil, rootKey, api.RateLimitOptions{
		Acquirer:  denyReadAcquirer{},
		KeyPrefix: "t:rl",
		Submit:    api.ClassLimit{Capacity: 10, RefillPerSec: 1},
		Read:      api.ClassLimit{Capacity: 10, RefillPerSec: 1},
		Admin:     api.ClassLimit{Capacity: 10, RefillPerSec: 1},
		Global:    api.ClassLimit{Capacity: 100, RefillPerSec: 10},
		Metrics:   am,
	}, api.WithRequestMetrics(am))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "metered", Scopes: []string{"submit", "read"}})

	do := func(method, path, token string, wantStatus int) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s %s = %d, want %d", method, path, resp.StatusCode, wantStatus)
		}
	}

	do(http.MethodGet, "/v1/runs", client.Key, http.StatusTooManyRequests) // read class denied
	do(http.MethodGet, "/v1/runs", "", http.StatusUnauthorized)            // no credential
	do(http.MethodGet, "/no/such/route", "", http.StatusNotFound)          // unmatched
	do(http.MethodGet, "/healthz", "", http.StatusOK)                      // exempt route, still metered

	assertCounter := func(labels map[string]string, want float64) {
		t.Helper()
		if got := counterSample(t, reg, "engine_api_requests_total", labels); got != want {
			t.Errorf("requests_total%v = %v, want %v", labels, got, want)
		}
	}
	assertCounter(map[string]string{"route": "/v1/runs", "method": "GET", "code": "429"}, 1)
	assertCounter(map[string]string{"route": "/v1/runs", "method": "GET", "code": "401"}, 1)
	assertCounter(map[string]string{"route": "unmatched", "method": "GET", "code": "404"}, 1)
	assertCounter(map[string]string{"route": "/healthz", "method": "GET", "code": "200"}, 1)
	// The key mint via createKey: POST /v1/keys, 201.
	assertCounter(map[string]string{"route": "/v1/keys", "method": "POST", "code": "201"}, 1)

	if got := histogramSampleCount(t, reg, "engine_api_request_duration_seconds",
		map[string]string{"route": "/v1/runs", "method": "GET"}); got != 2 {
		t.Errorf("request duration{/v1/runs,GET} observations = %d, want 2 (the 429 and the 401)", got)
	}

	// Rate-limit decisions: the denied read consumed one per-key denial;
	// the admin mint consumed a per-key allow and a global allow. The 401
	// and 404 never reached the limiter.
	if got := counterSample(t, reg, "engine_api_ratelimit_decisions_total",
		map[string]string{"class": "read", "bucket": "per_key", "decision": "denied"}); got != 1 {
		t.Errorf("decisions{read,per_key,denied} = %v, want 1", got)
	}
	if got := counterSample(t, reg, "engine_api_ratelimit_decisions_total",
		map[string]string{"class": "admin", "bucket": "per_key", "decision": "allowed"}); got != 1 {
		t.Errorf("decisions{admin,per_key,allowed} = %v, want 1", got)
	}
	if got := counterSample(t, reg, "engine_api_ratelimit_decisions_total",
		map[string]string{"class": "admin", "bucket": "global", "decision": "allowed"}); got != 1 {
		t.Errorf("decisions{admin,global,allowed} = %v, want 1", got)
	}

	// In-flight gauge (ticket 7.5): every request above has returned, so
	// the started/finished bracket must balance back to zero.
	if got := gaugeSample(t, reg, "engine_api_requests_in_flight"); got != 0 {
		t.Errorf("requests_in_flight after quiescence = %v, want 0", got)
	}
}

// gaugeSample returns the unlabeled gauge's value, failing the test when
// the family is absent (a gauge that never registered is broken wiring,
// not a zero).
func gaugeSample(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("gauge %s not gathered", name)
	return 0
}
