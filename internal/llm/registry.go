package llm

// Model provider registry & routing (ticket 8.4, ADR-009). Registry is
// the typed facade over plugin.Registry for kind model_provider — the
// same pattern exec.Registry established for executors: type-safe lookup
// at the call site, the one shared registration discipline (invalid /
// duplicate → boot error) and listing shape underneath.
//
// Resolve is the routing entry point the 8.6 llm executor calls: it maps
// a step's (optional explicit provider, model) onto a registered
// Provider, returning the canonical model the provider should actually
// see. Unroutable models and unconfigured providers surface as two
// deliberately distinct typed errors so a misconfiguration is
// diagnosable ("no provider knows this model" vs. "this provider isn't
// configured") — both deterministic, so the executor maps them permanent.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// Registry maps provider names to their Provider implementations. It is
// a typed facade over the generic plugin.Registry (kind model_provider,
// name = provider name; ADR-009), populated at startup and read-only
// afterwards, so lookups need no lock; Register is not safe for
// concurrent use.
type Registry struct {
	plugins *plugin.Registry
}

// NewRegistry builds a registry from the given providers. It fails on
// the same conditions as Register (nil provider, invalid manifest,
// wrong kind, duplicate name).
func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{plugins: plugin.NewRegistry()}
	for _, p := range providers {
		if err := r.Register(p); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds one provider under its manifest's name, rejecting a nil
// provider, a manifest that fails validation or does not declare kind
// model_provider, and a duplicate name — each a wiring bug worth failing
// startup over (ADR-009).
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return errors.New("llm: Register called with nil provider")
	}
	m := p.Manifest()
	if m.Kind != plugin.KindModelProvider {
		return fmt.Errorf("llm: provider %q declares plugin kind %q — providers must register as %q", m.Name, m.Kind, plugin.KindModelProvider)
	}
	if err := r.plugins.Register(m, p); err != nil {
		var dup *plugin.DuplicateError
		if errors.As(err, &dup) {
			return fmt.Errorf("llm: provider %q registered twice", m.Name)
		}
		return fmt.Errorf("llm: registering provider %q: %w", m.Name, err)
	}
	return nil
}

// Get returns the provider registered under name, reporting a miss with
// ok=false.
func (r *Registry) Get(name string) (Provider, bool) {
	impl, _, ok := r.plugins.Lookup(plugin.KindModelProvider, name)
	if !ok {
		return nil, false
	}
	return impl.(Provider), true
}

// Manifests returns every registered provider's manifest, sorted — the
// slice the API folds into GET /v1/plugins.
func (r *Registry) Manifests() []plugin.Manifest {
	return r.plugins.ManifestsByKind(plugin.KindModelProvider)
}

// Names returns every registered provider name, sorted.
func (r *Registry) Names() []string {
	ms := r.Manifests()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// UnknownModelError reports that no routing rule matched a model — the
// ticket's required typed error. Deterministic: the 8.6 executor maps it
// permanent (no provider will ever route it).
type UnknownModelError struct {
	// Model is the model id that matched no rule.
	Model string
}

func (e *UnknownModelError) Error() string {
	return fmt.Sprintf("llm: no provider routes model %q", e.Model)
}

// ProviderUnavailableError reports that a model routed to a provider
// name that is not registered — i.e. its API key is absent. Kept
// distinct from UnknownModelError because the fix differs: configure the
// provider vs. use a known model. Deterministic within an attempt, so
// the executor maps it permanent.
type ProviderUnavailableError struct {
	// Provider is the routed-to provider name.
	Provider string
	// Model is the model that routed there (empty when routing was by an
	// explicit provider field with no model context).
	Model string
}

func (e *ProviderUnavailableError) Error() string {
	if e.Model == "" {
		return fmt.Sprintf("llm: provider %q is not configured", e.Provider)
	}
	return fmt.Sprintf("llm: model %q routes to provider %q, which is not configured", e.Model, e.Provider)
}

// modelPrefix is one vendor-prefix routing rule for bare model ids.
type modelPrefix struct {
	prefix   string
	provider string
}

// modelPrefixes is the vendor-prefix routing table for bare model ids
// (no explicit provider, no "<provider>/" namespace). Matched
// longest-prefix-first, so a more specific rule always wins over a
// shorter one that also matches. Extend this table as providers land.
var modelPrefixes = []modelPrefix{
	{"claude", ProviderAnthropic},
	{"gpt-", ProviderOpenAI},
	{"chatgpt-", ProviderOpenAI},
	{"o1", ProviderOpenAI},
	{"o3", ProviderOpenAI},
	{"o4", ProviderOpenAI},
}

// Resolve selects a provider for a step's model and returns the
// canonical model the provider should receive. Rules, in priority order:
//
//  1. explicitProvider != "" — route by that exact provider name; the
//     model is passed through unchanged. An unregistered name is a
//     *ProviderUnavailableError.
//  2. model of the form "<name>/<rest>" — the namespace-qualified form
//     (8.5's mock provider registers as "mock" and is addressed
//     "mock/..."); route by <name> and hand the provider <rest> (the
//     provider never sees its own namespace prefix).
//  3. bare model — the longest matching vendor prefix in modelPrefixes.
//
// A model matching no rule is *UnknownModelError; a rule that resolves
// to an unregistered provider is *ProviderUnavailableError.
func (r *Registry) Resolve(explicitProvider, model string) (Provider, string, error) {
	// Rule 1: explicit provider field wins.
	if explicitProvider != "" {
		p, ok := r.Get(explicitProvider)
		if !ok {
			return nil, "", &ProviderUnavailableError{Provider: explicitProvider, Model: model}
		}
		return p, model, nil
	}

	if model == "" {
		return nil, "", &UnknownModelError{Model: model}
	}

	// Rule 2: "<provider>/<model>" namespace form.
	if name, rest, ok := strings.Cut(model, "/"); ok && name != "" && rest != "" {
		p, found := r.Get(name)
		if !found {
			return nil, "", &ProviderUnavailableError{Provider: name, Model: model}
		}
		return p, rest, nil
	}

	// Rule 3: bare model — longest vendor-prefix match.
	if provider, ok := providerForModel(model); ok {
		p, found := r.Get(provider)
		if !found {
			return nil, "", &ProviderUnavailableError{Provider: provider, Model: model}
		}
		return p, model, nil
	}

	return nil, "", &UnknownModelError{Model: model}
}

// providerForModel returns the provider owning model's longest matching
// vendor prefix, or ok=false when none matches.
func providerForModel(model string) (string, bool) {
	best := ""
	provider := ""
	for _, mp := range modelPrefixes {
		if strings.HasPrefix(model, mp.prefix) && len(mp.prefix) > len(best) {
			best = mp.prefix
			provider = mp.provider
		}
	}
	return provider, best != ""
}

// ProviderKeys holds the per-provider API keys used to assemble a
// registry, one field per provider. An empty key leaves that provider
// unconstructed — never a boot error, so a deployment running only some
// providers (or none) starts cleanly (8.4's independent-configurability
// requirement). The 8.6 executor's Resolve then surfaces a call for an
// absent provider as a *ProviderUnavailableError.
type ProviderKeys struct {
	Anthropic string
	OpenAI    string
}

// NewRegistryFromKeys assembles the registry both deployables share:
// each provider is constructed iff its key is present, so the plugin
// catalog and the routable provider set match a deployment's actual
// configuration. Constructing zero providers is valid — the registry is
// simply empty. It errors only if a present key fails provider
// construction (which today cannot happen for a non-empty key).
func NewRegistryFromKeys(keys ProviderKeys) (*Registry, error) {
	var providers []Provider
	if keys.Anthropic != "" {
		p, err := NewAnthropic(AnthropicConfig{APIKey: keys.Anthropic})
		if err != nil {
			return nil, fmt.Errorf("llm: configuring anthropic: %w", err)
		}
		providers = append(providers, p)
	}
	if keys.OpenAI != "" {
		p, err := NewOpenAI(OpenAIConfig{APIKey: keys.OpenAI})
		if err != nil {
			return nil, fmt.Errorf("llm: configuring openai: %w", err)
		}
		providers = append(providers, p)
	}
	return NewRegistry(providers...)
}
