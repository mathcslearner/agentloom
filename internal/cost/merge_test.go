package cost_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/cost"
)

func TestMergeReplacesByName(t *testing.T) {
	t.Parallel()

	base, err := cost.Parse([]byte(`{
		"schema_version": 1,
		"models": [
			{"name": "anthropic:claude-sonnet-5", "effective_from": "2025-01-01", "input_per_mtok": 3.0, "output_per_mtok": 15.0},
			{"name": "openai:*", "effective_from": "2025-01-01", "input_per_mtok": 2.5, "output_per_mtok": 10.0}
		],
		"fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}
	}`))
	if err != nil {
		t.Fatalf("Parse base: %v", err)
	}
	override, err := cost.Parse([]byte(`{
		"schema_version": 1,
		"models": [
			{"name": "anthropic:claude-sonnet-5", "effective_from": "2025-01-01", "input_per_mtok": 1.0, "output_per_mtok": 5.0},
			{"name": "mock:*", "effective_from": "2025-01-01", "input_per_mtok": 0.1, "output_per_mtok": 0.2}
		]
	}`))
	if err != nil {
		t.Fatalf("Parse override: %v", err)
	}

	merged := cost.Merge(base, override)
	at := date(t, "2025-06-01")

	// Overridden name takes the override's price.
	if r, _, ok := merged.Lookup("anthropic:claude-sonnet-5", at); !ok || r.InputPerMTok != 1.0 {
		t.Errorf("overridden sonnet = %+v ok=%v, want input 1.0", r, ok)
	}
	// Name only in base survives.
	if r, _, ok := merged.Lookup("openai:gpt-5", at); !ok || r.InputPerMTok != 2.5 {
		t.Errorf("base openai wildcard = %+v ok=%v, want input 2.5", r, ok)
	}
	// Name only in override is added.
	if r, _, ok := merged.Lookup("mock:sim-1", at); !ok || r.InputPerMTok != 0.1 {
		t.Errorf("override mock wildcard = %+v ok=%v, want input 0.1", r, ok)
	}
	// Override without a fallback inherits base's.
	if fb, ok := merged.Fallback(); !ok || fb.InputPerMTok != 30.0 {
		t.Errorf("merged fallback = %+v ok=%v, want base {30 60}", fb, ok)
	}

	// Merge must not mutate base.
	if r, _, _ := base.Lookup("anthropic:claude-sonnet-5", at); r.InputPerMTok != 3.0 {
		t.Errorf("base mutated by Merge: sonnet input = %v, want 3.0", r.InputPerMTok)
	}
}

func TestMergeReplacesFallback(t *testing.T) {
	t.Parallel()

	base, _ := cost.Parse([]byte(`{"schema_version": 1, "fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}}`))
	override, _ := cost.Parse([]byte(`{"schema_version": 1, "fallback": {"input_per_mtok": 99.0, "output_per_mtok": 99.0}}`))
	merged := cost.Merge(base, override)
	if fb, _ := merged.Fallback(); fb.InputPerMTok != 99.0 {
		t.Errorf("merged fallback input = %v, want 99.0 (override wins)", fb.InputPerMTok)
	}
}

func TestMergeNilOverride(t *testing.T) {
	t.Parallel()

	base, _ := cost.Parse([]byte(validCatalog))
	if cost.Merge(base, nil) != base {
		t.Error("Merge(base, nil) should return base unchanged")
	}
}
