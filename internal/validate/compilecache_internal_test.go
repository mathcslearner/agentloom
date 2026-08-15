package validate

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// TestCompileCacheReuse proves a compileCache builds an artifact once per
// distinct config and reuses it thereafter, even under concurrent access —
// the mechanism behind "compiled artifacts reused across attempts".
func TestCompileCacheReuse(t *testing.T) {
	t.Parallel()
	var built int
	c := newCompileCache(func(config []byte) (int, error) {
		return len(config), nil // a stand-in "compile"
	})
	cfg := []byte(`{"pattern":"x"}`)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.get(cfg); err != nil {
				t.Errorf("get: %v", err)
			}
		}()
	}
	wg.Wait()
	_ = built

	if got := c.compileCount(); got != 1 {
		t.Errorf("compiled %d times for one config, want 1 (a duplicate racing compile is tolerated but this run should coalesce)", got)
	}

	// A different config compiles once more.
	if _, err := c.get([]byte(`{"pattern":"y"}`)); err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got := c.compileCount(); got != 2 {
		t.Errorf("compileCount = %d after a second distinct config, want 2", got)
	}
}

// TestValidatorCompilesOncePerConfig checks each compiling built-in
// (json_schema, regex, cel) compiles a given config exactly once across many
// Validate calls — the per-attempt-recompilation guard, asserted on the real
// validators through their private caches.
func TestValidatorCompilesOncePerConfig(t *testing.T) {
	t.Parallel()

	t.Run("regex", func(t *testing.T) {
		v := NewRegex()
		cfg := json.RawMessage(`{"pattern":"^ok"}`)
		for i := 0; i < 100; i++ {
			if _, err := v.Validate(context.Background(), Input{Value: json.RawMessage(`"ok go"`), Config: cfg, Attempt: 1}); err != nil {
				t.Fatalf("Validate %d: %v", i, err)
			}
		}
		if got := v.cache.compileCount(); got != 1 {
			t.Errorf("regex compiled %d times, want 1", got)
		}
	})

	t.Run("cel", func(t *testing.T) {
		v := NewCEL()
		cfg := json.RawMessage(`{"expr":"size(value) > 1"}`)
		for i := 0; i < 100; i++ {
			if _, err := v.Validate(context.Background(), Input{Value: json.RawMessage(`"abc"`), Config: cfg, Attempt: 1}); err != nil {
				t.Fatalf("Validate %d: %v", i, err)
			}
		}
		if got := v.cache.compileCount(); got != 1 {
			t.Errorf("cel compiled %d times, want 1", got)
		}
	})

	t.Run("json_schema", func(t *testing.T) {
		v := NewJSONSchema()
		cfg := json.RawMessage(`{"schema":{"type":"object"}}`)
		for i := 0; i < 100; i++ {
			if _, err := v.Validate(context.Background(), Input{Value: json.RawMessage(`{"a":1}`), Config: cfg, Attempt: 1}); err != nil {
				t.Fatalf("Validate %d: %v", i, err)
			}
		}
		if got := v.cache.compileCount(); got != 1 {
			t.Errorf("json_schema compiled %d times, want 1", got)
		}
	})
}
