//go:build integration

package api_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 6.1's integration suite: key management over a real store —
// mint/list/revoke through the API, the admin gate's 401/403 semantics,
// and the secret-hygiene criterion (plaintext never persisted or logged).

// mintTestKey fabricates a syntactically valid key at runtime. Never a
// literal: the CI secret grep flags committed sk_-shaped strings.
func mintTestKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("reading entropy: %v", err)
	}
	return "sk_" + base64.RawURLEncoding.EncodeToString(raw)
}

// syncBuffer is a goroutine-safe log sink (the server handles requests
// on its own goroutines).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// keysServer boots the API with a root key, a mutable clock, and a
// captured log stream.
func keysServer(t *testing.T, rootKey string) (*store.Store, *httptest.Server, *time.Time, *syncBuffer) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t))
	now := testNow
	logs := &syncBuffer{}
	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, logs)
	h, err := api.New(s, func() time.Time { return now }, logger, rootKey, api.RateLimitOptions{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return s, srv, &now, logs
}

// doAuth runs one request with an optional bearer credential, decoding a
// JSON body into out (may be nil) and returning the response.
func doAuth(t *testing.T, srv *httptest.Server, method, path, bearer string, body []byte, out any) *http.Response {
	t.Helper()
	return doAuthHdr(t, srv, method, path, bearer, nil, body, out)
}

// doAuthHdr is doAuth with extra request headers (e.g. Idempotency-Key).
func doAuthHdr(t *testing.T, srv *httptest.Server, method, path, bearer string, headers map[string]string, body []byte, out any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close() //nolint:errcheck // read-side close
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decoding %s %s response: %v", method, path, err)
		}
	}
	return res
}

// createKey mints a key through the API, asserting success.
func createKey(t *testing.T, srv *httptest.Server, bearer string, req api.CreateKeyRequest) api.CreateKeyResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling create request: %v", err)
	}
	var resp api.CreateKeyResponse
	res := doAuth(t, srv, http.MethodPost, "/v1/keys", bearer, body, &resp)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/keys = %d, want 201 (resp %+v)", res.StatusCode, resp)
	}
	return resp
}

func TestKeysRequireAdminCredential(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	_, srv, _, _ := keysServer(t, rootKey)

	// Every keys route rejects a missing credential with the envelope and
	// the RFC 6750 challenge header.
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/v1/keys"},
		{http.MethodPost, "/v1/keys"},
		{http.MethodDelete, "/v1/keys/00000000-0000-0000-0000-000000000000"},
	} {
		var envelope api.ErrorBody
		res := doAuth(t, srv, probe.method, probe.path, "", nil, &envelope)
		if res.StatusCode != http.StatusUnauthorized || envelope.Error.Code != api.ErrCodeUnauthorized {
			t.Errorf("%s %s without credential = %d/%q, want 401/unauthorized",
				probe.method, probe.path, res.StatusCode, envelope.Error.Code)
		}
		if res.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s %s: missing WWW-Authenticate challenge", probe.method, probe.path)
		}
	}

	// Malformed and unknown-but-well-shaped credentials are the same 401.
	for name, bearer := range map[string]string{
		"garbage":     "not-a-key",
		"unknown key": mintTestKey(t),
	} {
		var envelope api.ErrorBody
		res := doAuth(t, srv, http.MethodGet, "/v1/keys", bearer, nil, &envelope)
		if res.StatusCode != http.StatusUnauthorized || envelope.Error.Code != api.ErrCodeUnauthorized {
			t.Errorf("%s credential = %d/%q, want 401/unauthorized", name, res.StatusCode, envelope.Error.Code)
		}
	}

	// A valid key without the admin scope is 403, not 401 — and the body
	// names the missing scope.
	limited := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "limited", Scopes: []string{"submit", "read"}})
	var envelope api.ErrorBody
	res := doAuth(t, srv, http.MethodGet, "/v1/keys", limited.Key, nil, &envelope)
	if res.StatusCode != http.StatusForbidden || envelope.Error.Code != api.ErrCodeForbidden {
		t.Fatalf("non-admin key on /v1/keys = %d/%q, want 403/forbidden", res.StatusCode, envelope.Error.Code)
	}
	if !strings.Contains(envelope.Error.Message, "admin") {
		t.Errorf("403 message %q does not name the missing scope", envelope.Error.Message)
	}
}

func TestKeysLifecycleRoundTrip(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	s, srv, now, logs := keysServer(t, rootKey)

	// Root mints a real admin key; the response carries the plaintext and
	// its derived projection.
	admin := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "ops-admin", Scopes: []string{"admin"}})
	if !strings.HasPrefix(admin.Key, "sk_") || len(admin.Key) != 46 {
		t.Fatalf("minted key %q does not have the sk_ shape", admin.Key)
	}
	if admin.Prefix != admin.Key[:11] {
		t.Errorf("prefix %q is not the key's first 11 chars", admin.Prefix)
	}
	if admin.ExpiresAt != nil {
		t.Errorf("no-TTL key got an expiry: %v", admin.ExpiresAt)
	}

	// The stored admin key works for management; TTL resolves against the
	// server clock.
	scoped := createKey(t, srv, admin.Key, api.CreateKeyRequest{Name: "ci", Scopes: []string{"submit", "read"}, TTL: "24h"})
	if scoped.ExpiresAt == nil || !scoped.ExpiresAt.Equal(testNow.Add(24*time.Hour)) {
		t.Errorf("expires_at = %v, want created_at+24h", scoped.ExpiresAt)
	}

	// Listing shows both keys, prefixes only — never plaintext or hashes.
	var list api.ListKeysResponse
	if res := doAuth(t, srv, http.MethodGet, "/v1/keys", admin.Key, nil, &list); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/keys = %d, want 200", res.StatusCode)
	}
	if len(list.Keys) != 2 {
		t.Fatalf("listed %d keys, want 2", len(list.Keys))
	}
	rawList, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("re-marshaling list: %v", err)
	}
	for _, secret := range []string{rootKey, admin.Key, scoped.Key} {
		if strings.Contains(string(rawList), secret) {
			t.Fatal("listing leaked a plaintext key")
		}
	}

	// Revoke is a 204, idempotently.
	for i := 0; i < 2; i++ {
		res := doAuth(t, srv, http.MethodDelete, "/v1/keys/"+scoped.ID, admin.Key, nil, nil)
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE (attempt %d) = %d, want 204", i+1, res.StatusCode)
		}
	}
	// The revoked key no longer authenticates — and the answer is the
	// indistinguishable 401, not 403.
	if res := doAuth(t, srv, http.MethodGet, "/v1/keys", scoped.Key, nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked key = %d, want 401", res.StatusCode)
	}

	// Expiry: a short-TTL key stops authenticating once the clock passes
	// expires_at (the mutable clock stands in for elapsed time).
	ephemeral := createKey(t, srv, admin.Key, api.CreateKeyRequest{Name: "ephemeral", Scopes: []string{"admin"}, TTL: "1h"})
	if res := doAuth(t, srv, http.MethodGet, "/v1/keys", ephemeral.Key, nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("fresh TTL key = %d, want 200", res.StatusCode)
	}
	*now = testNow.Add(time.Hour) // exactly at expires_at: expired (not-before semantics)
	if res := doAuth(t, srv, http.MethodGet, "/v1/keys", ephemeral.Key, nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired key = %d, want 401", res.StatusCode)
	}
	*now = testNow

	// Secret hygiene (the ticket's headline criterion): no plaintext in
	// any api_keys column, and none in anything the server logged.
	rows, err := s.APIKeys().List(t.Context())
	if err != nil {
		t.Fatalf("listing rows: %v", err)
	}
	for _, row := range rows {
		for _, secret := range []string{rootKey, admin.Key, scoped.Key, ephemeral.Key} {
			for col, v := range map[string]string{"name": row.Name, "prefix": row.Prefix, "key_hash": row.KeyHash} {
				if strings.Contains(v, secret) {
					t.Fatalf("plaintext key persisted in api_keys.%s", col)
				}
			}
		}
	}
	logged := logs.String()
	if logged == "" {
		t.Fatal("no logs captured — the hygiene assertion below would be vacuous")
	}
	for _, secret := range []string{rootKey, admin.Key, scoped.Key, ephemeral.Key} {
		if strings.Contains(logged, secret) {
			t.Fatal("plaintext key appeared in the server logs")
		}
	}
	// Positive control: the create path logged, with key material limited
	// to the prefix.
	if !strings.Contains(logged, admin.Prefix) {
		t.Error("expected the created key's prefix in the logs (the allowed correlation handle)")
	}
}

func TestKeysValidation(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	_, srv, _, _ := keysServer(t, rootKey)

	badBodies := map[string]api.CreateKeyRequest{
		"empty name":    {Name: "  ", Scopes: []string{"read"}},
		"no scopes":     {Name: "x"},
		"unknown scope": {Name: "x", Scopes: []string{"root"}},
		"duplicate":     {Name: "x", Scopes: []string{"read", "read"}},
		"bad ttl":       {Name: "x", Scopes: []string{"read"}, TTL: "soon"},
		"negative ttl":  {Name: "x", Scopes: []string{"read"}, TTL: "-1h"},
		"name too long": {Name: strings.Repeat("n", 201), Scopes: []string{"read"}},
	}
	for name, req := range badBodies {
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshaling %s: %v", name, err)
		}
		var envelope api.ErrorBody
		res := doAuth(t, srv, http.MethodPost, "/v1/keys", rootKey, body, &envelope)
		if res.StatusCode != http.StatusBadRequest || envelope.Error.Code != api.ErrCodeInvalidRequest {
			t.Errorf("%s = %d/%q, want 400/invalid_request", name, res.StatusCode, envelope.Error.Code)
		}
	}

	// Revocation misses: bad UUID is a 400, unknown UUID a 404.
	var envelope api.ErrorBody
	res := doAuth(t, srv, http.MethodDelete, "/v1/keys/not-a-uuid", rootKey, nil, &envelope)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE bad uuid = %d, want 400", res.StatusCode)
	}
	envelope = api.ErrorBody{}
	res = doAuth(t, srv, http.MethodDelete, "/v1/keys/00000000-0000-0000-0000-000000000001", rootKey, nil, &envelope)
	if res.StatusCode != http.StatusNotFound || envelope.Error.Code != api.ErrCodeKeyNotFound {
		t.Errorf("DELETE unknown uuid = %d/%q, want 404/key_not_found", res.StatusCode, envelope.Error.Code)
	}
}
