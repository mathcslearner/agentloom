package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
)

// fakeKeysServer stubs the /v1/keys routes, recording the bearer
// credential each request presented.
func fakeKeysServer(t *testing.T, bearers *[]string) *httptest.Server {
	t.Helper()
	// Constructed, not a literal: the CI secret grep flags committed
	// sk_-shaped strings.
	plaintext := "sk_" + strings.Repeat("k", 43)
	created := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	expires := created.Add(24 * time.Hour)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		*bearers = append(*bearers, r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodPost:
			var req api.CreateKeyRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decoding create request: %v", err)
			}
			if req.Name != "ci" || len(req.Scopes) != 2 || req.TTL != "24h" {
				t.Errorf("unexpected create request: %+v", req)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.CreateKeyResponse{
				KeyView: api.KeyView{
					ID:        "11111111-1111-1111-1111-111111111111",
					Prefix:    plaintext[:11],
					Name:      req.Name,
					Scopes:    req.Scopes,
					CreatedAt: created,
					ExpiresAt: &expires,
				},
				Key: plaintext,
			})
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(api.ListKeysResponse{Keys: []api.KeyView{
				{
					ID: "11111111-1111-1111-1111-111111111111", Prefix: plaintext[:11],
					Name: "ci", Scopes: []string{"submit", "read"}, CreatedAt: created,
				},
				{
					ID: "22222222-2222-2222-2222-222222222222", Prefix: "sk_deadbeef",
					Name: "old", Scopes: []string{"admin"}, CreatedAt: created, RevokedAt: &expires,
				},
			}})
		default:
			t.Errorf("unexpected method %s on /v1/keys", r.Method)
		}
	})
	mux.HandleFunc("/v1/keys/", func(w http.ResponseWriter, r *http.Request) {
		*bearers = append(*bearers, r.Header.Get("Authorization"))
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method %s on %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestKeysCreatePrintsPlaintextToStdoutOnly(t *testing.T) {
	t.Parallel()
	var bearers []string
	srv := fakeKeysServer(t, &bearers)
	adminKey := "sk_" + strings.Repeat("a", 43)

	out, errOut, err := runCtl(t, map[string]string{"AGENTLOOM_API_KEY": adminKey},
		"keys", "create", "--api", srv.URL, "--name", "ci", "--scopes", "submit, read", "--ttl", "24h")
	if err != nil {
		t.Fatalf("keys create: %v", err)
	}
	// stdout is exactly the plaintext key, so command substitution composes.
	if want := "sk_" + strings.Repeat("k", 43) + "\n"; out != want {
		t.Errorf("stdout = %q, want the bare plaintext key line", out)
	}
	for _, needle := range []string{"11111111", "shown once", "submit,read"} {
		if !strings.Contains(errOut, needle) {
			t.Errorf("stderr %q missing %q", errOut, needle)
		}
	}
	if len(bearers) != 1 || bearers[0] != "Bearer "+adminKey {
		t.Errorf("bearer headers = %v, want the AGENTLOOM_API_KEY credential", bearers)
	}
}

func TestKeysListRendersTable(t *testing.T) {
	t.Parallel()
	var bearers []string
	srv := fakeKeysServer(t, &bearers)

	out, _, err := runCtl(t, nil, "keys", "list", "--api", srv.URL, "--key", "sk_"+strings.Repeat("b", 43))
	if err != nil {
		t.Fatalf("keys list: %v", err)
	}
	for _, needle := range []string{"PREFIX", "sk_deadbeef", "submit,read", "2026-08-13T10:00:00Z"} {
		if !strings.Contains(out, needle) {
			t.Errorf("listing %q missing %q", out, needle)
		}
	}
	if len(bearers) != 1 || !strings.HasPrefix(bearers[0], "Bearer sk_b") {
		t.Errorf("bearer headers = %v, want the --key credential", bearers)
	}
}

func TestKeysRevoke(t *testing.T) {
	t.Parallel()
	var bearers []string
	srv := fakeKeysServer(t, &bearers)

	out, errOut, err := runCtl(t, nil, "keys", "revoke", "--api", srv.URL,
		"--key", "sk_"+strings.Repeat("c", 43), "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("keys revoke: %v", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty (status goes to stderr)", out)
	}
	if !strings.Contains(errOut, "revoked") {
		t.Errorf("stderr %q does not confirm revocation", errOut)
	}
}

func TestKeysCreateRequiresFlags(t *testing.T) {
	t.Parallel()
	if _, _, err := runCtl(t, nil, "keys", "create"); err == nil {
		t.Fatal("keys create without --name/--scopes succeeded")
	}
}
