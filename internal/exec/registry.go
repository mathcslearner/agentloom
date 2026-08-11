package exec

import (
	"errors"
	"fmt"
	"slices"
)

// Registry maps step types to their executors. It is populated at worker
// startup and read-only afterwards, so lookups need no lock; Register is
// not safe for concurrent use.
type Registry struct {
	execs map[string]Executor
}

// NewRegistry builds a registry from the given executors. It fails on the
// same conditions as Register (nil executor, empty type, duplicate type).
func NewRegistry(execs ...Executor) (*Registry, error) {
	r := &Registry{execs: make(map[string]Executor, len(execs))}
	for _, e := range execs {
		if err := r.Register(e); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds one executor, rejecting a nil executor, an empty type,
// and a type registered twice — each of those is a wiring bug worth
// failing startup over, not shadowing silently.
func (r *Registry) Register(e Executor) error {
	if e == nil {
		return errors.New("exec: Register called with nil executor")
	}
	t := e.Type()
	if t == "" {
		return fmt.Errorf("exec: executor %T has empty type", e)
	}
	if _, dup := r.execs[t]; dup {
		return fmt.Errorf("exec: executor type %q registered twice", t)
	}
	r.execs[t] = e
	return nil
}

// Get returns the executor for stepType, or an *UnknownTypeError
// (unwrapping to ErrUnknownType) when none is registered.
func (r *Registry) Get(stepType string) (Executor, error) {
	e, ok := r.execs[stepType]
	if !ok {
		return nil, &UnknownTypeError{Type: stepType}
	}
	return e, nil
}

// Types returns every registered step type, sorted. It exists for
// exhaustiveness checks against the dag catalog (and, come M8, plugin
// listing) — execution paths look up by type, never enumerate.
func (r *Registry) Types() []string {
	types := make([]string, 0, len(r.execs))
	for t := range r.execs {
		types = append(types, t)
	}
	slices.Sort(types)
	return types
}
