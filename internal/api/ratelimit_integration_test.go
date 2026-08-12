//go:build integration

package api_test

// Ticket 6.4's integration suite: the rate-limit middleware over the real
// Redis limiter (internal/ratelimit) and a real store. Headline: a key
// driven to its limit hits 429 exactly at the threshold and recovers
// after refill; the global safety bucket throttles even when every
// individual key is under its own limit.
//
// Timing note: the limiter deliberately reads the Redis server clock
// (ADR-007/6.3), and its fake-time seam is sealed inside the ratelimit
// package — so these tests use small real-time refill rates and bounded
// polling instead of an injected clock. Bursts complete orders of
// magnitude faster than the ≥ 2s-per-token refills configured here, so
// threshold exactness is not timing-fragile.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/ratelimit"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

const rlOpTimeout = 30 * time.Second

// generousLimits is the baseline geometry: every class roomy enough that
// only the bucket a test deliberately shrinks can deny.
func generousLimits() api.RateLimitOptions {
	return api.RateLimitOptions{
		Submit: api.ClassLimit{Capacity: 1000, RefillPerSec: 500},
		Read:   api.ClassLimit{Capacity: 1000, RefillPerSec: 500},
		Admin:  api.ClassLimit{Capacity: 1000, RefillPerSec: 500},
		Global: api.ClassLimit{Capacity: 1000, RefillPerSec: 500},
	}
}

// rlServer boots the API over a fresh database and the test Redis with a
// uniquely prefixed bucket namespace (deleted on cleanup — the queuetest
// isolation discipline), returning the server, the root credential, and
// the captured log stream. tweak adjusts the generous baseline.
func rlServer(t *testing.T, tweak func(*api.RateLimitOptions)) (*httptest.Server, string, *syncBuffer) {
	t.Helper()
	addr := os.Getenv(queuetest.EnvTestRedisAddr)
	if addr == "" {
		addr = "localhost:6379"
	}
	ctx, cancel := context.WithTimeout(context.Background(), rlOpTimeout)
	defer cancel()
	client, err := queue.Open(ctx, addr)
	if err != nil {
		t.Fatalf("cannot reach Redis at %s (is the dev stack running? try `make up`): %v", addr, err)
	}
	t.Cleanup(func() { client.Close() }) //nolint:errcheck // best-effort cleanup

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generating key prefix: %v", err)
	}
	prefix := "agentloom-test:" + hex.EncodeToString(b[:]) + ":ratelimit:api"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), rlOpTimeout)
		defer cancel()
		iter := client.Scan(ctx, 0, prefix+":*", 100).Iterator()
		for iter.Next(ctx) {
			if err := client.Del(ctx, iter.Val()).Err(); err != nil {
				t.Errorf("deleting bucket key %s: %v", iter.Val(), err)
			}
		}
		if err := iter.Err(); err != nil {
			t.Errorf("scanning bucket keys: %v", err)
		}
	})

	opts := generousLimits()
	if tweak != nil {
		tweak(&opts)
	}
	opts.Acquirer = ratelimit.New(client)
	opts.KeyPrefix = prefix

	s := store.NewFromPool(storetest.NewDB(t))
	rootKey := mintTestKey(t)
	logs := &syncBuffer{}
	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, logs)
	h, err := api.New(s, time.Now, logger, rootKey, opts)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, rootKey, logs
}

// readPath is a fresh read-class probe path: a well-formed run id that
// exists in no database, answering 404 after passing auth and rate
// limiting.
func readPath() string {
	return "/v1/runs/" + uuid.NewString()
}

// TestRateLimitKeyToLimitAndRecovery is the headline AC: a key driven to
// its read limit sees 429 exactly at the threshold with the contract
// headers, and recovers once the bucket has refilled.
func TestRateLimitKeyToLimitAndRecovery(t *testing.T) {
	t.Parallel()
	// 1 token per 2s: the 4-request burst finishes far inside one refill
	// interval, so the threshold is exact; recovery lands within ~2s.
	srv, rootKey, logs := rlServer(t, func(o *api.RateLimitOptions) {
		o.Read = api.ClassLimit{Capacity: 4, RefillPerSec: 0.5}
	})
	client := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "limited", Scopes: []string{"read"}})

	for i, wantRemaining := range []string{"3", "2", "1", "0"} {
		res := doAuth(t, srv, http.MethodGet, readPath(), client.Key, nil, nil)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("burst request %d: status = %d, want 404", i+1, res.StatusCode)
		}
		if got := res.Header.Get("X-RateLimit-Limit"); got != "4" {
			t.Errorf("burst request %d: X-RateLimit-Limit = %q, want 4", i+1, got)
		}
		if got := res.Header.Get("X-RateLimit-Remaining"); got != wantRemaining {
			t.Errorf("burst request %d: X-RateLimit-Remaining = %q, want %q", i+1, got, wantRemaining)
		}
		if res.Header.Get("X-RateLimit-Reset") == "" {
			t.Errorf("burst request %d: X-RateLimit-Reset missing", i+1)
		}
	}

	var body api.ErrorBody
	res := doAuth(t, srv, http.MethodGet, readPath(), client.Key, nil, &body)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-threshold request: status = %d, want 429", res.StatusCode)
	}
	if body.Error.Code != api.ErrCodeRateLimited {
		t.Errorf("429 error code = %q, want %q", body.Error.Code, api.ErrCodeRateLimited)
	}
	if got := res.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("429 X-RateLimit-Remaining = %q, want 0", got)
	}
	retryAfter := res.Header.Get("Retry-After")
	if retryAfter == "" || retryAfter == "0" {
		t.Errorf("429 Retry-After = %q, want a positive whole-second count", retryAfter)
	}

	// Recovery: within one refill interval (plus slack) a request passes
	// again. Poll rather than sleep-once so a slow refill can't flake.
	deadline := time.Now().Add(15 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		res := doAuth(t, srv, http.MethodGet, readPath(), client.Key, nil, nil)
		if res.StatusCode == http.StatusNotFound {
			recovered = true
			break
		}
		if res.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("recovery poll: status = %d, want 404 or 429", res.StatusCode)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("bucket never recovered after refill")
	}

	// Deny logging discipline: the 429 line carries key_id and never key
	// material (the lookup prefix is fine and expected elsewhere).
	if !strings.Contains(logs.String(), "request rate limited") {
		t.Error("logs missing the rate-limited line")
	}
	if strings.Contains(logs.String(), client.Key) {
		t.Error("logs contain the plaintext API key")
	}
}

// TestRateLimitGlobalBucketProtects is the second AC: the global safety
// bucket answers 429 even when every individual key is under its own
// limit.
func TestRateLimitGlobalBucketProtects(t *testing.T) {
	t.Parallel()
	srv, rootKey, _ := rlServer(t, func(o *api.RateLimitOptions) {
		o.Global = api.ClassLimit{Capacity: 3, RefillPerSec: 0.5}
	})
	a := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "a", Scopes: []string{"read"}})
	b := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "b", Scopes: []string{"read"}})

	// Key management consumed global tokens too (admin shares the global
	// bucket) — wait one full capacity's refill time (3 / 0.5 per s = 6s,
	// plus slack) so the measured burst starts from a provably full
	// bucket no matter what setup spent. A deliberate real-time wait: the
	// limiter's clock is Redis's, unreachable from here by design.
	time.Sleep(7 * time.Second)

	// Alternate keys: 3 grants drain the global bucket while each key
	// stays far under its 1000-capacity read bucket.
	bearers := []string{a.Key, b.Key, a.Key}
	for i, bearer := range bearers {
		res := doAuth(t, srv, http.MethodGet, readPath(), bearer, nil, nil)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("request %d: status = %d, want 404", i+1, res.StatusCode)
		}
	}
	var body api.ErrorBody
	res := doAuth(t, srv, http.MethodGet, readPath(), b.Key, nil, &body)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("global-exhausted request: status = %d, want 429", res.StatusCode)
	}
	if body.Error.Code != api.ErrCodeRateLimited {
		t.Errorf("429 error code = %q, want %q", body.Error.Code, api.ErrCodeRateLimited)
	}
	if !strings.Contains(body.Error.Message, "API-wide") {
		t.Errorf("429 message %q does not name the global limit", body.Error.Message)
	}
	// The denied caller's own bucket is barely touched: headers describe
	// the per-key class bucket, not the global one.
	if got := res.Header.Get("X-RateLimit-Limit"); got != "1000" {
		t.Errorf("429 X-RateLimit-Limit = %q, want 1000 (per-key bucket)", got)
	}
}

// TestRateLimitPerKeyIsolation: one key exhausting its bucket must not
// throttle another.
func TestRateLimitPerKeyIsolation(t *testing.T) {
	t.Parallel()
	srv, rootKey, _ := rlServer(t, func(o *api.RateLimitOptions) {
		o.Read = api.ClassLimit{Capacity: 2, RefillPerSec: 0.5}
	})
	a := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "noisy", Scopes: []string{"read"}})
	b := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "quiet", Scopes: []string{"read"}})

	for i := 0; i < 2; i++ {
		if res := doAuth(t, srv, http.MethodGet, readPath(), a.Key, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("key A request %d: status = %d, want 404", i+1, res.StatusCode)
		}
	}
	if res := doAuth(t, srv, http.MethodGet, readPath(), a.Key, nil, nil); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("key A over threshold: status = %d, want 429", res.StatusCode)
	}
	for i := 0; i < 2; i++ {
		if res := doAuth(t, srv, http.MethodGet, readPath(), b.Key, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("key B request %d: status = %d, want 404 — key A's exhaustion leaked", i+1, res.StatusCode)
		}
	}
}

// TestRateLimitRootKeyLimited: the env root credential is rate limited
// like any other caller, bucketed under key_id "root".
func TestRateLimitRootKeyLimited(t *testing.T) {
	t.Parallel()
	srv, rootKey, _ := rlServer(t, func(o *api.RateLimitOptions) {
		o.Admin = api.ClassLimit{Capacity: 2, RefillPerSec: 0.5}
	})

	for i := 0; i < 2; i++ {
		if res := doAuth(t, srv, http.MethodGet, "/v1/keys", rootKey, nil, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("root list %d: status = %d, want 200", i+1, res.StatusCode)
		}
	}
	res := doAuth(t, srv, http.MethodGet, "/v1/keys", rootKey, nil, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("root over threshold: status = %d, want 429", res.StatusCode)
	}
}

// TestRateLimitCredentialFailuresConsumeNothing: 401s answer before the
// middleware, so a bad-credential storm cannot starve the caller's (or
// anyone's) buckets.
func TestRateLimitCredentialFailuresConsumeNothing(t *testing.T) {
	t.Parallel()
	srv, rootKey, _ := rlServer(t, func(o *api.RateLimitOptions) {
		o.Read = api.ClassLimit{Capacity: 3, RefillPerSec: 0.5}
	})
	client := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "victim", Scopes: []string{"read"}})

	// A storm of unknown (validly shaped, never minted) keys: uniform
	// 401s, no acquires.
	for i := 0; i < 10; i++ {
		if res := doAuth(t, srv, http.MethodGet, readPath(), mintTestKey(t), nil, nil); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("storm request %d: status = %d, want 401", i+1, res.StatusCode)
		}
	}
	// The real key's full burst still fits: nothing was consumed.
	for i, wantRemaining := range []string{"2", "1", "0"} {
		res := doAuth(t, srv, http.MethodGet, readPath(), client.Key, nil, nil)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("burst request %d: status = %d, want 404", i+1, res.StatusCode)
		}
		if got := res.Header.Get("X-RateLimit-Remaining"); got != wantRemaining {
			t.Errorf("burst request %d: X-RateLimit-Remaining = %q, want %q", i+1, got, wantRemaining)
		}
	}
}

// TestRateLimitSubmitClassOnRealSubmission: the submit class throttles
// real run submissions end to end (the unit suite proves class routing;
// this pins it against the production handler path).
func TestRateLimitSubmitClassOnRealSubmission(t *testing.T) {
	t.Parallel()
	srv, rootKey, _ := rlServer(t, func(o *api.RateLimitOptions) {
		o.Submit = api.ClassLimit{Capacity: 2, RefillPerSec: 0.5}
	})
	client := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "submitter", Scopes: []string{"submit", "read"}})

	def := []byte(`{"schema_version":1,"name":"rl-probe","steps":[{"id":"a","type":"noop"}],"edges":[]}`)
	for i := 0; i < 2; i++ {
		body := submitBody(t, def, "")
		res := doAuth(t, srv, http.MethodPost, "/v1/runs", client.Key, body, nil)
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("submit %d: status = %d, want 201", i+1, res.StatusCode)
		}
	}
	res := doAuth(t, srv, http.MethodPost, "/v1/runs", client.Key, submitBody(t, def, ""), nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("submit over threshold: status = %d, want 429", res.StatusCode)
	}
	// Reads keep flowing: class buckets are independent.
	if res := doAuth(t, srv, http.MethodGet, readPath(), client.Key, nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("read with submit exhausted: status = %d, want 404", res.StatusCode)
	}
}
