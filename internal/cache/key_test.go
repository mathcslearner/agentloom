package cache_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// ptr returns a pointer to v — for the nil-vs-value temperature cases.
func ptr[T any](v T) *T { return &v }

// baselineLLM is a fixed llm key input the golden and semantic tests mutate.
func baselineLLM() cache.KeyInput {
	return cache.KeyInput{
		SchemaVersion: 1,
		Executor:      cache.PluginRef{Kind: plugin.KindExecutor, Name: "llm", Version: "1.0.0"},
		Plugin:        cache.PluginRef{Kind: plugin.KindModelProvider, Name: "anthropic", Version: "1.0.0"},
		Scope:         dag.CacheGlobal,
		Request: cache.LLMRequest{
			Model:       "claude-sonnet-5",
			System:      "Be concise.",
			Temperature: ptr(0.0),
			MaxTokens:   1024,
			Messages:    []byte(`[{"role":"user","content":"hello"}]`),
		},
	}
}

func mustKey(t *testing.T, in cache.KeyInput) string {
	t.Helper()
	k, err := cache.Key(in)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	return k
}

// TestKeyGolden pins the literal digest of a fixed input so an accidental
// change to the key format (component order, framing, canonicalization)
// fails loudly rather than silently orphaning or, worse, colliding with
// every previously written entry.
func TestKeyGolden(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		in   cache.KeyInput
		want string
	}{
		"llm": {
			in:   baselineLLM(),
			want: "630ea7ab4d2c5ef9ca5152c045ac2d054d5f711701918c4f0af2aec6f33dba95",
		},
		"tool": {
			in: cache.KeyInput{
				SchemaVersion: 1,
				Executor:      cache.PluginRef{Kind: plugin.KindExecutor, Name: "tool", Version: "1.0.0"},
				Plugin:        cache.PluginRef{Kind: plugin.KindTool, Name: "json_transform", Version: "1.0.0"},
				Request:       cache.ToolRequest{Input: []byte(`{"query":".foo","data":[1,2,3]}`)},
			},
			want: "2e2dc7deef3626c29580ebac90ba40b656f9cad8bfa757ce7c7716d7f689651f",
		},
		"retrieve": {
			in: cache.KeyInput{
				SchemaVersion: 1,
				Executor:      cache.PluginRef{Kind: plugin.KindExecutor, Name: "retrieve", Version: "1.0.0"},
				Plugin:        cache.PluginRef{Kind: plugin.KindRetriever, Name: "pg_fulltext", Version: "1.0.0"},
				Request:       cache.RetrieveRequest{Query: "durable execution", TopK: 5},
			},
			want: "7c7f3753ef158d4b67839e109946968efdc4d9eafa36cb73a2b9105155ac3412",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := mustKey(t, tc.in)
			if got != tc.want {
				t.Errorf("key = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestKeyStableUnderJSONReordering is the first 9.4 acceptance property:
// reordered object keys and different-but-equal number spellings in the
// request JSON produce the *same* key — the canonicalization guarantee.
func TestKeyStableUnderJSONReordering(t *testing.T) {
	t.Parallel()

	base := baselineLLM()
	base.Request = cache.LLMRequest{
		Model:     "m",
		MaxTokens: 1,
		Messages:  []byte(`[{"role":"user","content":"hi","meta":{"a":1,"b":2.0}}]`),
		Tools:     []byte(`[{"name":"t","input_schema":{"type":"object","x":1e2}}]`),
	}
	reordered := baselineLLM()
	reordered.Request = cache.LLMRequest{
		Model:     "m",
		MaxTokens: 1,
		// Same content: object keys reordered, 2.0 spelled 2, 1e2 spelled 100.
		Messages: []byte(`[{"content":"hi","meta":{"b":2,"a":1},"role":"user"}]`),
		Tools:    []byte(`[{"input_schema":{"x":100,"type":"object"},"name":"t"}]`),
	}
	if a, b := mustKey(t, base), mustKey(t, reordered); a != b {
		t.Errorf("reordered/renumbered JSON changed the key:\n %s\n %s", a, b)
	}
}

// TestKeyChangesOnSemanticChange is the second 9.4 acceptance property:
// any semantic change to the input yields a different key. Each variant
// mutates exactly one component of the baseline.
func TestKeyChangesOnSemanticChange(t *testing.T) {
	t.Parallel()

	base := mustKey(t, baselineLLM())

	variants := map[string]func(*cache.KeyInput){
		"schema version":   func(in *cache.KeyInput) { in.SchemaVersion = 2 },
		"executor version": func(in *cache.KeyInput) { in.Executor.Version = "1.0.1" },
		"executor name":    func(in *cache.KeyInput) { in.Executor.Name = "planner" },
		"plugin name":      func(in *cache.KeyInput) { in.Plugin.Name = "openai" },
		"plugin version":   func(in *cache.KeyInput) { in.Plugin.Version = "2.0.0" },
		"plugin kind":      func(in *cache.KeyInput) { in.Plugin.Kind = plugin.KindTool },
		"model": func(in *cache.KeyInput) {
			r := in.Request.(cache.LLMRequest)
			r.Model = "claude-opus-5"
			in.Request = r
		},
		"system":     func(in *cache.KeyInput) { r := in.Request.(cache.LLMRequest); r.System = "Be verbose."; in.Request = r },
		"max tokens": func(in *cache.KeyInput) { r := in.Request.(cache.LLMRequest); r.MaxTokens = 2048; in.Request = r },
		"temperature value": func(in *cache.KeyInput) {
			r := in.Request.(cache.LLMRequest)
			r.Temperature = ptr(0.5)
			in.Request = r
		},
		"temperature nil vs zero": func(in *cache.KeyInput) {
			r := in.Request.(cache.LLMRequest)
			r.Temperature = nil
			in.Request = r
		},
		"messages": func(in *cache.KeyInput) {
			r := in.Request.(cache.LLMRequest)
			r.Messages = []byte(`[{"role":"user","content":"goodbye"}]`)
			in.Request = r
		},
		"tools added": func(in *cache.KeyInput) {
			r := in.Request.(cache.LLMRequest)
			r.Tools = []byte(`[{"name":"t","input_schema":{"type":"object"}}]`)
			in.Request = r
		},
		"scope run": func(in *cache.KeyInput) { in.Scope = dag.CacheRun; in.RunID = "run-1" },
	}

	seen := map[string]string{base: "baseline"}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := baselineLLM()
			mutate(&in)
			got := mustKey(t, in)
			if got == base {
				t.Errorf("%q did not change the key", name)
			}
		})
		// Distinctness across variants (sequential map read is fine here —
		// the subtests above are the parallel part).
		in := baselineLLM()
		mutate(&in)
		got := mustKey(t, in)
		if other, dup := seen[got]; dup {
			t.Errorf("%q collides with %q", name, other)
		}
		seen[got] = name
	}
}

// TestKeyRunScopeIsolation confirms two runs with byte-identical requests
// get different run-scoped keys but the same global key.
func TestKeyRunScopeIsolation(t *testing.T) {
	t.Parallel()

	mk := func(scope dag.CacheScope, runID string) string {
		in := baselineLLM()
		in.Scope = scope
		in.RunID = runID
		return mustKey(t, in)
	}
	if a, b := mk(dag.CacheRun, "run-a"), mk(dag.CacheRun, "run-b"); a == b {
		t.Error("run-scoped keys did not isolate distinct runs")
	}
	if a, b := mk(dag.CacheGlobal, "run-a"), mk(dag.CacheGlobal, "run-b"); a != b {
		t.Error("global keys must ignore the run id")
	}
}

// TestKeySeparatorUnambiguous probes the length-prefix framing: shifting a
// byte across a component boundary must change the key, so component
// contents can never be confused with a boundary.
func TestKeySeparatorUnambiguous(t *testing.T) {
	t.Parallel()

	mk := func(model, system string) string {
		in := baselineLLM()
		r := in.Request.(cache.LLMRequest)
		r.Model, r.System = model, system
		in.Request = r
		return mustKey(t, in)
	}
	if a, b := mk("ab", ""), mk("a", "b"); a == b {
		t.Error("component boundary is ambiguous: (\"ab\",\"\") hashed equal to (\"a\",\"b\")")
	}
}

// TestKeyErrors covers the unusable inputs Key rejects rather than
// producing a weak or unsafe key.
func TestKeyErrors(t *testing.T) {
	t.Parallel()

	cases := map[string]func() cache.KeyInput{
		"no executor": func() cache.KeyInput { in := baselineLLM(); in.Executor = cache.PluginRef{}; return in },
		"no plugin":   func() cache.KeyInput { in := baselineLLM(); in.Plugin = cache.PluginRef{}; return in },
		"no version":  func() cache.KeyInput { in := baselineLLM(); in.Plugin.Version = ""; return in },
		"nil request": func() cache.KeyInput { in := baselineLLM(); in.Request = nil; return in },
		"run scope no run id": func() cache.KeyInput {
			in := baselineLLM()
			in.Scope = dag.CacheRun
			return in
		},
		"malformed request json": func() cache.KeyInput {
			in := baselineLLM()
			in.Request = cache.ToolRequest{Input: []byte(`{not json`)}
			return in
		},
		"unknown scope": func() cache.KeyInput {
			in := baselineLLM()
			in.Scope = dag.CacheScope("tenant")
			return in
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := cache.Key(mk()); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

// TestRedisKey pins the storage-key layout 9.6 busts by prefix.
func TestRedisKey(t *testing.T) {
	t.Parallel()

	p := cache.PluginRef{Kind: plugin.KindTool, Name: "json_transform", Version: "1.0.0"}
	got := cache.RedisKey("cache", p, "deadbeef")
	want := "cache:v1:tool:json_transform:deadbeef"
	if got != want {
		t.Errorf("RedisKey = %q, want %q", got, want)
	}
}
