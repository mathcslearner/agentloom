package exec

import (
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// SelfDescribing is the optional plugin half of an executor (ticket 8.1,
// ADR-009): production executors implement it to carry their manifest —
// version, capability flags, description — and the registry validates
// that the manifest's identity matches the executor (kind executor, name
// equal to Type()). Executors without it stay registrable (the engine's
// many test doubles), receiving a synthesized conservative manifest;
// TestBuiltinManifestConformance pins that everything in the shipped
// builtin sets self-describes.
type SelfDescribing interface {
	PluginManifest() plugin.Manifest
}

// Registry maps step types to their executors. Since ticket 8.1 it is a
// typed facade over the generic plugin registry (kind executor, name =
// step type; ADR-009): the 4.1 surface — NewRegistry, Register, Get,
// Types — is unchanged, and Manifests exposes the plugin listing the API
// serves. It is populated at worker startup and read-only afterwards, so
// lookups need no lock; Register is not safe for concurrent use.
type Registry struct {
	plugins *plugin.Registry
}

// NewRegistry builds a registry from the given executors. It fails on the
// same conditions as Register (nil executor, empty type, duplicate type,
// invalid manifest).
func NewRegistry(execs ...Executor) (*Registry, error) {
	r := &Registry{plugins: plugin.NewRegistry()}
	for _, e := range execs {
		if err := r.Register(e); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds one executor, rejecting a nil executor, an empty type, a
// type registered twice, and — for self-describing executors (ticket
// 8.1) — a manifest that fails validation or claims an identity other
// than the executor's own. Each of those is a wiring bug worth failing
// startup over, not shadowing silently.
func (r *Registry) Register(e Executor) error {
	if e == nil {
		return errors.New("exec: Register called with nil executor")
	}
	t := e.Type()
	if t == "" {
		return fmt.Errorf("exec: executor %T has empty type", e)
	}
	m, err := manifestFor(e, t)
	if err != nil {
		return err
	}
	if err := r.plugins.Register(m, e); err != nil {
		var dup *plugin.DuplicateError
		if errors.As(err, &dup) {
			return fmt.Errorf("exec: executor type %q registered twice", t)
		}
		return fmt.Errorf("exec: registering executor type %q: %w", t, err)
	}
	return nil
}

// manifestFor resolves e's manifest: its own when it self-describes
// (identity-checked — a plugin may not register under a name it does not
// execute), a synthesized conservative one otherwise (ADR-009: version
// 0.0.0, side_effectful true so nothing skips journaling for an unknown
// plugin, cacheable false so nothing serves it from cache). Either way a
// nil config schema is filled from the dag catalog's config struct when
// the type is a catalog step type — generated from the same struct the
// decoder uses, so it cannot drift.
func manifestFor(e Executor, t string) (plugin.Manifest, error) {
	var m plugin.Manifest
	if sd, ok := e.(SelfDescribing); ok {
		m = sd.PluginManifest()
		if m.Kind != plugin.KindExecutor {
			return m, fmt.Errorf("exec: executor type %q declares plugin kind %q — executors must register as %q", t, m.Kind, plugin.KindExecutor)
		}
		if m.Name != t {
			return m, fmt.Errorf("exec: executor type %q declares manifest name %q — a plugin may not claim another identity", t, m.Name)
		}
	} else {
		m = plugin.Manifest{
			Kind:         plugin.KindExecutor,
			Name:         t,
			Version:      "0.0.0",
			Capabilities: plugin.Capabilities{SideEffectful: true},
		}
	}
	if m.ConfigSchema == nil {
		if schema, err := dag.StepConfigSchema(dag.StepType(t)); err == nil {
			m.ConfigSchema = schema
		}
		// A non-catalog type (test doubles) simply has no schema to
		// attach; TestBuiltinManifestConformance pins that every shipped
		// builtin carries one.
	}
	return m, nil
}

// Get returns the executor for stepType, or an *UnknownTypeError
// (unwrapping to ErrUnknownType) when none is registered.
func (r *Registry) Get(stepType string) (Executor, error) {
	impl, _, ok := r.plugins.Lookup(plugin.KindExecutor, stepType)
	if !ok {
		return nil, &UnknownTypeError{Type: stepType}
	}
	return impl.(Executor), nil
}

// Types returns every registered step type, sorted. It exists for
// exhaustiveness checks against the dag catalog and the plugin listing —
// execution paths look up by type, never enumerate.
func (r *Registry) Types() []string {
	ms := r.plugins.ManifestsByKind(plugin.KindExecutor)
	types := make([]string, 0, len(ms))
	for _, m := range ms {
		types = append(types, m.Name)
	}
	return types
}

// Manifests returns every registered executor's plugin manifest in the
// canonical listing order (ticket 8.1): the catalog cmd/api serves on
// GET /v1/plugins and cmd/worker logs at boot.
func (r *Registry) Manifests() []plugin.Manifest {
	return r.plugins.Manifests()
}
