//go:build integration

package store_test

// Ticket 6.5's registry-op tests: CreateDefinition / CreateDefinitionVersion
// semantics — canonical storage, version allocation serialized by the
// per-name advisory lock (concurrent appenders get consecutive versions),
// and the unseen-name / existing-name refusals.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// registryDef builds a minimal valid definition named name.
func registryDef(t *testing.T, name, echoMsg string) *dag.Definition {
	t.Helper()
	doc := fmt.Sprintf(`{
		"schema_version": 1,
		"name": %q,
		"steps": [{"id": "a", "type": "echo", "config": {"input": {"msg": %q}}}],
		"edges": []
	}`, name, echoMsg)
	def, err := dag.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	return def
}

func TestCreateDefinitionSemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := store.NewFromPool(storetest.NewDB(t))

	def := registryDef(t, "reg", "v1")
	row, err := s.CreateDefinition(ctx, def)
	if err != nil {
		t.Fatalf("CreateDefinition: %v", err)
	}
	if row.Name != "reg" || row.Version != 1 {
		t.Errorf("row = %s v%d, want reg v1", row.Name, row.Version)
	}
	// Stored canonically (modulo jsonb's formatting normalization): the
	// read-back spec decodes clean and re-encodes to the same canonical
	// bytes as the submitted definition.
	canonical, err := dag.Encode(def)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := dag.Decode(row.Spec)
	if err != nil {
		t.Fatalf("stored spec does not decode: %v", err)
	}
	storedCanonical, err := dag.Encode(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedCanonical, canonical) {
		t.Errorf("stored spec re-encodes differently:\n%s\nvs\n%s", storedCanonical, canonical)
	}

	// The name is taken: create again conflicts.
	var conflict *store.ConflictError
	if _, err := s.CreateDefinition(ctx, registryDef(t, "reg", "v1-again")); !errors.As(err, &conflict) {
		t.Fatalf("duplicate CreateDefinition error = %v, want *ConflictError", err)
	}

	// Appending works and allocates 2.
	v2, err := s.CreateDefinitionVersion(ctx, registryDef(t, "reg", "v2"), nil)
	if err != nil {
		t.Fatalf("CreateDefinitionVersion: %v", err)
	}
	if v2.Version != 2 {
		t.Errorf("appended version = %d, want 2", v2.Version)
	}

	// Appending to an unseen name is ErrNotFound, not a silent create.
	if _, err := s.CreateDefinitionVersion(ctx, registryDef(t, "ghost", "x"), nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unseen-name append error = %v, want ErrNotFound", err)
	}

	// If-Match precondition (ticket 17.6): the latest is now v2. An append that
	// asserts it opened at v1 (stale) is refused; asserting v2 (current) works.
	stale := int32(1)
	var vc *store.VersionConflictError
	if _, err := s.CreateDefinitionVersion(ctx, registryDef(t, "reg", "v3-stale"), &stale); !errors.As(err, &vc) {
		t.Fatalf("stale-precondition append error = %v, want *VersionConflictError", err)
	}
	if vc.Expected != 1 || vc.Latest != 2 {
		t.Errorf("VersionConflictError = %+v, want expected 1 latest 2", vc)
	}
	current := int32(2)
	v3, err := s.CreateDefinitionVersion(ctx, registryDef(t, "reg", "v3"), &current)
	if err != nil {
		t.Fatalf("matched-precondition append: %v", err)
	}
	if v3.Version != 3 {
		t.Errorf("appended version = %d, want 3", v3.Version)
	}

	// ListLatest shows only the head.
	latest, err := s.Definitions().ListLatest(ctx, nil, 10)
	if err != nil {
		t.Fatalf("ListLatest: %v", err)
	}
	if len(latest) != 1 || latest[0].Version != 3 {
		t.Errorf("ListLatest = %+v, want [reg v3]", latest)
	}
}

// TestCreateDefinitionVersionConcurrent proves the advisory lock
// serializes allocation: N concurrent appenders all succeed with distinct,
// gap-free versions.
func TestCreateDefinitionVersionConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := store.NewFromPool(storetest.NewDB(t))

	if _, err := s.CreateDefinition(ctx, registryDef(t, "contended", "v1")); err != nil {
		t.Fatalf("seeding v1: %v", err)
	}

	const appenders = 8
	versions := make([]int32, appenders)
	errs := make([]error, appenders)
	var wg sync.WaitGroup
	for i := range appenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			row, err := s.CreateDefinitionVersion(ctx, registryDef(t, "contended", fmt.Sprintf("v-%d", i)), nil)
			versions[i], errs[i] = row.Version, err
		}()
	}
	wg.Wait()

	seen := map[int32]bool{}
	for i := range appenders {
		if errs[i] != nil {
			t.Fatalf("appender %d failed: %v", i, errs[i])
		}
		if seen[versions[i]] {
			t.Errorf("version %d allocated twice", versions[i])
		}
		seen[versions[i]] = true
	}
	// Consecutive: exactly versions 2..appenders+1.
	for v := int32(2); v <= appenders+1; v++ {
		if !seen[v] {
			t.Errorf("version %d missing — allocation left a gap", v)
		}
	}
}
