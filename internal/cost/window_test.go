package cost_test

import (
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/cost"
)

// windowCatalog exercises context-window resolution: an exact model with a
// window, an exact rate-only model with no window (inherits nothing of its
// own), a wildcard carrying a window, and the fallback (no window).
const windowCatalog = `{
  "schema_version": 1,
  "models": [
    {"name": "anthropic:claude-sonnet-5", "effective_from": "2025-01-01", "context_window": 200000, "input_per_mtok": 3.0, "output_per_mtok": 15.0},
    {"name": "anthropic:claude-legacy",   "effective_from": "2025-01-01", "input_per_mtok": 3.0, "output_per_mtok": 15.0},
    {"name": "anthropic:*",               "effective_from": "2025-01-01", "context_window": 128000, "input_per_mtok": 3.0, "output_per_mtok": 15.0},
    {"name": "openai:gpt-legacy",         "effective_from": "2025-01-01", "input_per_mtok": 2.5, "output_per_mtok": 10.0}
  ],
  "fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}
}`

func TestContextWindowResolution(t *testing.T) {
	t.Parallel()

	cat, err := cost.Parse([]byte(windowCatalog))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cases := []struct {
		name       string
		resource   string
		wantWindow int64
		wantSrc    cost.Source
		wantOK     bool
	}{
		{"exact window", "anthropic:claude-sonnet-5", 200000, cost.SourceExact, true},
		{"rate-only model inherits wildcard window", "anthropic:claude-legacy", 128000, cost.SourceWildcard, true},
		{"wildcard directly", "anthropic:whatever", 128000, cost.SourceWildcard, true},
		{"no wildcard window and no fallback window", "openai:gpt-legacy", 0, cost.SourceExact, false},
		{"unknown provider", "cohere:command", 0, cost.SourceExact, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, src, ok := cat.ContextWindow(tc.resource, date(t, "2026-01-01"))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if w != tc.wantWindow || src != tc.wantSrc {
				t.Errorf("got window=%d src=%v, want window=%d src=%v", w, src, tc.wantWindow, tc.wantSrc)
			}
		})
	}
}

func TestContextWindowNilSafe(t *testing.T) {
	t.Parallel()
	var cat *cost.Catalog
	if w, _, ok := cat.ContextWindow("anthropic:claude-sonnet-5", date(t, "2026-01-01")); ok || w != 0 {
		t.Errorf("nil catalog ContextWindow = (%d, ok=%v), want (0, false)", w, ok)
	}
}

func TestContextWindowNegativeRejected(t *testing.T) {
	t.Parallel()
	const bad = `{
  "schema_version": 1,
  "models": [
    {"name": "mock:x", "effective_from": "2025-01-01", "context_window": -1, "input_per_mtok": 1.0, "output_per_mtok": 2.0}
  ],
  "fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}
}`
	_, err := cost.Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "context_window must be positive") {
		t.Fatalf("Parse err = %v, want context_window validation error", err)
	}
}

// TestDefaultCatalogWindows guards the embedded defaults: every real,
// non-wildcard model and the mock guardrail entries carry a positive window,
// so the M12.6 guardrail has something to compare against out of the box.
func TestDefaultCatalogWindows(t *testing.T) {
	t.Parallel()
	cat, err := cost.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	for _, res := range []string{
		"anthropic:claude-opus-5", "anthropic:claude-sonnet-5", "anthropic:claude-haiku-4-5",
		"openai:gpt-5", "openai:o3", "mock:small", "mock:sim-1",
	} {
		if w, _, ok := cat.ContextWindow(res, date(t, "2026-01-01")); !ok || w <= 0 {
			t.Errorf("default context window for %q = (%d, ok=%v), want a positive window", res, w, ok)
		}
	}
	// The small mock model exists to exercise the guardrail offline.
	if w, src, ok := cat.ContextWindow("mock:small", date(t, "2026-01-01")); !ok || src != cost.SourceExact || w != 1024 {
		t.Errorf("mock:small window = (%d, src=%v, ok=%v), want (1024, exact, true)", w, src, ok)
	}
}
