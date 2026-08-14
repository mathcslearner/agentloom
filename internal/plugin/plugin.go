// Package plugin defines the shared vocabulary of agentloom's plugin SPI
// (ticket 8.1, ADR-009): the plugin kinds, the self-describing Manifest
// every plugin carries (identity, version, capability flags, config
// schema), and the generic Registry the kind-owning packages wrap in
// typed facades (internal/exec for executors today; llm/tools/validate
// as their milestones land).
//
// This is deliberately a leaf package — it imports none of the
// kind-owning packages, so every one of them can import it without
// cycles. Plugins are ordinary Go code compiled into the binaries
// (ADR-009's in-process model); registries are populated at boot and
// read-only afterwards, and an invalid or duplicate registration is a
// startup failure, never a silent shadow.
package plugin

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// Kind is a plugin's kind — the closed ADR-009 vocabulary. A plugin's
// identity is the (kind, name) pair; names are unique per kind.
type Kind string

// The five plugin kinds (ADR-009). Executors are the only kind populated
// in 8.1; tools (8.7), retrievers (8.8), model providers (8.3/8.4), and
// validators (M11) register through the same SPI as they land.
const (
	KindExecutor      Kind = "executor"
	KindTool          Kind = "tool"
	KindRetriever     Kind = "retriever"
	KindModelProvider Kind = "model_provider"
	KindValidator     Kind = "validator"
)

// kinds is the vocabulary in documentation order; it also fixes the sort
// order of cross-kind listings.
var kinds = []Kind{KindExecutor, KindTool, KindRetriever, KindModelProvider, KindValidator}

// Kinds returns the kind vocabulary in documentation order.
func Kinds() []Kind { return append([]Kind(nil), kinds...) }

// Valid reports whether k is one of the five ADR-009 kinds.
func (k Kind) Valid() bool {
	for _, known := range kinds {
		if k == known {
			return true
		}
	}
	return false
}

// kindOrder is k's position in the documentation order, for sorting.
func kindOrder(k Kind) int {
	for i, known := range kinds {
		if k == known {
			return i
		}
	}
	return len(kinds)
}

// Capabilities are ADR-009's three capability flags — exactly what the
// M9 middleware (cache, rate limits) and M10 cost ledger must know about
// a plugin without executing it. Flags describe the plugin's contract,
// not today's implementation shortcut: the dev stubs carry the flags of
// the real semantics that replace them in place.
type Capabilities struct {
	// SideEffectful: the plugin performs externally observable side
	// effects. Executions must go through the side-effect journal /
	// idempotency keys (5.5), and outputs must never be served from cache.
	SideEffectful bool `json:"side_effectful"`
	// Cacheable: the output is a function of (config, input) AND caching
	// is semantically sound — stronger than "deterministic" (sleep is
	// pure but a cache hit would skip the wait), and satisfiable by
	// non-deterministic plugins (LLM calls are exactly what M9's response
	// cache exists to reuse).
	Cacheable bool `json:"cacheable"`
	// CostBearing: executing the plugin spends money (provider tokens,
	// paid APIs); M10 meters these.
	CostBearing bool `json:"cost_bearing"`
}

// nameRE is the plugin naming rule: step-type spelling, since an
// executor plugin's name IS its step type.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// maxNameLen bounds plugin names.
const maxNameLen = 64

// versionRE is the semver shape (MAJOR.MINOR.PATCH with optional
// pre-release/build suffix). Deliberately a format check, not a parser:
// nothing orders versions yet — M9's cache keys only need equality
// (ADR-009).
var versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z][0-9A-Za-z.-]*)?(\+[0-9A-Za-z][0-9A-Za-z.-]*)?$`)

// Manifest is a plugin's self-description (ADR-009): identity, version,
// capability flags, and the JSON Schema of its config. The registry
// validates it at registration and the API serves it verbatim on
// GET /v1/plugins.
type Manifest struct {
	// Kind is the plugin's kind; with Name it forms the identity.
	Kind Kind
	// Name identifies the plugin within its kind: ^[a-z][a-z0-9_]*$, at
	// most 64 characters. For executors, the name is the step type.
	Name string
	// Version is the plugin's semver version string. It feeds M9's cache
	// keys, so the bump rule is behavioral: bump whenever a change should
	// invalidate previously cached outputs.
	Version string
	// Description is an optional one-line human description.
	Description string
	// Capabilities are the ADR-009 capability flags.
	Capabilities Capabilities
	// ConfigSchema is the generated JSON Schema (2020-12) for the
	// plugin's config, as served to UI forms; nil means the plugin takes
	// no config. When present it must be a JSON object. Generated from
	// the same Go structs the decoders use — never hand-written.
	ConfigSchema json.RawMessage
}

// Validate checks the manifest against ADR-009's rules. The zero
// manifest is invalid; a nil ConfigSchema is fine (no config).
func (m Manifest) Validate() error {
	if !m.Kind.Valid() {
		return fmt.Errorf("kind %q is not a plugin kind", m.Kind)
	}
	if m.Name == "" {
		return errors.New("name is empty")
	}
	if len(m.Name) > maxNameLen {
		return fmt.Errorf("name %q exceeds %d characters", m.Name, maxNameLen)
	}
	if !nameRE.MatchString(m.Name) {
		return fmt.Errorf("name %q does not match %s", m.Name, nameRE)
	}
	if !versionRE.MatchString(m.Version) {
		return fmt.Errorf("version %q is not a semver version string", m.Version)
	}
	if m.ConfigSchema != nil {
		trimmed := bytes.TrimSpace(m.ConfigSchema)
		if !json.Valid(trimmed) || len(trimmed) == 0 || trimmed[0] != '{' {
			return errors.New("config schema is not a JSON object")
		}
	}
	return nil
}

// ErrInvalidManifest is the sentinel every manifest rejection unwraps
// to; errors.As an *InvalidManifestError for the offender.
var ErrInvalidManifest = errors.New("invalid plugin manifest")

// ErrDuplicate is the sentinel every duplicate registration unwraps to;
// errors.As a *DuplicateError for the offending identity.
var ErrDuplicate = errors.New("duplicate plugin registration")

// InvalidManifestError reports a manifest that failed Validate, or an
// impl-level registration rule (nil impl, facade-enforced identity).
type InvalidManifestError struct {
	// Kind and Name identify the offender as far as they were usable.
	Kind  Kind
	Name  string
	cause error
}

func (e *InvalidManifestError) Error() string {
	return fmt.Sprintf("plugin: invalid manifest for %s %q: %v", e.Kind, e.Name, e.cause)
}

func (e *InvalidManifestError) Unwrap() error { return ErrInvalidManifest }

// DuplicateError reports a (kind, name) registered twice.
type DuplicateError struct {
	Kind Kind
	Name string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("plugin: %s %q registered twice", e.Kind, e.Name)
}

func (e *DuplicateError) Unwrap() error { return ErrDuplicate }

// entry is one registration.
type entry struct {
	manifest Manifest
	impl     any
}

// Registry is the generic plugin registry keyed by (kind, name). It is
// populated at startup and read-only afterwards, so lookups need no
// lock; Register is not safe for concurrent use. Kind-owning packages
// wrap it in typed facades (exec.Registry for executors) — Lookup's
// untyped impl is theirs to assert.
type Registry struct {
	entries map[Kind]map[string]entry
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[Kind]map[string]entry)}
}

// Register adds one plugin under its manifest's (kind, name), rejecting
// a nil impl, an invalid manifest, and a duplicate identity — each a
// wiring bug worth failing startup over (ADR-009).
func (r *Registry) Register(m Manifest, impl any) error {
	if err := m.Validate(); err != nil {
		return &InvalidManifestError{Kind: m.Kind, Name: m.Name, cause: err}
	}
	if impl == nil {
		return &InvalidManifestError{Kind: m.Kind, Name: m.Name, cause: errors.New("nil implementation")}
	}
	byName := r.entries[m.Kind]
	if byName == nil {
		byName = make(map[string]entry)
		r.entries[m.Kind] = byName
	}
	if _, dup := byName[m.Name]; dup {
		return &DuplicateError{Kind: m.Kind, Name: m.Name}
	}
	byName[m.Name] = entry{manifest: m, impl: impl}
	return nil
}

// Lookup returns the registered implementation and manifest for
// (kind, name), reporting a miss with ok=false — the typed facades turn
// that into their kind's established error (exec's *UnknownTypeError).
func (r *Registry) Lookup(k Kind, name string) (impl any, m Manifest, ok bool) {
	e, ok := r.entries[k][name]
	return e.impl, e.manifest, ok
}

// Manifests returns every registered manifest, sorted by kind (in the
// documentation order) then name. This is GET /v1/plugins' listing
// order.
func (r *Registry) Manifests() []Manifest {
	var out []Manifest
	for _, k := range kinds {
		out = append(out, r.ManifestsByKind(k)...)
	}
	return out
}

// ManifestsByKind returns one kind's manifests, sorted by name.
func (r *Registry) ManifestsByKind(k Kind) []Manifest {
	byName := r.entries[k]
	out := make([]Manifest, 0, len(byName))
	for _, e := range byName {
		out = append(out, e.manifest)
	}
	sortManifests(out)
	return out
}

// SortManifests sorts manifests by kind (documentation order) then name
// — the canonical listing order, exported for callers that aggregate
// manifests from several registries.
func SortManifests(ms []Manifest) { sortManifests(ms) }

func sortManifests(ms []Manifest) {
	slices.SortFunc(ms, func(a, b Manifest) int {
		if c := cmp.Compare(kindOrder(a.Kind), kindOrder(b.Kind)); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}
