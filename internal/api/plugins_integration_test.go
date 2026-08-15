//go:build integration

package api_test

// Ticket 8.1: GET /v1/plugins serves the compiled-in plugin catalog —
// manifests with capability flags and per-plugin config schemas
// (ADR-009). Auth/rate-limit coverage rides the route tables' matrix
// tests; this suite covers the payload.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/tools"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// pluginsServer boots the API with the full builtin catalog wired, the
// way cmd/api does with AGENTLOOM_API_TEST_EXECUTORS=true.
func pluginsServer(t *testing.T, rootKey string) *httptest.Server {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t))
	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, testDiscard{})
	h, err := api.New(s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{},
		api.WithPlugins(exec.Builtins(nil, nil, nil).Manifests()))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// testDiscard is an io.Writer sink for logs this suite never asserts on.
type testDiscard struct{}

func (testDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestListPlugins(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	srv := pluginsServer(t, rootKey)

	var resp api.ListPluginsResponse
	if res := doAuth(t, srv, http.MethodGet, "/v1/plugins", rootKey, nil, &resp); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins = %d, want 200", res.StatusCode)
	}
	if len(resp.Plugins) != 11 {
		t.Fatalf("catalog has %d plugins, want the 11 builtins", len(resp.Plugins))
	}
	if !sort.SliceIsSorted(resp.Plugins, func(i, j int) bool {
		a, b := resp.Plugins[i], resp.Plugins[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	}) {
		t.Error("catalog is not sorted by (kind, name)")
	}

	byName := map[string]api.PluginInfo{}
	for _, p := range resp.Plugins {
		if p.Kind != "executor" {
			t.Errorf("%s: kind %q — 8.1 ships executors only", p.Name, p.Kind)
		}
		if p.Version == "" {
			t.Errorf("%s: empty version", p.Name)
		}
		// Machine-usable schema (the ticket's acceptance): every builtin
		// carries a parseable JSON Schema object.
		var schema map[string]any
		if err := json.Unmarshal(p.ConfigSchema, &schema); err != nil {
			t.Errorf("%s: config_schema is not a JSON object: %v", p.Name, err)
		} else if schema["$schema"] == nil {
			t.Errorf("%s: config_schema missing $schema", p.Name)
		}
		byName[p.Name] = p
	}

	// Capability flags queryable per plugin (the ticket's acceptance):
	// spot-check the ADR-009 table's three distinct shapes.
	if p := byName["counter"]; !p.Capabilities.SideEffectful || p.Capabilities.Cacheable {
		t.Errorf("counter capabilities = %+v — want side_effectful only", p.Capabilities)
	}
	if p := byName["llm"]; !p.Capabilities.Cacheable || !p.Capabilities.CostBearing || p.Capabilities.SideEffectful {
		t.Errorf("llm capabilities = %+v — want cacheable + cost_bearing", p.Capabilities)
	}
	if p := byName["sleep"]; p.Capabilities != (api.PluginCapabilities{}) {
		t.Errorf("sleep capabilities = %+v — want none", p.Capabilities)
	}

	// The llm schema names its config fields — the property UI forms
	// will walk (M17.4).
	var llmSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(byName["llm"].ConfigSchema, &llmSchema); err != nil {
		t.Fatalf("unmarshaling llm schema: %v", err)
	}
	if _, ok := llmSchema.Properties["model"]; !ok {
		t.Error("llm config_schema does not declare the model property")
	}
}

// TestListPluginsCoreSet pins the 6.2 gating on the listing: without
// test executors, counter and effectful_echo are absent — the catalog
// mirrors what the matching worker config would execute (ADR-009's
// single-build assumption).
func TestListPluginsCoreSet(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	s := store.NewFromPool(storetest.NewDB(t))
	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, testDiscard{})
	h, err := api.New(s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{},
		api.WithPlugins(exec.CoreBuiltins(nil, nil, nil).Manifests()))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var resp api.ListPluginsResponse
	if res := doAuth(t, srv, http.MethodGet, "/v1/plugins", rootKey, nil, &resp); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins = %d, want 200", res.StatusCode)
	}
	if len(resp.Plugins) != 9 {
		t.Fatalf("core catalog has %d plugins, want 9", len(resp.Plugins))
	}
	for _, p := range resp.Plugins {
		if p.Name == "counter" || p.Name == "effectful_echo" {
			t.Errorf("core catalog lists opt-in test executor %q", p.Name)
		}
	}
}

// TestListPluginsWithProviders pins ticket 8.4's catalog wiring: model
// providers appended to the executor catalog (the way cmd/api folds in
// llm.NewRegistryFromKeys) surface on GET /v1/plugins with kind
// model_provider and their capability flags, sorted into (kind, name)
// order alongside the executors.
func TestListPluginsWithProviders(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)

	// Both provider keys present → both providers in the catalog. Keys are
	// constructed at runtime, never committed (6.1 secret grep).
	key := "sk-test-" + strings.Repeat("z", 8)
	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Anthropic: key, OpenAI: key})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	manifests := append(exec.CoreBuiltins(nil, nil, nil).Manifests(), providers.Manifests()...)

	s := store.NewFromPool(storetest.NewDB(t))
	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, testDiscard{})
	h, err := api.New(s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{},
		api.WithPlugins(manifests))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var resp api.ListPluginsResponse
	if res := doAuth(t, srv, http.MethodGet, "/v1/plugins", rootKey, nil, &resp); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins = %d, want 200", res.StatusCode)
	}
	byKind := map[string][]api.PluginInfo{}
	for _, p := range resp.Plugins {
		byKind[p.Kind] = append(byKind[p.Kind], p)
	}
	provs := byKind["model_provider"]
	if len(provs) != 2 {
		t.Fatalf("catalog has %d model providers, want 2", len(provs))
	}
	for _, p := range provs {
		if !p.Capabilities.Cacheable || !p.Capabilities.CostBearing || p.Capabilities.SideEffectful {
			t.Errorf("%s capabilities = %+v — want cacheable + cost_bearing", p.Name, p.Capabilities)
		}
		if p.Version == "" {
			t.Errorf("%s: empty version", p.Name)
		}
	}
	if provs[0].Name != "anthropic" || provs[1].Name != "openai" {
		t.Errorf("provider order = [%s %s], want [anthropic openai]", provs[0].Name, provs[1].Name)
	}
	// The full catalog stays sorted by (kind, name): executors precede
	// model_provider in the documentation order.
	if !sort.SliceIsSorted(resp.Plugins, func(i, j int) bool {
		a, b := resp.Plugins[i], resp.Plugins[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	}) {
		t.Error("combined catalog is not sorted by (kind, name)")
	}
}

// TestListPluginsWithMockProvider pins ticket 8.5's catalog wiring: the
// mock provider (enabled by config, no key) surfaces on GET /v1/plugins
// with kind model_provider and its distinctive flags — cacheable but
// deliberately NOT cost_bearing (a free, offline provider).
func TestListPluginsWithMockProvider(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)

	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	manifests := append(exec.CoreBuiltins(nil, nil, nil).Manifests(), providers.Manifests()...)

	s := store.NewFromPool(storetest.NewDB(t))
	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, testDiscard{})
	h, err := api.New(s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{},
		api.WithPlugins(manifests))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var resp api.ListPluginsResponse
	if res := doAuth(t, srv, http.MethodGet, "/v1/plugins", rootKey, nil, &resp); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins = %d, want 200", res.StatusCode)
	}
	var mock *api.PluginInfo
	for i := range resp.Plugins {
		if resp.Plugins[i].Kind == "model_provider" && resp.Plugins[i].Name == "mock" {
			mock = &resp.Plugins[i]
		}
	}
	if mock == nil {
		t.Fatalf("mock provider missing from catalog: %+v", resp.Plugins)
	}
	if !mock.Capabilities.Cacheable || mock.Capabilities.CostBearing || mock.Capabilities.SideEffectful {
		t.Errorf("mock capabilities = %+v — want cacheable only (free, offline)", mock.Capabilities)
	}
}

// TestListPluginsWithTools pins ticket 8.7's catalog wiring: the built-in
// tools (the way cmd/api folds in tools.NewBuiltins) surface on
// GET /v1/plugins with kind tool, their distinct capability flags, and an
// args JSON Schema — the schema UI forms (M17.4) render for tool config.
func TestListPluginsWithTools(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)

	toolReg, err := tools.NewBuiltins(tools.HTTPOptions{})
	if err != nil {
		t.Fatalf("tools.NewBuiltins: %v", err)
	}
	manifests := append(exec.CoreBuiltins(nil, nil, nil).Manifests(), toolReg.Manifests()...)

	s := store.NewFromPool(storetest.NewDB(t))
	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, testDiscard{})
	h, err := api.New(s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{},
		api.WithPlugins(manifests))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var resp api.ListPluginsResponse
	if res := doAuth(t, srv, http.MethodGet, "/v1/plugins", rootKey, nil, &resp); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins = %d, want 200", res.StatusCode)
	}
	byName := map[string]api.PluginInfo{}
	for _, p := range resp.Plugins {
		if p.Kind == "tool" {
			byName[p.Name] = p
		}
	}
	if len(byName) != 2 {
		t.Fatalf("catalog has %d tools, want 2 (http_request, json_transform)", len(byName))
	}
	if p := byName["http_request"]; !p.Capabilities.SideEffectful || p.Capabilities.Cacheable {
		t.Errorf("http_request capabilities = %+v — want side_effectful only", p.Capabilities)
	}
	if p := byName["json_transform"]; !p.Capabilities.Cacheable || p.Capabilities.SideEffectful || p.Capabilities.CostBearing {
		t.Errorf("json_transform capabilities = %+v — want cacheable only", p.Capabilities)
	}
	// Each tool carries a machine-usable args schema naming its fields.
	var httpSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(byName["http_request"].ConfigSchema, &httpSchema); err != nil {
		t.Fatalf("unmarshaling http_request schema: %v", err)
	}
	if _, ok := httpSchema.Properties["url"]; !ok {
		t.Error("http_request args schema does not declare the url property")
	}
}

// TestListPluginsWithRetrievers pins ticket 8.8's catalog wiring: the
// reference retriever (the way cmd/api folds in retrieval.NewRegistry)
// surfaces on GET /v1/plugins with kind retriever, its capability flags,
// and no config schema (retrievers declare none), sorted into (kind, name)
// order alongside the executors.
func TestListPluginsWithRetrievers(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)

	s := store.NewFromPool(storetest.NewDB(t))
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	manifests := append(exec.CoreBuiltins(nil, nil, nil).Manifests(), retrievers.Manifests()...)

	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, testDiscard{})
	h, err := api.New(s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{},
		api.WithPlugins(manifests))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var resp api.ListPluginsResponse
	if res := doAuth(t, srv, http.MethodGet, "/v1/plugins", rootKey, nil, &resp); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins = %d, want 200", res.StatusCode)
	}
	var pg *api.PluginInfo
	for i := range resp.Plugins {
		if resp.Plugins[i].Kind == "retriever" && resp.Plugins[i].Name == "pg_fulltext" {
			pg = &resp.Plugins[i]
		}
	}
	if pg == nil {
		t.Fatalf("pg_fulltext retriever missing from catalog: %+v", resp.Plugins)
	}
	if !pg.Capabilities.Cacheable || pg.Capabilities.SideEffectful || pg.Capabilities.CostBearing {
		t.Errorf("pg_fulltext capabilities = %+v — want cacheable only", pg.Capabilities)
	}
	if pg.Version == "" {
		t.Error("pg_fulltext has an empty version")
	}
	// The combined catalog stays sorted by (kind, name): executors precede
	// retriever in the documentation order.
	if !sort.SliceIsSorted(resp.Plugins, func(i, j int) bool {
		a, b := resp.Plugins[i], resp.Plugins[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	}) {
		t.Error("combined catalog is not sorted by (kind, name)")
	}
}

// TestListPluginsWithValidators pins ticket 11.2's catalog wiring: the five
// deterministic built-in validators (the way cmd/api folds in
// validate.NewBuiltins) surface on GET /v1/plugins with kind validator,
// cacheable-only flags, and — the "config schemas exposed via plugin
// registry (UI-ready)" criterion — a non-empty, self-describing config
// schema each.
func TestListPluginsWithValidators(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)

	s := store.NewFromPool(storetest.NewDB(t))
	validators, err := validate.NewBuiltins(nil)
	if err != nil {
		t.Fatalf("validate.NewBuiltins: %v", err)
	}
	manifests := append(exec.CoreBuiltins(nil, nil, nil).Manifests(), validators.Manifests()...)

	logger := log.New(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, testDiscard{})
	h, err := api.New(s, func() time.Time { return testNow }, logger, rootKey, api.RateLimitOptions{},
		api.WithPlugins(manifests))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var resp api.ListPluginsResponse
	if res := doAuth(t, srv, http.MethodGet, "/v1/plugins", rootKey, nil, &resp); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins = %d, want 200", res.StatusCode)
	}
	got := map[string]api.PluginInfo{}
	for _, p := range resp.Plugins {
		if p.Kind == "validator" {
			got[p.Name] = p
		}
	}
	wantNames := []string{"cel", "contains", "json_schema", "llm_judge", "numeric_range", "regex"}
	if len(got) != len(wantNames) {
		t.Fatalf("catalog has %d validators, want %d: %+v", len(got), len(wantNames), got)
	}
	for _, name := range wantNames {
		p, ok := got[name]
		if !ok {
			t.Errorf("validator %q missing from catalog", name)
			continue
		}
		// The deterministic validators are cacheable-only; llm_judge (11.5) is
		// additionally cost_bearing (it calls a provider). None is ever
		// side_effectful.
		wantCostBearing := name == "llm_judge"
		if !p.Capabilities.Cacheable || p.Capabilities.SideEffectful || p.Capabilities.CostBearing != wantCostBearing {
			t.Errorf("%s capabilities = %+v — want cacheable%s", name, p.Capabilities,
				map[bool]string{true: "+cost_bearing", false: " only"}[wantCostBearing])
		}
		if p.Version != "1.0.0" {
			t.Errorf("%s version = %q, want 1.0.0", name, p.Version)
		}
		// UI-ready: a valid, self-describing config schema.
		var schema map[string]any
		if err := json.Unmarshal(p.ConfigSchema, &schema); err != nil {
			t.Errorf("%s: config_schema is not a JSON object: %v", name, err)
		} else if schema["$schema"] == nil {
			t.Errorf("%s: config_schema missing $schema", name)
		}
	}
}

// TestListPluginsEmptyCatalog pins the default: a handler with no
// WithPlugins serves an empty list, not null and not an error.
func TestListPluginsEmptyCatalog(t *testing.T) {
	t.Parallel()
	rootKey := mintTestKey(t)
	_, srv, _, _ := keysServer(t, rootKey)

	res := doAuth(t, srv, http.MethodGet, "/v1/plugins", rootKey, nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins = %d, want 200", res.StatusCode)
	}
	body := readBody(t, srv, rootKey)
	if body != `{"plugins":[]}` {
		t.Fatalf("empty catalog body = %s — want {\"plugins\":[]}", body)
	}
}

// readBody re-fetches /v1/plugins and returns the trimmed raw body (the
// null-vs-empty distinction is invisible through decoding).
func readBody(t *testing.T, srv *httptest.Server, bearer string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/plugins", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/plugins: %v", err)
	}
	defer res.Body.Close() //nolint:errcheck // read-side close
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return strings.TrimSpace(string(raw))
}
