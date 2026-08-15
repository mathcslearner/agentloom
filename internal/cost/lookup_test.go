package cost_test

import (
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/cost"
)

// multiDateCatalog has one model repriced across two effective dates and a
// wildcard, for effective-date and resolution-order tests.
const multiDateCatalog = `{
  "schema_version": 1,
  "models": [
    {"name": "anthropic:claude-sonnet-5", "effective_from": "2025-01-01", "input_per_mtok": 3.0, "output_per_mtok": 15.0},
    {"name": "anthropic:claude-sonnet-5", "effective_from": "2025-06-01", "input_per_mtok": 2.0, "output_per_mtok": 10.0},
    {"name": "anthropic:*", "effective_from": "2025-03-01", "input_per_mtok": 5.0, "output_per_mtok": 25.0}
  ],
  "fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}
}`

func TestEffectiveDateSelection(t *testing.T) {
	t.Parallel()

	cat, err := cost.Parse([]byte(multiDateCatalog))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cases := []struct {
		name    string
		at      string
		wantIn  float64
		wantSrc cost.Source
		wantOK  bool
	}{
		{"before any entry", "2024-12-31", 0, cost.SourceExact, false},
		{"exactly on first date", "2025-01-01", 3.0, cost.SourceExact, true},
		{"between dates", "2025-05-31", 3.0, cost.SourceExact, true},
		{"exactly on second date", "2025-06-01", 2.0, cost.SourceExact, true},
		{"after second date", "2025-12-01", 2.0, cost.SourceExact, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, src, ok := cat.Lookup("anthropic:claude-sonnet-5", date(t, tc.at))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if r.InputPerMTok != tc.wantIn || src != tc.wantSrc {
				t.Errorf("got input=%v src=%v, want input=%v src=%v", r.InputPerMTok, src, tc.wantIn, tc.wantSrc)
			}
		})
	}
}

func TestLookupWildcardFallthrough(t *testing.T) {
	t.Parallel()

	cat, err := cost.Parse([]byte(multiDateCatalog))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// A model with no exact entry falls to the provider wildcard...
	r, src, ok := cat.Lookup("anthropic:claude-opus-5", date(t, "2025-06-01"))
	if !ok || src != cost.SourceWildcard || r.InputPerMTok != 5.0 {
		t.Errorf("wildcard lookup = %+v src=%v ok=%v, want {5 25} wildcard true", r, src, ok)
	}
	// ...but only once the wildcard entry is itself effective.
	if _, _, ok := cat.Lookup("anthropic:claude-opus-5", date(t, "2025-02-01")); ok {
		t.Error("wildcard should not resolve before its own effective_from")
	}
	// A different provider does not borrow another provider's wildcard.
	if _, _, ok := cat.Lookup("openai:gpt-5", date(t, "2025-06-01")); ok {
		t.Error("openai model should not match anthropic wildcard")
	}
}

func TestPriceModelPolicy(t *testing.T) {
	t.Parallel()

	cat, err := cost.Parse([]byte(multiDateCatalog))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	at := date(t, "2025-06-01")

	// Known model: priced exactly, no fallback flag, either policy.
	for _, pol := range []cost.UnknownModelPolicy{cost.PolicyEstimate, cost.PolicyFail} {
		p, err := cat.PriceModel("anthropic:claude-sonnet-5", at, pol)
		if err != nil {
			t.Fatalf("known model under policy %v: %v", pol, err)
		}
		if p.Fallback || p.Source != cost.SourceExact || p.Rate.InputPerMTok != 2.0 {
			t.Errorf("known model priced %+v, want exact {2 10} no-fallback", p)
		}
	}

	// Unknown model, estimate policy: fallback rate + warning flag.
	p, err := cat.PriceModel("cohere:command", at, cost.PolicyEstimate)
	if err != nil {
		t.Fatalf("unknown model estimate: %v", err)
	}
	if !p.Fallback || p.Source != cost.SourceFallback || p.Rate.InputPerMTok != 30.0 {
		t.Errorf("unknown model estimate = %+v, want fallback {30 60} flagged", p)
	}

	// Unknown model, fail policy: typed error.
	_, err = cat.PriceModel("cohere:command", at, cost.PolicyFail)
	var unknown *cost.UnknownModelError
	if !errors.As(err, &unknown) {
		t.Fatalf("unknown model fail = %v, want *UnknownModelError", err)
	}
	if unknown.Model != "cohere:command" {
		t.Errorf("error model = %q, want cohere:command", unknown.Model)
	}
}

func TestPriceModelNoFallback(t *testing.T) {
	t.Parallel()

	// A directly-parsed override with no fallback cannot estimate an unknown
	// model even under PolicyEstimate.
	cat, err := cost.Parse([]byte(`{"schema_version": 1, "models": [{"name": "a:b", "effective_from": "2025-01-01", "input_per_mtok": 1, "output_per_mtok": 1}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = cat.PriceModel("x:y", date(t, "2025-06-01"), cost.PolicyEstimate)
	var unknown *cost.UnknownModelError
	if !errors.As(err, &unknown) {
		t.Fatalf("no-fallback estimate = %v, want *UnknownModelError", err)
	}
}

func TestUnknownModelWarningPayload(t *testing.T) {
	t.Parallel()

	cat, err := cost.Parse([]byte(multiDateCatalog))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p, err := cat.PriceModel("cohere:command", date(t, "2025-06-01"), cost.PolicyEstimate)
	if err != nil {
		t.Fatalf("PriceModel: %v", err)
	}
	w := cost.NewUnknownModelWarning("cohere:command", p.Rate)
	if w.Model != "cohere:command" || w.Fallback.InputPerMTok != 30.0 {
		t.Errorf("warning = %+v, want model cohere:command fallback {30 ...}", w)
	}
}

func TestNilCatalogSafe(t *testing.T) {
	t.Parallel()

	var cat *cost.Catalog
	if _, _, ok := cat.Lookup("a:b", date(t, "2025-01-01")); ok {
		t.Error("nil catalog Lookup should miss")
	}
	if _, ok := cat.ToolPrice("tool:x", date(t, "2025-01-01")); ok {
		t.Error("nil catalog ToolPrice should miss")
	}
	if cat.ModelCount() != 0 || cat.ToolCount() != 0 {
		t.Error("nil catalog counts should be zero")
	}
	if _, ok := cat.Fallback(); ok {
		t.Error("nil catalog Fallback should be absent")
	}
}
