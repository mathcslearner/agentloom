package api

// Ticket 6.2's unit-level guards: the route→scope coverage check (a /v1
// route cannot ship without a decided ADR-007 scope) and the request-
// context plumbing requireScope stamps for downstream consumers.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mathcslearner/agentloom/internal/store"
)

// routeScopes is ADR-007's route→scope table as enforced (ticket 6.2).
// It is deliberately spelled out here rather than derived from the
// router: the coverage test below fails when the router and this table
// drift apart in either direction. When adding a row, add the matching
// behavioral coverage to TestV1AuthMatrix in
// auth_routes_integration_test.go.
var routeScopes = map[string]Scope{
	"POST /v1/runs":                                ScopeSubmit,
	"GET /v1/runs":                                 ScopeRead,
	"GET /v1/runs/{runID}":                         ScopeRead,
	"GET /v1/runs/{runID}/cost":                    ScopeRead,
	"POST /v1/runs/{runID}/cancel":                 ScopeSubmit,
	"POST /v1/runs/{runID}/park":                   ScopeSubmit,
	"POST /v1/runs/{runID}/unpark":                 ScopeSubmit,
	"PATCH /v1/runs/{runID}/budget":                ScopeSubmit,
	"POST /v1/runs/{runID}/steps/{stepID}/requeue": ScopeSubmit,
	"GET /v1/runs/{runID}/steps/{stepID}/logs":     ScopeRead,
	"GET /v1/runs/{runID}/blackboard":              ScopeRead,
	"GET /v1/runs/{runID}/graph":                   ScopeRead,
	"POST /v1/runs/{runID}/ws-ticket":              ScopeRead,
	"POST /v1/definitions":                         ScopeSubmit,
	"GET /v1/definitions":                          ScopeRead,
	"GET /v1/definitions/{defID}":                  ScopeRead,
	"GET /v1/definitions/{name}/versions":          ScopeRead,
	"POST /v1/definitions/{name}/versions":         ScopeSubmit,
	"GET /v1/approvals":                            ScopeRead,
	"POST /v1/approvals/{approvalID}:decide":       ScopeApprove,
	"GET /v1/plugins":                              ScopeRead,
	"POST /v1/cache/bust":                          ScopeAdmin,
	"GET /v1/cache/stats":                          ScopeAdmin,
	"POST /v1/keys":                                ScopeAdmin,
	"GET /v1/keys":                                 ScopeAdmin,
	"DELETE /v1/keys/{keyID}":                      ScopeAdmin,
}

// TestV1RouteScopeCoverage walks the live router and asserts every /v1
// route appears in routeScopes and vice versa. /healthz is the only
// deliberate exemption (probes need no secret, ADR-007); anything else
// outside /v1 is a routing mistake.
func TestV1RouteScopeCoverage(t *testing.T) {
	t.Parallel()

	// Router construction never touches the database, so a store over a
	// nil pool is enough to walk the route table.
	h, err := New(store.NewFromPool(nil), time.Now, nil, "", RateLimitOptions{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seen := map[string]bool{}
	walkErr := chi.Walk(h.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route != "/" {
			route = strings.TrimSuffix(route, "/")
		}
		if !strings.HasPrefix(route, "/v1") {
			if route != "/healthz" {
				t.Errorf("unexpected route outside /v1: %s %s — only /healthz may bypass auth", method, route)
			}
			return nil
		}
		key := method + " " + route
		// The run WebSocket (ticket 16.3) authenticates via requireReadOrTicket
		// (a signed ticket OR a read bearer), not requireScope, so it is
		// deliberately absent from the route→scope table; TestWSTicket* and the
		// WS integration suite cover its auth. Every other /v1 route uses
		// requireScope and must appear here.
		if key == "GET /v1/runs/{runID}/ws" {
			return nil
		}
		seen[key] = true
		if _, ok := routeScopes[key]; !ok {
			t.Errorf("route %q is not in the route→scope table — decide its ADR-007 scope, mount requireScope, and extend the auth matrix", key)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking router: %v", walkErr)
	}
	for key := range routeScopes {
		if !seen[key] {
			t.Errorf("table route %q is not mounted on the router", key)
		}
	}
}

// TestIdentityContextRoundTrip covers the stamp requireScope leaves for
// handlers and 6.4's per-key rate limiting.
func TestIdentityContextRoundTrip(t *testing.T) {
	t.Parallel()

	if _, ok := identityFrom(context.Background()); ok {
		t.Fatal("bare context yielded an identity")
	}
	want := identity{keyID: "key-1", scopes: []string{"submit", "read"}}
	got, ok := identityFrom(identityInto(context.Background(), want))
	if !ok || got.keyID != want.keyID || len(got.scopes) != 2 {
		t.Fatalf("identityFrom = %+v, %v — want %+v", got, ok, want)
	}
}

// TestAuthStampRoundTrip covers the mutable slot that carries the
// authenticated key_id back up to requestLog's per-request line.
func TestAuthStampRoundTrip(t *testing.T) {
	t.Parallel()

	if _, ok := authStampFrom(context.Background()); ok {
		t.Fatal("bare context yielded a stamp")
	}
	stamp := &authStamp{}
	got, ok := authStampFrom(authStampInto(context.Background(), stamp))
	if !ok || got != stamp {
		t.Fatalf("authStampFrom returned %v, %v — want the installed slot", got, ok)
	}
	got.keyID = "key-1"
	if stamp.keyID != "key-1" {
		t.Fatal("write through the retrieved stamp did not reach the installed slot")
	}
}
