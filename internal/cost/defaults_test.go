package cost_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/cost"
)

// TestEmbeddedDefaultsValid guards the build: an edit to defaults.json that
// breaks the schema, drops the fallback, or removes the mock wildcard fails
// here rather than at a worker's boot.
func TestEmbeddedDefaultsValid(t *testing.T) {
	t.Parallel()

	cat, err := cost.Default()
	if err != nil {
		t.Fatalf("embedded default catalog invalid: %v", err)
	}
	if cat.ModelCount() == 0 {
		t.Error("default catalog has no models")
	}
	if _, ok := cat.Fallback(); !ok {
		t.Error("default catalog must carry a fallback rate")
	}
	// The mock wildcard must price any mock model so the offline M10 exit
	// workflow shows cost without configuration.
	if r, src, ok := cat.Lookup("mock:sim-1", date(t, "2026-01-01")); !ok || src != cost.SourceWildcard || r.InputPerMTok <= 0 {
		t.Errorf("mock:* lookup = %+v src=%v ok=%v, want a positive wildcard rate", r, src, ok)
	}
	// A representative real model resolves.
	if _, _, ok := cat.Lookup("anthropic:claude-sonnet-5", date(t, "2026-01-01")); !ok {
		t.Error("default catalog should price anthropic:claude-sonnet-5")
	}
}

// TestDefaultShared confirms Default caches: repeated calls return the same
// instance (it is treated as immutable; Merge copies before overlaying).
func TestDefaultShared(t *testing.T) {
	t.Parallel()

	a, err := cost.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	b, _ := cost.Default()
	if a != b {
		t.Error("Default should return the same cached catalog")
	}
}
