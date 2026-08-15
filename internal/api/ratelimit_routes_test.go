package api

// Ticket 6.4's route→class coverage guard, the same discipline as 6.2's
// route→scope test: a /v1 route cannot ship without a decided rate-limit
// route class.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mathcslearner/agentloom/internal/store"
)

// routeClasses is ADR-007's route→class table as enforced (ticket 6.4).
// Deliberately spelled out rather than derived from the router: the
// coverage test below fails when the router and this table drift apart in
// either direction. When adding a row, mount h.rateLimit(<class>) after
// requireScope on the route and extend the behavioral coverage in
// ratelimit_integration_test.go.
var routeClasses = map[string]routeClass{
	"POST /v1/runs":                                classSubmit,
	"GET /v1/runs":                                 classRead,
	"GET /v1/runs/{runID}":                         classRead,
	"GET /v1/runs/{runID}/cost":                    classRead,
	"POST /v1/runs/{runID}/cancel":                 classSubmit,
	"POST /v1/runs/{runID}/park":                   classSubmit,
	"POST /v1/runs/{runID}/unpark":                 classSubmit,
	"POST /v1/runs/{runID}/steps/{stepID}/requeue": classSubmit,
	"GET /v1/runs/{runID}/steps/{stepID}/logs":     classRead,
	"POST /v1/definitions":                         classSubmit,
	"GET /v1/definitions":                          classRead,
	"GET /v1/definitions/{defID}":                  classRead,
	"GET /v1/definitions/{name}/versions":          classRead,
	"POST /v1/definitions/{name}/versions":         classSubmit,
	"GET /v1/plugins":                              classRead,
	"POST /v1/cache/bust":                          classAdmin,
	"GET /v1/cache/stats":                          classAdmin,
	"POST /v1/keys":                                classAdmin,
	"GET /v1/keys":                                 classAdmin,
	"DELETE /v1/keys/{keyID}":                      classAdmin,
}

// TestV1RouteClassCoverage walks the live router and asserts every /v1
// route appears in routeClasses and vice versa. /healthz is the only
// deliberate exemption (probes must never be throttled by client limits);
// 404/405 fallbacks run outside the /v1 middleware tree, same as auth.
func TestV1RouteClassCoverage(t *testing.T) {
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
			return nil // auth coverage polices non-/v1 routes
		}
		key := method + " " + route
		seen[key] = true
		if _, ok := routeClasses[key]; !ok {
			t.Errorf("route %q is not in the route→class table — decide its rate-limit class, mount rateLimit after requireScope, and extend the integration coverage", key)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking router: %v", walkErr)
	}
	for key := range routeClasses {
		if !seen[key] {
			t.Errorf("route→class table lists %q but the router does not serve it", key)
		}
	}
}
