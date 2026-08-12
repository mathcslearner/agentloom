//go:build integration

package api_test

// Ticket 6.2's integration suite: every /v1 route behind ADR-007's
// route→scope table — the table-driven credential matrix (401 uniformity,
// 403 with the named scope, success with the exact scope and via
// admin-implies-all), expiry collapsing to 401 on every route, the
// /healthz exemption, and the key_id-yes/key-material-never log
// discipline. The route list here mirrors routeScopes in
// auth_routes_test.go, whose walk-based coverage test catches routes
// added to the router but not to these tables.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
)

// probeDefJSON is a minimal one-noop definition for submit probes.
var probeDefJSON = []byte(`{"schema_version":1,"name":"auth-probe","steps":[{"id":"a","type":"noop"}],"edges":[]}`)

func TestV1AuthMatrix(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	_, srv, now, logs := keysServer(t, rootKey)

	// Fixtures: an admin key minted by root, a run for the GET probe, and
	// a victim key for the DELETE probe (revoke is idempotent, so every
	// successful probe answers 204).
	admin := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "admin", Scopes: []string{"admin"}})
	var sub api.SubmitRunResponse
	if res := doAuth(t, srv, http.MethodPost, "/v1/runs", admin.Key, submitBody(t, probeDefJSON, "", ""), &sub); res.StatusCode != http.StatusCreated {
		t.Fatalf("fixture submit = %d, want 201", res.StatusCode)
	}
	victim := createKey(t, srv, admin.Key, api.CreateKeyRequest{Name: "victim", Scopes: []string{"read"}})

	// Per-scope credentials: rightKey carries exactly the route's scope,
	// wrongKey carries every non-admin scope except it — so a 403 proves
	// the check is on the right scope, not on "any scope at all".
	nonAdmin := []string{"submit", "read", "approve"}
	rightKey := map[api.Scope]string{api.ScopeAdmin: admin.Key}
	wrongKey := map[api.Scope]string{}
	for _, scope := range []api.Scope{api.ScopeSubmit, api.ScopeRead} {
		rightKey[scope] = createKey(t, srv, admin.Key, api.CreateKeyRequest{
			Name: "right-" + string(scope), Scopes: []string{string(scope)},
		}).Key
		var others []string
		for _, s := range nonAdmin {
			if s != string(scope) {
				others = append(others, s)
			}
		}
		wrongKey[scope] = createKey(t, srv, admin.Key, api.CreateKeyRequest{
			Name: "wrong-" + string(scope), Scopes: others,
		}).Key
	}
	wrongKey[api.ScopeAdmin] = createKey(t, srv, admin.Key, api.CreateKeyRequest{
		Name: "wrong-admin", Scopes: nonAdmin,
	}).Key

	// A revoked and an expiring credential, both fully scoped — their
	// rejection must come from lifecycle state, never from scopes.
	revoked := createKey(t, srv, admin.Key, api.CreateKeyRequest{Name: "revoked", Scopes: []string{"admin"}})
	if res := doAuth(t, srv, http.MethodDelete, "/v1/keys/"+revoked.ID, admin.Key, nil, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking fixture key = %d, want 204", res.StatusCode)
	}
	expiring := createKey(t, srv, admin.Key, api.CreateKeyRequest{Name: "expiring", Scopes: []string{"admin"}, TTL: "1h"})

	// A forged credential: a real key's lookup prefix with a fabricated
	// suffix — well-shaped, prefix found, hash mismatch.
	forged := admin.Prefix + strings.Repeat("A", 46-len(admin.Prefix))
	if forged == admin.Key {
		t.Fatal("forged key collided with the real one")
	}

	keyBody := func() []byte {
		b, err := json.Marshal(api.CreateKeyRequest{Name: "probe-mint", Scopes: []string{"read"}})
		if err != nil {
			t.Fatalf("marshaling key body: %v", err)
		}
		return b
	}

	routes := []struct {
		method, path string
		scope        api.Scope
		body         func() []byte // nil = no body
		ok           int           // success status with a sufficient scope
	}{
		{http.MethodPost, "/v1/runs", api.ScopeSubmit, func() []byte { return submitBody(t, probeDefJSON, "", "") }, http.StatusCreated},
		{http.MethodGet, "/v1/runs/" + sub.RunID, api.ScopeRead, nil, http.StatusOK},
		{http.MethodPost, "/v1/keys", api.ScopeAdmin, keyBody, http.StatusCreated},
		{http.MethodGet, "/v1/keys", api.ScopeAdmin, nil, http.StatusOK},
		{http.MethodDelete, "/v1/keys/" + victim.ID, api.ScopeAdmin, nil, http.StatusNoContent},
	}
	bodyOf := func(f func() []byte) []byte {
		if f == nil {
			return nil
		}
		return f()
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			// Every credential failure collapses to the uniform 401 with
			// the RFC 6750 challenge — missing, malformed, unknown,
			// wrong-hash-right-prefix, and revoked are indistinguishable.
			for name, bearer := range map[string]string{
				"missing":       "",
				"malformed":     "not-a-key",
				"unknown":       mintTestKey(t),
				"forged prefix": forged,
				"revoked":       revoked.Key,
			} {
				var envelope api.ErrorBody
				res := doAuth(t, srv, rt.method, rt.path, bearer, bodyOf(rt.body), &envelope)
				if res.StatusCode != http.StatusUnauthorized || envelope.Error.Code != api.ErrCodeUnauthorized {
					t.Errorf("%s credential = %d/%q, want 401/unauthorized", name, res.StatusCode, envelope.Error.Code)
				}
				if res.Header.Get("WWW-Authenticate") != "Bearer" {
					t.Errorf("%s credential: missing WWW-Authenticate challenge", name)
				}
			}

			// A non-Bearer scheme is a credential failure too.
			req, err := http.NewRequest(rt.method, srv.URL+rt.path, bytes.NewReader(bodyOf(rt.body)))
			if err != nil {
				t.Fatalf("building basic-scheme request: %v", err)
			}
			req.Header.Set("Authorization", "Basic dXNlcjpwdw==")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("basic-scheme request: %v", err)
			}
			res.Body.Close() //nolint:errcheck // read-side close
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("basic scheme = %d, want 401", res.StatusCode)
			}

			// A valid key lacking the scope: 403 with the machine-readable
			// envelope naming the missing scope.
			var envelope api.ErrorBody
			resp := doAuth(t, srv, rt.method, rt.path, wrongKey[rt.scope], bodyOf(rt.body), &envelope)
			if resp.StatusCode != http.StatusForbidden || envelope.Error.Code != api.ErrCodeForbidden {
				t.Errorf("scope-lacking key = %d/%q, want 403/forbidden", resp.StatusCode, envelope.Error.Code)
			}
			if !strings.Contains(envelope.Error.Message, string(rt.scope)) {
				t.Errorf("403 message %q does not name the missing %q scope", envelope.Error.Message, rt.scope)
			}

			// The exact scope suffices, and admin implies it.
			for name, bearer := range map[string]string{"exact scope": rightKey[rt.scope], "admin": admin.Key} {
				resp := doAuth(t, srv, rt.method, rt.path, bearer, bodyOf(rt.body), nil)
				if resp.StatusCode != rt.ok {
					t.Errorf("%s key = %d, want %d", name, resp.StatusCode, rt.ok)
				}
			}
		})
	}

	// Expiry collapses to the same 401 on every route once the server
	// clock passes expires_at.
	*now = testNow.Add(2 * time.Hour)
	for _, rt := range routes {
		if res := doAuth(t, srv, rt.method, rt.path, expiring.Key, bodyOf(rt.body), nil); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("expired key on %s %s = %d, want 401", rt.method, rt.path, res.StatusCode)
		}
	}
	*now = testNow

	// /healthz stays exempt: no credential, 200.
	if res := doAuth(t, srv, http.MethodGet, "/healthz", "", nil, nil); res.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz anonymous = %d, want 200", res.StatusCode)
	}

	assertAuthLogDiscipline(t, logs, admin, rightKey, wrongKey, []string{rootKey, revoked.Key, expiring.Key, victim.Key})
}

// assertAuthLogDiscipline is the ticket's third criterion: auth outcomes
// are logged with key_id — the request lines of authenticated calls and
// the forbidden lines both carry it — and no plaintext credential ever
// reaches the log stream.
func assertAuthLogDiscipline(t *testing.T, logs *syncBuffer, admin api.CreateKeyResponse, rightKey, wrongKey map[api.Scope]string, extraSecrets []string) {
	t.Helper()
	logged := logs.String()
	if logged == "" {
		t.Fatal("no logs captured — the assertions below would be vacuous")
	}

	// key_id present: the admin key's row id (stamped on request lines and
	// scope-check outcomes) and the root pseudo-id from the bootstrap mint.
	for _, want := range []string{`"key_id":"` + admin.ID + `"`, `"key_id":"root"`} {
		if !strings.Contains(logged, want) {
			t.Errorf("logs lack %s — auth outcomes must carry key_id", want)
		}
	}
	// The forbidden outcome names the missing scope in a structured field.
	if !strings.Contains(logged, `"missing_scope"`) {
		t.Error("logs lack a missing_scope field for the 403 outcomes")
	}

	// And never any key material beyond the prefix.
	secrets := append([]string{admin.Key}, extraSecrets...)
	for _, m := range []map[api.Scope]string{rightKey, wrongKey} {
		for _, k := range m {
			secrets = append(secrets, k)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Fatal("plaintext key appeared in the server logs")
		}
	}
	// Positive control so the absence assertion cannot be vacuous.
	if !strings.Contains(logged, admin.Prefix) {
		t.Error("expected the admin key's prefix in the logs (the allowed correlation handle)")
	}
}
