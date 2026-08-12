package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// CreateDefinition registers a new workflow definition under its spec name
// at version 1 (ticket 6.5). The definition is validated and stored in
// canonical encoding, like CreateRun's snapshot. A name that already exists
// fails with a *ConflictError on the (name, version) constraint — appending
// to an existing name is CreateDefinitionVersion's.
func (s *Store) CreateDefinition(ctx context.Context, def *dag.Definition) (gen.WorkflowDefinition, error) {
	spec, err := canonicalSpec(def)
	if err != nil {
		return gen.WorkflowDefinition{}, fmt.Errorf("store: CreateDefinition: %w", err)
	}
	row, err := s.Definitions().Create(ctx, gen.CreateDefinitionParams{Name: def.Name, Version: 1, Spec: spec})
	if err != nil {
		return gen.WorkflowDefinition{}, err
	}
	return row, nil
}

// CreateDefinitionVersion appends the next version of an existing name
// (ticket 6.5). The name must already be registered — an unseen name fails
// with ErrNotFound (use CreateDefinition). Allocation runs in one
// transaction serialized by a per-name advisory lock, so concurrent
// appenders get consecutive versions instead of racing the (name, version)
// constraint (MAX+1 alone cannot lock rows that do not exist yet).
func (s *Store) CreateDefinitionVersion(ctx context.Context, def *dag.Definition) (gen.WorkflowDefinition, error) {
	spec, err := canonicalSpec(def)
	if err != nil {
		return gen.WorkflowDefinition{}, fmt.Errorf("store: CreateDefinitionVersion: %w", err)
	}
	var row gen.WorkflowDefinition
	txErr := s.WithTx(ctx, func(ctx context.Context, q Querier) error {
		if err := q.Definitions().LockName(ctx, def.Name); err != nil {
			return err
		}
		version, err := q.Definitions().NextVersion(ctx, def.Name)
		if err != nil {
			return err
		}
		if version == 1 {
			return fmt.Errorf("store: CreateDefinitionVersion: definition %q: %w", def.Name, ErrNotFound)
		}
		row, err = q.Definitions().Create(ctx, gen.CreateDefinitionParams{Name: def.Name, Version: version, Spec: spec})
		return err
	})
	if txErr != nil {
		return gen.WorkflowDefinition{}, txErr
	}
	return row, nil
}

// definitionNameLockKey hashes a definition name into the advisory-lock
// key space serializing that name's version allocation. Domain-prefixed so
// it cannot systematically collide with other advisory-lock users (the
// reconciler's fleet lock).
func definitionNameLockKey(name string) int64 {
	h := fnv.New64a()
	h.Write([]byte("agentloom:definition-version:")) //nolint:errcheck // fnv never errors
	h.Write([]byte(name))                            //nolint:errcheck // fnv never errors
	return int64(h.Sum64())                          //nolint:gosec // wraparound is fine for a lock key
}

// canonicalSpec validates the definition and returns its canonical
// encoding — what the registry stores, so a stored spec always decodes and
// byte-compares against other canonical encodings.
func canonicalSpec(def *dag.Definition) ([]byte, error) {
	if def == nil {
		return nil, errors.New("nil definition")
	}
	if _, err := dag.Validate(def); err != nil {
		return nil, err
	}
	spec, err := dag.Encode(def)
	if err != nil {
		return nil, fmt.Errorf("encoding spec: %w", err)
	}
	return spec, nil
}
