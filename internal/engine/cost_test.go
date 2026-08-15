package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// priceAt is a fixed query time after every embedded catalog effective_from
// (2025-08-01), so effective-date selection lands on the current entries.
var priceAt = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// testCatalog is the embedded default catalog (prices mock:* at 1.0/2.0
// per-MTok, carries a fallback).
func testCatalog(t *testing.T) *cost.Catalog {
	t.Helper()
	c, err := cost.Default()
	if err != nil {
		t.Fatalf("cost.Default: %v", err)
	}
	return c
}

// engineWith returns a bare Engine carrying just the pricing catalog — enough
// to exercise priceAttempt, which reads no other engine state.
func engineWith(cat *cost.Catalog) *Engine {
	return &Engine{pricing: cat}
}

func stepRow() gen.RunStep {
	return gen.RunStep{RunID: uuid.New(), StepID: "gen", AttemptCount: 1}
}

func TestPriceAttempt(t *testing.T) {
	t.Parallel()
	cat := testCatalog(t)
	// mock:* wildcard: input 1.0/MTok, output 2.0/MTok.
	mockRate := cost.Rate{InputPerMTok: 1.0, OutputPerMTok: 2.0}

	t.Run("nil catalog ledgers nothing", func(t *testing.T) {
		e := engineWith(nil)
		out := exec.Output{Resource: "mock:sim-1", Usage: &exec.Usage{InputTokens: 100, OutputTokens: 50}}
		if got := e.priceAttempt(t.Context(), stepRow(), out, priceAt); got != nil {
			t.Errorf("nil catalog: got %+v, want nil", got)
		}
	})

	t.Run("no resource ledgers nothing", func(t *testing.T) {
		e := engineWith(cat)
		out := exec.Output{Usage: &exec.Usage{InputTokens: 100, OutputTokens: 50}}
		if got := e.priceAttempt(t.Context(), stepRow(), out, priceAt); got != nil {
			t.Errorf("empty resource: got %+v, want nil", got)
		}
	})

	t.Run("llm attempt priced by tokens", func(t *testing.T) {
		e := engineWith(cat)
		out := exec.Output{Resource: "mock:sim-1", Usage: &exec.Usage{InputTokens: 1000, OutputTokens: 500}}
		got := e.priceAttempt(t.Context(), stepRow(), out, priceAt)
		if got == nil {
			t.Fatal("got nil, want a cost row")
		}
		want := cost.Cost(1000, 500, mockRate) // 1000*1000 + 500*2000 = 2,000,000 nano
		if got.CostNanoUSD != want {
			t.Errorf("CostNanoUSD = %d, want %d", got.CostNanoUSD, want)
		}
		if got.SavedNanoUSD != 0 {
			t.Errorf("SavedNanoUSD = %d, want 0", got.SavedNanoUSD)
		}
		if got.CacheHit {
			t.Error("CacheHit = true, want false")
		}
		if got.RateSource != "wildcard" {
			t.Errorf("RateSource = %q, want wildcard", got.RateSource)
		}
		if got.Warning != nil {
			t.Errorf("Warning = %s, want none (known model)", got.Warning)
		}
	})

	t.Run("cache hit costs zero, saves the counterfactual", func(t *testing.T) {
		e := engineWith(cat)
		out := exec.Output{Resource: "mock:sim-1", Usage: &exec.Usage{InputTokens: 1000, OutputTokens: 500, CacheHit: true}}
		got := e.priceAttempt(t.Context(), stepRow(), out, priceAt)
		if got == nil {
			t.Fatal("got nil, want a cost row")
		}
		if got.CostNanoUSD != 0 {
			t.Errorf("CostNanoUSD = %d, want 0 (cache hit)", got.CostNanoUSD)
		}
		if want := cost.Cost(1000, 500, mockRate); got.SavedNanoUSD != want {
			t.Errorf("SavedNanoUSD = %d, want %d", got.SavedNanoUSD, want)
		}
		if !got.CacheHit {
			t.Error("CacheHit = false, want true")
		}
	})

	t.Run("llm attempt without usage ledgers nothing", func(t *testing.T) {
		e := engineWith(cat)
		out := exec.Output{Resource: "mock:sim-1"} // no usage
		if got := e.priceAttempt(t.Context(), stepRow(), out, priceAt); got != nil {
			t.Errorf("no usage: got %+v, want nil", got)
		}
	})

	t.Run("unknown model priced at fallback with a warning", func(t *testing.T) {
		e := engineWith(cat)
		out := exec.Output{Resource: "acme:unpriced", Usage: &exec.Usage{InputTokens: 1000, OutputTokens: 500}}
		got := e.priceAttempt(t.Context(), stepRow(), out, priceAt)
		if got == nil {
			t.Fatal("got nil, want a fallback-priced row")
		}
		if got.RateSource != "fallback" {
			t.Errorf("RateSource = %q, want fallback", got.RateSource)
		}
		if got.Warning == nil {
			t.Fatal("Warning = nil, want a cost_unknown_model payload")
		}
		var w cost.UnknownModelWarning
		if err := json.Unmarshal(got.Warning, &w); err != nil {
			t.Fatalf("unmarshaling warning: %v", err)
		}
		if w.Model != "acme:unpriced" {
			t.Errorf("warning model = %q, want acme:unpriced", w.Model)
		}
		// Fallback rate from the embedded defaults (30/60 per MTok).
		if want := cost.Cost(1000, 500, w.Fallback); got.CostNanoUSD != want {
			t.Errorf("CostNanoUSD = %d, want %d (fallback rate)", got.CostNanoUSD, want)
		}
	})

	t.Run("unknown model cache hit stays silent", func(t *testing.T) {
		e := engineWith(cat)
		out := exec.Output{Resource: "acme:unpriced", Usage: &exec.Usage{InputTokens: 1000, OutputTokens: 500, CacheHit: true}}
		got := e.priceAttempt(t.Context(), stepRow(), out, priceAt)
		if got == nil {
			t.Fatal("got nil, want a row")
		}
		if got.Warning != nil {
			t.Errorf("Warning = %s, want none (a hit spent nothing; the miss already warned)", got.Warning)
		}
		if got.CostNanoUSD != 0 {
			t.Errorf("CostNanoUSD = %d, want 0", got.CostNanoUSD)
		}
	})

	t.Run("priced tool charges a flat per-call cost", func(t *testing.T) {
		// A catalog with a priced tool; models fall through to the fallback.
		priced := mustCatalog(t, `{
			"schema_version": 1,
			"models": [{"name":"mock:*","effective_from":"2025-08-01","input_per_mtok":1.0,"output_per_mtok":2.0}],
			"tools": [{"name":"tool:paid_search","effective_from":"2025-08-01","per_call_usd":0.01}],
			"fallback": {"input_per_mtok":30.0,"output_per_mtok":60.0}
		}`)
		e := engineWith(priced)
		out := exec.Output{Resource: "tool:paid_search"}
		got := e.priceAttempt(t.Context(), stepRow(), out, priceAt)
		if got == nil {
			t.Fatal("got nil, want a tool cost row")
		}
		if got.CostNanoUSD != cost.ToolCost(0.01) {
			t.Errorf("CostNanoUSD = %d, want %d", got.CostNanoUSD, cost.ToolCost(0.01))
		}
		if len(got.Usage) != 0 {
			t.Errorf("tool row Usage = %s, want none", got.Usage)
		}
		if got.RateSource != "exact" {
			t.Errorf("RateSource = %q, want exact", got.RateSource)
		}
	})

	t.Run("unpriced tool is free (no row)", func(t *testing.T) {
		e := engineWith(cat) // embedded defaults carry no tool entries
		out := exec.Output{Resource: "tool:json_transform"}
		if got := e.priceAttempt(t.Context(), stepRow(), out, priceAt); got != nil {
			t.Errorf("unpriced tool: got %+v, want nil (free)", got)
		}
	})
}

// mustCatalog parses a catalog document or fails the test.
func mustCatalog(t *testing.T, doc string) *cost.Catalog {
	t.Helper()
	c, err := cost.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("cost.Parse: %v", err)
	}
	return c
}
