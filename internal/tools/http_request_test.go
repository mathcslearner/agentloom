package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// hostOf returns a server URL's host for the allowlist.
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	return u.Host
}

// httpArgsJSON builds an http_request args payload.
func httpArgsJSON(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshaling args: %v", err)
	}
	return b
}

func TestHTTPRequestSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]string{"echo": r.URL.Query().Get("q")})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, srv.URL)}})
	raw, err := tool.Invoke(context.Background(), Invocation{
		Args: httpArgsJSON(t, map[string]any{"method": "GET", "url": srv.URL + "?q=hi"}),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("output %s: %v", raw, err)
	}
	if out.Status != 200 {
		t.Errorf("status = %d, want 200", out.Status)
	}
	if string(out.Body) != `{"echo":"hi"}` {
		t.Errorf("body = %s, want the JSON object", out.Body)
	}
}

func TestHTTPRequestNonJSONBodyIsString(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("plain text"))
	}))
	defer srv.Close()

	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, srv.URL)}})
	raw, err := tool.Invoke(context.Background(), Invocation{Args: httpArgsJSON(t, map[string]any{"url": srv.URL})})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("output %s: %v", raw, err)
	}
	if string(out.Body) != `"plain text"` {
		t.Errorf("body = %s, want a JSON string", out.Body)
	}
}

func TestHTTPRequestIdempotencyKey(t *testing.T) {
	t.Parallel()

	var seenKey atomic.Value
	seenKey.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey.Store(r.Header.Get(idempotencyHeader))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, srv.URL)}})

	// POST carries the automatic Idempotency-Key.
	_, err := tool.Invoke(context.Background(), Invocation{
		Args:           httpArgsJSON(t, map[string]any{"method": "POST", "url": srv.URL, "body": map[string]any{"a": 1}}),
		IdempotencyKey: "stable-key-123",
	})
	if err != nil {
		t.Fatalf("POST Invoke: %v", err)
	}
	if got := seenKey.Load().(string); got != "stable-key-123" {
		t.Errorf("POST Idempotency-Key = %q, want stable-key-123", got)
	}

	// GET carries none, even with a key set.
	seenKey.Store("")
	_, err = tool.Invoke(context.Background(), Invocation{
		Args:           httpArgsJSON(t, map[string]any{"method": "GET", "url": srv.URL}),
		IdempotencyKey: "stable-key-123",
	})
	if err != nil {
		t.Fatalf("GET Invoke: %v", err)
	}
	if got := seenKey.Load().(string); got != "" {
		t.Errorf("GET Idempotency-Key = %q, want empty", got)
	}
}

func TestHTTPRequestStatusClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status    int
		transient bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusInternalServerError, true},
		{http.StatusNotFound, false},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, srv.URL)}})
			_, err := tool.Invoke(context.Background(), Invocation{Args: httpArgsJSON(t, map[string]any{"url": srv.URL})})
			if tc.transient {
				assertTransient(t, err)
			} else {
				assertPermanent(t, err)
			}
		})
	}
}

func TestHTTPRequestAllowlistBlocksWithoutRequest(t *testing.T) {
	t.Parallel()

	// A tripwire transport fails the test if any request is dispatched.
	var dispatched atomic.Int64
	tripwire := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return nil, errors.New("tripwire: request should never be sent")
	})}
	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{"allowed.example.com"}, Client: tripwire})

	_, err := tool.Invoke(context.Background(), Invocation{
		Args: httpArgsJSON(t, map[string]any{"url": "https://blocked.example.com/secret?token=abc"}),
	})
	var hostErr *HostNotAllowedError
	if !errors.As(err, &hostErr) {
		t.Fatalf("err = %v, want *HostNotAllowedError", err)
	}
	if hostErr.Class() != dag.ClassPermanent {
		t.Errorf("HostNotAllowedError class = %v, want permanent", hostErr.Class())
	}
	if hostErr.Host != "blocked.example.com" {
		t.Errorf("blocked host = %q, want blocked.example.com", hostErr.Host)
	}
	// The error must not leak the URL's query token.
	if strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaked URL detail: %v", err)
	}
	if n := dispatched.Load(); n != 0 {
		t.Errorf("dispatched %d requests, want 0 — the block must precede any connection", n)
	}
}

func TestHTTPRequestRedirectRevalidated(t *testing.T) {
	t.Parallel()

	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"reached":true}`))
	}))
	defer blocked.Close()
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blocked.URL, http.StatusFound)
	}))
	defer entry.Close()

	// Only the entry host is allowlisted; the redirect target is not.
	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, entry.URL)}})
	_, err := tool.Invoke(context.Background(), Invocation{Args: httpArgsJSON(t, map[string]any{"url": entry.URL})})
	var hostErr *HostNotAllowedError
	if !errors.As(err, &hostErr) {
		t.Fatalf("err = %v, want *HostNotAllowedError from the redirect hop", err)
	}
}

func TestHTTPRequestSchemeGuard(t *testing.T) {
	t.Parallel()
	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{"example.com"}})
	_, err := tool.Invoke(context.Background(), Invocation{Args: httpArgsJSON(t, map[string]any{"url": "ftp://example.com/x"})})
	assertPermanent(t, err)
}

func TestHTTPRequestSizeCapPermanent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()
	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, srv.URL)}, MaxResponseBytes: 1024})
	_, err := tool.Invoke(context.Background(), Invocation{Args: httpArgsJSON(t, map[string]any{"url": srv.URL})})
	assertPermanent(t, err)
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want a size-cap message", err)
	}
}

func TestHTTPRequestTimeoutTransient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, srv.URL)}})
	// A tiny per-request timeout trips while the parent ctx is alive.
	_, err := tool.Invoke(context.Background(), Invocation{
		Args: httpArgsJSON(t, map[string]any{"url": srv.URL, "timeout": "20ms"}),
	})
	assertTransient(t, err)
}

func TestHTTPRequestContextCancelPassthrough(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, srv.URL)}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := tool.Invoke(ctx, Invocation{Args: httpArgsJSON(t, map[string]any{"url": srv.URL})})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded to pass through", err)
	}
	var te *Error
	if errors.As(err, &te) {
		t.Errorf("context error wrapped in classified *Error: %v", err)
	}
}

func TestHTTPRequestSecretHygiene(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tool := NewHTTPRequest(HTTPOptions{Allowlist: []string{hostOf(t, srv.URL)}})
	_, err := tool.Invoke(context.Background(), Invocation{
		Args: httpArgsJSON(t, map[string]any{
			"method":  "POST",
			"url":     srv.URL,
			"headers": map[string]string{"Authorization": "Bearer super-secret-token"},
			"body":    map[string]any{"password": "hunter2"},
		}),
	})
	if err == nil {
		t.Fatal("want an error for 500")
	}
	for _, secret := range []string{"super-secret-token", "hunter2", "Bearer"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked %q: %v", secret, err)
		}
	}
}

func TestHTTPRequestMissingURLPermanent(t *testing.T) {
	t.Parallel()
	tool := NewHTTPRequest(HTTPOptions{})
	_, err := tool.Invoke(context.Background(), Invocation{Args: json.RawMessage(`{}`)})
	assertPermanent(t, err)
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
