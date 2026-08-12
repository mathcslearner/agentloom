package api

// Ticket 6.4's unit coverage: middleware semantics (threshold exactness,
// headers, global bucket, fail-open, disabled mode, no-consumption on
// 401), header math, options validation, and the metrics hooks — all
// against a scripted in-memory acquirer, no Redis. The real-limiter
// behavior is ratelimit_integration_test.go's.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/ratelimit"
	"github.com/mathcslearner/agentloom/internal/store"
)

// fakeAcquirer is an in-memory never-refilling token bucket per key: a
// deterministic stand-in for the Redis limiter. It records every acquire
// in order.
type fakeAcquirer struct {
	mu         sync.Mutex
	used       map[string]int64
	calls      []string
	err        error         // when set, every Acquire fails
	retryAfter time.Duration // reported on denials
}

func (f *fakeAcquirer) Acquire(_ context.Context, b ratelimit.Bucket, cost int64) (ratelimit.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, b.Key)
	if f.err != nil {
		return ratelimit.Result{}, f.err
	}
	if f.used == nil {
		f.used = map[string]int64{}
	}
	used := f.used[b.Key]
	if used+cost > b.Capacity {
		return ratelimit.Result{Allowed: false, Remaining: b.Capacity - used, RetryAfter: f.retryAfter}, nil
	}
	f.used[b.Key] = used + cost
	return ratelimit.Result{Allowed: true, Remaining: b.Capacity - used - cost}, nil
}

func (f *fakeAcquirer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// recordingMetrics captures the M7 hook invocations.
type recordingMetrics struct {
	mu        sync.Mutex
	decisions []string // "<class>/<key|global>/<allowed>"
	failOpens []string
}

func (m *recordingMetrics) Decision(class string, global, allowed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := "key"
	if global {
		scope = "global"
	}
	outcome := "denied"
	if allowed {
		outcome = "allowed"
	}
	m.decisions = append(m.decisions, class+"/"+scope+"/"+outcome)
}

func (m *recordingMetrics) FailOpen(class string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failOpens = append(m.failOpens, class)
}

// testLimits is the unit suite's bucket geometry: read capacity 3 at 2/s,
// generous submit/admin, global capacity 100.
func testLimits() RateLimitOptions {
	return RateLimitOptions{
		KeyPrefix: "test:rl",
		Submit:    ClassLimit{Capacity: 50, RefillPerSec: 10},
		Read:      ClassLimit{Capacity: 3, RefillPerSec: 2},
		Admin:     ClassLimit{Capacity: 50, RefillPerSec: 10},
		Global:    ClassLimit{Capacity: 100, RefillPerSec: 50},
	}
}

// rateLimitedServer boots the full router (nil DB pool — the probe routes
// below fail their input validation before any store access) with the
// given acquirer, authenticating via the root credential. Constructed, not
// a literal: no sk_-shaped string may be committed verbatim.
func rateLimitedServer(t *testing.T, opts RateLimitOptions) (*httptest.Server, string) {
	t.Helper()
	rootKey := "sk_" + strings.Repeat("a", 43)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(store.NewFromPool(nil), time.Now, discard, rootKey, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, rootKey
}

// decodeBody decodes the response body into out.
func decodeBody(t *testing.T, res *http.Response, out any) {
	t.Helper()
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
}

// readProbe GETs /v1/runs/not-a-uuid — through auth and the read-class
// rate limit, 400 in the handler before any store access — and returns
// the response. An empty bearer sends no Authorization header.
func readProbe(t *testing.T, srv *httptest.Server, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/runs/not-a-uuid", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { res.Body.Close() }) //nolint:errcheck // test cleanup
	return res
}

func TestRateLimitExactThreshold(t *testing.T) {
	t.Parallel()
	acq := &fakeAcquirer{retryAfter: 1500 * time.Millisecond}
	opts := testLimits()
	opts.Acquirer = acq
	srv, rootKey := rateLimitedServer(t, opts)

	// Read capacity is 3: requests 1..3 reach the handler (400 — bad
	// UUID) with Remaining counting down; request 4 is 429 exactly at the
	// threshold.
	for i, wantRemaining := range []string{"2", "1", "0"} {
		res := readProbe(t, srv, rootKey)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("request %d: status = %d, want 400", i+1, res.StatusCode)
		}
		if got := res.Header.Get("X-RateLimit-Limit"); got != "3" {
			t.Errorf("request %d: X-RateLimit-Limit = %q, want 3", i+1, got)
		}
		if got := res.Header.Get("X-RateLimit-Remaining"); got != wantRemaining {
			t.Errorf("request %d: X-RateLimit-Remaining = %q, want %q", i+1, got, wantRemaining)
		}
	}
	res := readProbe(t, srv, rootKey)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-threshold request: status = %d, want 429", res.StatusCode)
	}
	if got := res.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("429 X-RateLimit-Remaining = %q, want 0", got)
	}
	// 1500ms rounds up: a client honoring the header never retries early.
	if got := res.Header.Get("Retry-After"); got != "2" {
		t.Errorf("429 Retry-After = %q, want 2", got)
	}
	// 3 tokens missing at 2/s → ceil(1.5s) = 2.
	if got := res.Header.Get("X-RateLimit-Reset"); got != "2" {
		t.Errorf("429 X-RateLimit-Reset = %q, want 2", got)
	}
	var body ErrorBody
	decodeBody(t, res, &body)
	if body.Error.Code != ErrCodeRateLimited {
		t.Errorf("429 error code = %q, want %q", body.Error.Code, ErrCodeRateLimited)
	}
}

func TestRateLimitGlobalBucket(t *testing.T) {
	t.Parallel()
	acq := &fakeAcquirer{retryAfter: 400 * time.Millisecond}
	opts := testLimits()
	opts.Read = ClassLimit{Capacity: 100, RefillPerSec: 50}
	opts.Global = ClassLimit{Capacity: 2, RefillPerSec: 1}
	opts.Acquirer = acq
	srv, rootKey := rateLimitedServer(t, opts)

	// The per-key bucket has plenty; the global bucket runs dry after 2.
	for i := 0; i < 2; i++ {
		if res := readProbe(t, srv, rootKey); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("request %d: status = %d, want 400", i+1, res.StatusCode)
		}
	}
	res := readProbe(t, srv, rootKey)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("global-exhausted request: status = %d, want 429", res.StatusCode)
	}
	// Headers keep describing the caller's own class bucket (the per-key
	// token was spent before the global denial — the documented cost of
	// per-key-first ordering).
	if got := res.Header.Get("X-RateLimit-Remaining"); got != "97" {
		t.Errorf("429 X-RateLimit-Remaining = %q, want 97 (per-key state)", got)
	}
	if got := res.Header.Get("Retry-After"); got != "1" {
		t.Errorf("429 Retry-After = %q, want 1 (from the global bucket)", got)
	}
	var body ErrorBody
	decodeBody(t, res, &body)
	if body.Error.Code != ErrCodeRateLimited {
		t.Errorf("429 error code = %q, want %q", body.Error.Code, ErrCodeRateLimited)
	}
	if !strings.Contains(body.Error.Message, "API-wide") {
		t.Errorf("429 message %q does not name the global limit", body.Error.Message)
	}
}

func TestRateLimitBucketKeysAndClassIsolation(t *testing.T) {
	t.Parallel()
	acq := &fakeAcquirer{}
	opts := testLimits()
	opts.Acquirer = acq
	srv, rootKey := rateLimitedServer(t, opts)

	// Exhaust the read class, then prove submit still serves: classes are
	// separate buckets under the same key_id.
	for i := 0; i < 3; i++ {
		if res := readProbe(t, srv, rootKey); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("read %d: status = %d, want 400", i+1, res.StatusCode)
		}
	}
	if res := readProbe(t, srv, rootKey); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("read over threshold: status = %d, want 429", res.StatusCode)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/runs", strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+rootKey)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close() //nolint:errcheck // test cleanup
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("submit with read exhausted: status = %d, want 400 (separate bucket)", res.StatusCode)
	}

	// The recorded keys pin the ADR-007 naming: <prefix>:<key_id>:<class>
	// per key, <prefix>:global for the safety bucket.
	acq.mu.Lock()
	defer acq.mu.Unlock()
	wantKeys := map[string]bool{
		"test:rl:root:read":   true,
		"test:rl:root:submit": true,
		"test:rl:global":      true,
	}
	seen := map[string]bool{}
	for _, k := range acq.calls {
		if !wantKeys[k] {
			t.Errorf("unexpected bucket key %q", k)
		}
		seen[k] = true
	}
	for k := range wantKeys {
		if !seen[k] {
			t.Errorf("bucket key %q never acquired", k)
		}
	}
}

func TestRateLimitFailOpen(t *testing.T) {
	t.Parallel()
	metrics := &recordingMetrics{}
	acq := &fakeAcquirer{err: errors.New("redis is down")}
	opts := testLimits()
	opts.Acquirer = acq
	opts.Metrics = metrics
	srv, rootKey := rateLimitedServer(t, opts)

	res := readProbe(t, srv, rootKey)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — an acquire error must fail open", res.StatusCode)
	}
	if got := res.Header.Get("X-RateLimit-Limit"); got != "" {
		t.Errorf("fail-open response carries X-RateLimit-Limit %q, want none", got)
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if len(metrics.failOpens) != 1 || metrics.failOpens[0] != "read" {
		t.Errorf("FailOpen hooks = %v, want exactly [read]", metrics.failOpens)
	}
}

func TestRateLimitDisabled(t *testing.T) {
	t.Parallel()
	srv, rootKey := rateLimitedServer(t, RateLimitOptions{})

	for i := 0; i < 10; i++ {
		res := readProbe(t, srv, rootKey)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("request %d: status = %d, want 400 (no limiting)", i+1, res.StatusCode)
		}
		if got := res.Header.Get("X-RateLimit-Limit"); got != "" {
			t.Errorf("disabled mode set X-RateLimit-Limit %q", got)
		}
	}
}

func TestRateLimitUnauthorizedConsumesNothing(t *testing.T) {
	t.Parallel()
	acq := &fakeAcquirer{}
	opts := testLimits()
	opts.Acquirer = acq
	srv, _ := rateLimitedServer(t, opts)

	// Credential failures answer before the middleware: no header at all
	// and a malformed bearer both 401 without an acquire. (Both fail
	// before any store read too, which is what lets this test run on a
	// nil pool.)
	for _, bearer := range []string{"", "sk_not-a-real-key"} {
		res := readProbe(t, srv, bearer)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", res.StatusCode)
		}
	}
	if n := acq.callCount(); n != 0 {
		t.Errorf("unauthorized requests performed %d acquires, want 0", n)
	}
}

func TestRateLimitMetricsDecisions(t *testing.T) {
	t.Parallel()
	metrics := &recordingMetrics{}
	acq := &fakeAcquirer{}
	opts := testLimits()
	opts.Read = ClassLimit{Capacity: 1, RefillPerSec: 1}
	opts.Acquirer = acq
	opts.Metrics = metrics
	srv, rootKey := rateLimitedServer(t, opts)

	readProbe(t, srv, rootKey) // allowed: per-key + global decisions
	readProbe(t, srv, rootKey) // denied per-key: no global decision

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	want := []string{"read/key/allowed", "read/global/allowed", "read/key/denied"}
	if len(metrics.decisions) != len(want) {
		t.Fatalf("decisions = %v, want %v", metrics.decisions, want)
	}
	for i := range want {
		if metrics.decisions[i] != want[i] {
			t.Errorf("decision[%d] = %q, want %q", i, metrics.decisions[i], want[i])
		}
	}
}

func TestNewRejectsInvalidRateLimitOptions(t *testing.T) {
	t.Parallel()
	base := func() RateLimitOptions {
		o := testLimits()
		o.Acquirer = &fakeAcquirer{}
		return o
	}
	for name, mutate := range map[string]func(*RateLimitOptions){
		"empty key prefix":     func(o *RateLimitOptions) { o.KeyPrefix = "" },
		"zero capacity":        func(o *RateLimitOptions) { o.Read.Capacity = 0 },
		"negative capacity":    func(o *RateLimitOptions) { o.Global.Capacity = -1 },
		"zero refill":          func(o *RateLimitOptions) { o.Submit.RefillPerSec = 0 },
		"negative refill":      func(o *RateLimitOptions) { o.Admin.RefillPerSec = -2 },
		"non-finite refill":    func(o *RateLimitOptions) { o.Read.RefillPerSec = math.Inf(1) },
		"NaN refill":           func(o *RateLimitOptions) { o.Read.RefillPerSec = math.NaN() },
		"zero global capacity": func(o *RateLimitOptions) { o.Global.Capacity = 0 },
	} {
		opts := base()
		mutate(&opts)
		if _, err := New(store.NewFromPool(nil), time.Now, nil, "", opts); err == nil {
			t.Errorf("%s: New accepted invalid rate-limit options", name)
		}
	}
	// The zero value stays valid: it means disabled.
	if _, err := New(store.NewFromPool(nil), time.Now, nil, "", RateLimitOptions{}); err != nil {
		t.Errorf("zero options rejected: %v", err)
	}
}

func TestResetSeconds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		limit     ClassLimit
		remaining int64
		want      int64
	}{
		{ClassLimit{Capacity: 10, RefillPerSec: 2}, 10, 0}, // full
		{ClassLimit{Capacity: 10, RefillPerSec: 2}, 4, 3},  // 6 missing at 2/s
		{ClassLimit{Capacity: 10, RefillPerSec: 4}, 9, 1},  // sub-second rounds up
		{ClassLimit{Capacity: 3, RefillPerSec: 0.5}, 0, 6}, // fractional rate
	} {
		if got := resetSeconds(tc.limit, tc.remaining); got != tc.want {
			t.Errorf("resetSeconds(%+v, %d) = %d, want %d", tc.limit, tc.remaining, got, tc.want)
		}
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		d    time.Duration
		want int64
	}{
		{100 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{ratelimit.RetryAfterNever, 1}, // defensive clamp; unreachable via config validation
	} {
		if got := retryAfterSeconds(tc.d); got != tc.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", tc.d, got, tc.want)
		}
	}
}
