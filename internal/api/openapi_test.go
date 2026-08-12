package api

// Ticket 6.6's contract guards: api/openapi.yaml is the hand-maintained
// API contract, and these tests keep it honest against the live router.
// TestOpenAPIRouteCoverage is the drift check — a route cannot ship
// undocumented, and a documented path cannot outlive its handler.
// TestOpenAPIOperationContracts pins the envelope conventions every
// operation must document. Both parse the spec as plain YAML: the checks
// only need paths/operations/responses, and a full OpenAPI resolver
// would drag in a second source of truth (the spec's own validity is
// vacuum's job — `make openapi-lint`).

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"

	"github.com/mathcslearner/agentloom/internal/store"
)

// specPath locates api/openapi.yaml from this package's directory.
const specPath = "../../api/openapi.yaml"

// operation is the slice of an OpenAPI operation these tests care about.
type operation struct {
	OperationID string `yaml:"operationId"`
	Responses   map[string]struct {
		Ref     string         `yaml:"$ref"`
		Content map[string]any `yaml:"content"`
	} `yaml:"responses"`
}

// httpMethods are the OpenAPI path-item keys that name operations; the
// other path-item keys (parameters, description, ...) are not routes.
var httpMethods = map[string]string{
	"get":     http.MethodGet,
	"put":     http.MethodPut,
	"post":    http.MethodPost,
	"delete":  http.MethodDelete,
	"options": http.MethodOptions,
	"head":    http.MethodHead,
	"patch":   http.MethodPatch,
	"trace":   http.MethodTrace,
}

// pathParamPattern erases path-parameter names: the spec spells them
// snake_case ({run_id}) and chi camelCase ({runID}), and the contract
// identity of a route does not hang on the placeholder's name.
var pathParamPattern = regexp.MustCompile(`\{[^}]*\}`)

// loadSpecOperations parses the spec and returns every documented
// operation keyed "METHOD /normalized/path".
func loadSpecOperations(t *testing.T) map[string]operation {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}
	var spec struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parsing %s: %v", specPath, err)
	}
	if len(spec.Paths) == 0 {
		t.Fatalf("%s has no paths — parse mismatch?", specPath)
	}
	ops := map[string]operation{}
	for path, item := range spec.Paths {
		for key, node := range item {
			method, ok := httpMethods[key]
			if !ok {
				continue
			}
			var op operation
			if err := node.Decode(&op); err != nil {
				t.Fatalf("decoding operation %s %s: %v", key, path, err)
			}
			ops[method+" "+pathParamPattern.ReplaceAllString(path, "{}")] = op
		}
	}
	return ops
}

// routerRoutes walks the live router and returns every mounted route as
// "METHOD /normalized/path". Router construction never touches the
// database, so a store over a nil pool suffices.
func routerRoutes(t *testing.T) map[string]bool {
	t.Helper()
	h, err := New(store.NewFromPool(nil), time.Now, nil, "", RateLimitOptions{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	routes := map[string]bool{}
	walkErr := chi.Walk(h.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route != "/" {
			route = strings.TrimSuffix(route, "/")
		}
		routes[method+" "+pathParamPattern.ReplaceAllString(route, "{}")] = true
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking router: %v", walkErr)
	}
	return routes
}

// TestOpenAPIRouteCoverage is the drift check: every mounted route must
// be documented in api/openapi.yaml and every documented operation must
// be mounted. Runs in the plain unit suite, so CI enforces it on every
// change.
func TestOpenAPIRouteCoverage(t *testing.T) {
	t.Parallel()

	ops := loadSpecOperations(t)
	routes := routerRoutes(t)

	for key := range routes {
		if _, ok := ops[key]; !ok {
			t.Errorf("route %q is mounted but missing from api/openapi.yaml — the spec is the contract, document it", key)
		}
	}
	for key := range ops {
		if !routes[key] {
			t.Errorf("api/openapi.yaml documents %q but no such route is mounted — remove it or mount the handler", key)
		}
	}
}

// TestOpenAPIOperationContracts pins the response conventions: every
// operation carries an operationId (the typed client's method name,
// M17) and at least one 2xx; every authenticated /v1 operation also
// documents the envelope failures auth and rate limiting can hand any
// caller (401, 403, 429).
func TestOpenAPIOperationContracts(t *testing.T) {
	t.Parallel()

	for key, op := range loadSpecOperations(t) {
		if op.OperationID == "" {
			t.Errorf("%s: missing operationId", key)
		}
		if len(op.Responses) == 0 {
			t.Errorf("%s: no responses documented", key)
			continue
		}
		var has2xx bool
		for status, resp := range op.Responses {
			if !strings.HasPrefix(status, "2") {
				continue
			}
			has2xx = true
			// A success response must carry a schema'd body — 204 is the
			// one deliberate no-body success.
			if status != "204" && resp.Ref == "" && len(resp.Content) == 0 {
				t.Errorf("%s: success response %s has no content schema", key, status)
			}
		}
		if !has2xx {
			t.Errorf("%s: no 2xx response documented", key)
		}
		if !strings.HasPrefix(key[strings.Index(key, " ")+1:], "/v1") {
			continue
		}
		for _, status := range []string{"401", "403", "429"} {
			if _, ok := op.Responses[status]; !ok {
				t.Errorf("%s: /v1 operations must document %s (uniform auth + rate-limit envelope)", key, status)
			}
		}
	}
}
