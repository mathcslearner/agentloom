package llm_test

// Ticket 8.4: the model-provider registry facade and its routing table.
// Routing is tested against real Anthropic/OpenAI providers (constructed
// with runtime keys — never a call is made, only Manifest/identity is
// used) so the (kind, name) identity wiring is exercised end to end.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

func mustAnthropic(t *testing.T) *llm.Anthropic {
	t.Helper()
	p, err := llm.NewAnthropic(llm.AnthropicConfig{APIKey: testKey()})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	return p
}

func mustOpenAI(t *testing.T) *llm.OpenAI {
	t.Helper()
	p, err := llm.NewOpenAI(llm.OpenAIConfig{APIKey: testKey()})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	return p
}

// TestRegistryRegistration pins the registration discipline: sorted
// listings, duplicate rejection, wrong-kind rejection, nil rejection.
func TestRegistryRegistration(t *testing.T) {
	t.Parallel()

	r, err := llm.NewRegistry(mustOpenAI(t), mustAnthropic(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := r.Names(); !reflect.DeepEqual(got, []string{llm.ProviderAnthropic, llm.ProviderOpenAI}) {
		t.Errorf("Names() = %v, want sorted [anthropic openai]", got)
	}
	if ms := r.Manifests(); len(ms) != 2 || ms[0].Kind != plugin.KindModelProvider {
		t.Errorf("Manifests() = %+v", ms)
	}
	if _, ok := r.Get(llm.ProviderOpenAI); !ok {
		t.Error("Get(openai) missed a registered provider")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) hit an unregistered provider")
	}

	// Duplicate name.
	if _, err := llm.NewRegistry(mustAnthropic(t), mustAnthropic(t)); err == nil {
		t.Error("NewRegistry with a duplicate provider: want error, got nil")
	}
	// Nil provider.
	if err := (&registryRegisterProbe{}).run(); err == nil {
		t.Error("Register(nil): want error, got nil")
	}
	// Wrong kind.
	if _, err := llm.NewRegistry(wrongKindProvider{}); err == nil {
		t.Error("NewRegistry with a non-model_provider manifest: want error, got nil")
	}
}

// registryRegisterProbe exercises Register(nil) without exporting a
// direct hook — NewRegistry with a nil variadic element.
type registryRegisterProbe struct{}

func (registryRegisterProbe) run() error {
	_, err := llm.NewRegistry(nil)
	return err
}

// wrongKindProvider is a Provider whose manifest lies about its kind.
type wrongKindProvider struct{}

func (wrongKindProvider) Chat(_ context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (wrongKindProvider) Manifest() plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindExecutor, Name: "openai", Version: "1.0.0"}
}

// TestRegistryResolve is the routing-table matrix: explicit provider,
// namespace-qualified model, vendor prefixes, unknown model, and
// unconfigured provider — each mapped to the right provider or the right
// typed error.
func TestRegistryResolve(t *testing.T) {
	t.Parallel()

	r, err := llm.NewRegistry(mustAnthropic(t), mustOpenAI(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	cases := []struct {
		name         string
		explicit     string
		model        string
		wantProvider string // provider Manifest().Name, or "" when an error is expected
		wantModel    string
	}{
		{"explicit provider wins over prefix", "anthropic", "gpt-4o", llm.ProviderAnthropic, "gpt-4o"},
		{"explicit openai", "openai", "anything", llm.ProviderOpenAI, "anything"},
		{"namespace anthropic", "", "anthropic/claude-sonnet-5", llm.ProviderAnthropic, "claude-sonnet-5"},
		{"namespace openai strips prefix", "", "openai/gpt-4o", llm.ProviderOpenAI, "gpt-4o"},
		{"bare claude prefix", "", "claude-sonnet-5", llm.ProviderAnthropic, "claude-sonnet-5"},
		{"bare gpt prefix", "", "gpt-4o-mini", llm.ProviderOpenAI, "gpt-4o-mini"},
		{"bare o1 prefix", "", "o1-preview", llm.ProviderOpenAI, "o1-preview"},
		{"bare o3 prefix", "", "o3-mini", llm.ProviderOpenAI, "o3-mini"},
		{"bare chatgpt prefix", "", "chatgpt-4o-latest", llm.ProviderOpenAI, "chatgpt-4o-latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, model, err := r.Resolve(tc.explicit, tc.model)
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", tc.explicit, tc.model, err)
			}
			if p.Manifest().Name != tc.wantProvider {
				t.Errorf("provider = %q, want %q", p.Manifest().Name, tc.wantProvider)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
		})
	}
}

// TestRegistryResolveErrors pins the two distinct typed errors.
func TestRegistryResolveErrors(t *testing.T) {
	t.Parallel()

	// Only Anthropic configured — OpenAI-routed models are unavailable,
	// unknown models are unknown.
	r, err := llm.NewRegistry(mustAnthropic(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Unknown model: no rule matches.
	_, _, err = r.Resolve("", "mystery-model-9000")
	var unknown *llm.UnknownModelError
	if !errors.As(err, &unknown) {
		t.Fatalf("Resolve(unknown model) error = %v (%T), want *UnknownModelError", err, err)
	}
	if unknown.Model != "mystery-model-9000" {
		t.Errorf("UnknownModelError.Model = %q", unknown.Model)
	}

	// Empty model with no explicit provider is unroutable.
	if _, _, err := r.Resolve("", ""); !errors.As(err, &unknown) {
		t.Errorf("Resolve(empty) error = %v, want *UnknownModelError", err)
	}

	// Provider routed but not configured: bare gpt- prefix → openai.
	_, _, err = r.Resolve("", "gpt-4o")
	var unavail *llm.ProviderUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("Resolve(gpt-4o) error = %v (%T), want *ProviderUnavailableError", err, err)
	}
	if unavail.Provider != llm.ProviderOpenAI || unavail.Model != "gpt-4o" {
		t.Errorf("ProviderUnavailableError = %+v, want {openai gpt-4o}", unavail)
	}

	// Explicit unknown provider is unavailable, not unknown-model.
	if _, _, err := r.Resolve("cohere", "command-r"); !errors.As(err, &unavail) {
		t.Errorf("Resolve(explicit cohere) error = %v, want *ProviderUnavailableError", err)
	}

	// Namespace form naming an unconfigured provider is unavailable.
	if _, _, err := r.Resolve("", "openai/gpt-4o"); !errors.As(err, &unavail) {
		t.Errorf("Resolve(openai/gpt-4o) error = %v, want *ProviderUnavailableError", err)
	}
}

// TestNewRegistryFromKeys pins the third acceptance criterion: providers
// configurable independently, either (or neither) absent without a boot
// error, and the catalog matching the configured set.
func TestNewRegistryFromKeys(t *testing.T) {
	t.Parallel()

	key := testKey()
	cases := []struct {
		name      string
		keys      llm.ProviderKeys
		wantNames []string
	}{
		{"neither", llm.ProviderKeys{}, nil},
		{"anthropic only", llm.ProviderKeys{Anthropic: key}, []string{llm.ProviderAnthropic}},
		{"openai only", llm.ProviderKeys{OpenAI: key}, []string{llm.ProviderOpenAI}},
		{"both", llm.ProviderKeys{Anthropic: key, OpenAI: key}, []string{llm.ProviderAnthropic, llm.ProviderOpenAI}},
		{"mock only", llm.ProviderKeys{Mock: &llm.MockConfig{}}, []string{llm.ProviderMock}},
		{"mock plus anthropic", llm.ProviderKeys{Anthropic: key, Mock: &llm.MockConfig{}}, []string{llm.ProviderAnthropic, llm.ProviderMock}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := llm.NewRegistryFromKeys(tc.keys)
			if err != nil {
				t.Fatalf("NewRegistryFromKeys: %v", err)
			}
			if got := r.Names(); !reflect.DeepEqual(nilToEmpty(got), nilToEmpty(tc.wantNames)) {
				t.Errorf("Names() = %v, want %v", got, tc.wantNames)
			}
		})
	}
}

// TestMockNamespaceRouting pins ticket 8.5's "registered like any
// provider (model: mock/...)": the reserved namespace form routes to the
// mock and hands it the model with the "mock/" prefix stripped.
func TestMockNamespaceRouting(t *testing.T) {
	t.Parallel()
	r, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	p, model, err := r.Resolve("", "mock/sim-1")
	if err != nil {
		t.Fatalf("Resolve(mock/sim-1): %v", err)
	}
	if model != "sim-1" {
		t.Errorf("canonical model = %q, want sim-1 (prefix stripped)", model)
	}
	if p.Manifest().Name != llm.ProviderMock {
		t.Errorf("routed to %q, want mock", p.Manifest().Name)
	}

	// Without the mock configured, the same address is an unavailable
	// provider, not an unknown model — the fix is "enable the mock".
	empty, err := llm.NewRegistryFromKeys(llm.ProviderKeys{})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys(empty): %v", err)
	}
	var unavail *llm.ProviderUnavailableError
	if _, _, err := empty.Resolve("", "mock/sim-1"); !errors.As(err, &unavail) {
		t.Errorf("Resolve(mock/sim-1) with no mock = %v, want *ProviderUnavailableError", err)
	}
}

func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
