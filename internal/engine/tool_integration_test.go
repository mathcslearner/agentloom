//go:build integration

package engine_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/tools"
)

// toolReg builds the built-in tool registry for the engine tests, with the
// given allowlist and the default client.
func toolReg(t *testing.T, allowlist ...string) *tools.Registry {
	t.Helper()
	reg, err := tools.NewBuiltins(tools.HTTPOptions{Allowlist: allowlist})
	if err != nil {
		t.Fatalf("tools.NewBuiltins: %v", err)
	}
	return reg
}

// TestHTTPToolIdempotencyKeyAcrossRetries is ticket 8.7's headline
// acceptance: an http_request POST step whose server fails once (500 →
// transient) then succeeds retries per the M5 policy, and the SAME stable
// Idempotency-Key rides both attempts — the exactly-once guarantee an
// idempotency-aware server uses to deduplicate the retried effect.
func TestHTTPToolIdempotencyKeyAcrossRetries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	var mu sync.Mutex
	var keys []string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError) // first attempt fails transient
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	def := fmt.Sprintf(`{
		"schema_version": 1,
		"name": "http-retry",
		"steps": [
			{"id": "call", "type": "tool",
			 "config": {"tool": "http_request", "input": {"method": "POST", "url": %q, "body": {"hello": "world"}}},
			 "retry": {"max_attempts": 3, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}},
			{"id": "after", "type": "noop"}
		],
		"edges": [{"from": "call", "to": "after"}]
	}`, srv.URL)

	s, h, runID := setup(t, def)
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())

	reg, err := exec.NewRegistry(exec.NewToolExecutor(toolReg(t, hostOf(t, srv.URL))), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	// Attempt 1's 500 classifies transient and schedules retry 1 at t0+1s.
	due1 := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "call", due1)

	clk.Set(due1)
	if res := h.PromoteDue(ctx, due1, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries at due time, want 1", res.Promoted)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"call": store.StepStatusSucceeded, "after": store.StepStatusSucceeded,
	})
	requireAttemptOutcomes(t, s, runID, "call", []string{
		store.AttemptOutcomeTransient, store.StepStatusSucceeded,
	})

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("server saw %d requests, want 2 (one per attempt)", len(keys))
	}
	if keys[0] == "" {
		t.Error("no Idempotency-Key on the POST retry")
	}
	if keys[0] != keys[1] {
		t.Errorf("Idempotency-Key differed across attempts: %q vs %q — must be stable", keys[0], keys[1])
	}
}

// TestHTTPToolAllowlistDeadLetters is ticket 8.7's SSRF acceptance: a
// step targeting a host off the allowlist fails permanent and dead-letters
// (source permanent) without any request — the guard is not a retryable
// condition.
func TestHTTPToolAllowlistDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const def = `{
		"schema_version": 1,
		"name": "http-blocked",
		"on_failure": "fail_fast",
		"steps": [
			{"id": "call", "type": "tool",
			 "config": {"tool": "http_request", "input": {"method": "GET", "url": "https://blocked.example.com/data"}}},
			{"id": "after", "type": "noop"}
		],
		"edges": [{"from": "call", "to": "after"}]
	}`

	s, h, runID := setup(t, def)
	d := startDispatcher(t, s, h.Queue())

	// Allowlist deliberately omits blocked.example.com.
	reg, err := exec.NewRegistry(exec.NewToolExecutor(toolReg(t, "allowed.example.com")), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	// fail_fast fails the run immediately; the downstream noop stays
	// pending (never written off).
	requireStepStatuses(t, s, runID, map[string]string{
		"call": store.StepStatusDeadLettered, "after": store.StepStatusPending,
	})

	dls, err := s.DeadLetters().ListByStep(ctx, runID, "call")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dls))
	}
	if dls[0].Source != store.DeadLetterSourcePermanent {
		t.Errorf("dead letter source = %q, want permanent", dls[0].Source)
	}
	// The step failed on its first attempt — no retries for a permanent
	// allowlist block.
	requireAttemptOutcomes(t, s, runID, "call", []string{store.AttemptOutcomePermanent})
}

// hostOf returns a URL's host for the allowlist.
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	return u.Host
}
