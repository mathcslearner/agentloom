package exec

import (
	"encoding/json"
	"testing"

	"github.com/mathcslearner/agentloom/internal/cache"
)

// TestLLMModelFallbacks reports the primary model and the ordered chain, and
// preserves each fallback's optional threshold.
func TestLLMModelFallbacks(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})

	frac := 0.8
	current, chain, err := e.ModelFallbacks(llmStep(t, map[string]any{
		"model":  "rec/sim-1",
		"prompt": "hi",
		"model_fallbacks": []map[string]any{
			{"model": "rec/cheap", "at_budget_fraction": frac},
			{"model": "rec/cheapest"},
		},
	}))
	if err != nil {
		t.Fatalf("ModelFallbacks: %v", err)
	}
	if current != "rec/sim-1" {
		t.Errorf("current = %q, want rec/sim-1", current)
	}
	if len(chain) != 2 {
		t.Fatalf("chain len = %d, want 2", len(chain))
	}
	if chain[0].Model != "rec/cheap" || chain[0].AtBudgetFraction == nil || *chain[0].AtBudgetFraction != frac {
		t.Errorf("chain[0] = %+v, want rec/cheap @0.8", chain[0])
	}
	if chain[1].Model != "rec/cheapest" || chain[1].AtBudgetFraction != nil {
		t.Errorf("chain[1] = %+v, want rec/cheapest with no threshold", chain[1])
	}

	// A step with no chain reports the primary and an empty chain.
	_, empty, err := e.ModelFallbacks(llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "hi"}))
	if err != nil {
		t.Fatalf("ModelFallbacks (no chain): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty chain len = %d, want 0", len(empty))
	}
}

// TestLLMWithModelReplacesOnlyModel rewrites the model field and leaves every
// other config value byte-identical.
func TestLLMWithModelReplacesOnlyModel(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})

	sc := llmStep(t, map[string]any{
		"model": "rec/sim-1", "prompt": "keep me", "max_tokens": 321, "temperature": 0.0,
		"model_fallbacks": []map[string]any{{"model": "rec/cheap"}},
	})
	out, err := e.WithModel(sc, "rec/cheap")
	if err != nil {
		t.Fatalf("WithModel: %v", err)
	}
	var got, orig map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal rewritten: %v", err)
	}
	if err := json.Unmarshal(sc.Config, &orig); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if string(got["model"]) != `"rec/cheap"` {
		t.Errorf("model = %s, want \"rec/cheap\"", got["model"])
	}
	for k, v := range orig {
		if k == "model" {
			continue
		}
		if string(got[k]) != string(v) {
			t.Errorf("field %q changed: %s -> %s", k, v, got[k])
		}
	}
}

// TestDowngradeChangesCacheKey is the ticket-10.4 assertion that a different
// model yields a different cache key: build the binding on the primary and on
// the WithModel-rewritten fallback config, and confirm the content keys differ
// while nothing else in the request does.
func TestDowngradeChangesCacheKey(t *testing.T) {
	t.Parallel()
	// The recording provider resolves any "rec/<model>" via the namespace
	// form, so both models route; the cache key hashes the resolved model, so
	// the two keys differ on that alone.
	e := recExecutor(t, &recordingProvider{resp: okResponse()})

	sc := llmStep(t, map[string]any{
		"model": "rec/sim-1", "prompt": "identical prompt", "temperature": 0.0,
	})
	primary, err := e.CacheBinding(sc)
	if err != nil {
		t.Fatalf("CacheBinding primary: %v", err)
	}
	rewritten, err := e.WithModel(sc, "rec/cheap")
	if err != nil {
		t.Fatalf("WithModel: %v", err)
	}
	sc.Config = rewritten
	fallback, err := e.CacheBinding(sc)
	if err != nil {
		t.Fatalf("CacheBinding fallback: %v", err)
	}

	kp, err := cache.Key(cache.KeyInput{SchemaVersion: 1, Executor: primary.Executor, Plugin: primary.Plugin, Request: primary.Request})
	if err != nil {
		t.Fatalf("Key primary: %v", err)
	}
	kf, err := cache.Key(cache.KeyInput{SchemaVersion: 1, Executor: fallback.Executor, Plugin: fallback.Plugin, Request: fallback.Request})
	if err != nil {
		t.Fatalf("Key fallback: %v", err)
	}
	if kp == kf {
		t.Errorf("downgrade did not change the cache key: both = %s", kp)
	}
}
