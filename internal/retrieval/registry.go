package retrieval

// Retriever registry (ticket 8.8, ADR-009). Registry is the typed facade
// over plugin.Registry for kind retriever — the pattern exec.Registry,
// llm.Registry, and tools.Registry established: type-safe lookup at the
// call site over the one shared registration discipline (invalid /
// duplicate → boot error) and listing shape. Unlike tools.Registry there
// is no per-retriever args schema to compile — retrievers declare no
// config schema (the retrieve step's config schema lives on the executor).

import (
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// Registry maps retriever names to their Retriever implementations. It is
// a typed facade over the generic plugin.Registry (kind retriever, name =
// retriever name; ADR-009), populated at startup and read-only afterwards,
// so lookups need no lock; Register is not safe for concurrent use.
type Registry struct {
	plugins *plugin.Registry
	byName  map[string]Retriever
}

// NewRegistry builds a registry from the given retrievers. It fails on the
// same conditions as Register (nil retriever, invalid manifest, wrong
// kind, duplicate name).
func NewRegistry(rs ...Retriever) (*Registry, error) {
	r := &Registry{plugins: plugin.NewRegistry(), byName: make(map[string]Retriever)}
	for _, rt := range rs {
		if err := r.Register(rt); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds one retriever under its manifest's name, rejecting a nil
// retriever, a manifest that fails validation or does not declare kind
// retriever, and a duplicate name — each a wiring bug worth failing
// startup over (ADR-009).
func (r *Registry) Register(rt Retriever) error {
	if rt == nil {
		return errors.New("retrieval: Register called with nil retriever")
	}
	m := rt.Manifest()
	if m.Kind != plugin.KindRetriever {
		return fmt.Errorf("retrieval: retriever %q declares plugin kind %q — retrievers must register as %q", m.Name, m.Kind, plugin.KindRetriever)
	}
	if err := r.plugins.Register(m, rt); err != nil {
		var dup *plugin.DuplicateError
		if errors.As(err, &dup) {
			return fmt.Errorf("retrieval: retriever %q registered twice", m.Name)
		}
		return fmt.Errorf("retrieval: registering retriever %q: %w", m.Name, err)
	}
	r.byName[m.Name] = rt
	return nil
}

// Get returns the retriever registered under name, or an
// *UnknownRetrieverError (unwrapping to ErrUnknownRetriever) when none is
// registered.
func (r *Registry) Get(name string) (Retriever, error) {
	rt, ok := r.byName[name]
	if !ok {
		return nil, &UnknownRetrieverError{Name: name}
	}
	return rt, nil
}

// Manifests returns every registered retriever's manifest, sorted — the
// slice the API folds into GET /v1/plugins.
func (r *Registry) Manifests() []plugin.Manifest {
	return r.plugins.ManifestsByKind(plugin.KindRetriever)
}

// Names returns every registered retriever name, sorted.
func (r *Registry) Names() []string {
	ms := r.Manifests()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}
