package exec

import (
	"encoding/json"
	"testing"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/tools"
)

// retrievalRegistry builds a retriever registry over a spy retriever with
// the given name.
func retrievalRegistry(t *testing.T, name string) (*retrieval.Registry, error) {
	t.Helper()
	return retrieval.NewRegistry(spyRetriever{name: name})
}

// TestLLMCacheBinding: the llm binder resolves the provider identity, builds
// the resolved request (default max_tokens, temperature pointer), and reports
// the provider's cacheable flag; temperature==0 is deterministic, nil is not.
func TestLLMCacheBinding(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: okResponse()}
	e := recExecutor(t, p)

	temp := 0.0
	b, err := e.CacheBinding(llmStep(t, map[string]any{
		"model": "rec/sim-1", "prompt": "say hi", "temperature": temp,
	}))
	if err != nil {
		t.Fatalf("CacheBinding: %v", err)
	}
	if b.Executor != (cache.PluginRef{Kind: plugin.KindExecutor, Name: "llm", Version: llmVersion}) {
		t.Errorf("executor ref = %+v", b.Executor)
	}
	if b.Plugin != (cache.PluginRef{Kind: plugin.KindModelProvider, Name: "rec", Version: "1.0.0"}) {
		t.Errorf("plugin ref = %+v", b.Plugin)
	}
	if b.Caps != p.Manifest().Capabilities {
		t.Errorf("caps = %+v, want the provider manifest's %+v", b.Caps, p.Manifest().Capabilities)
	}
	if !b.Deterministic {
		t.Error("temperature 0 must be deterministic")
	}
	lr, ok := b.Request.(cache.LLMRequest)
	if !ok {
		t.Fatalf("request type = %T, want cache.LLMRequest", b.Request)
	}
	if lr.Model != "sim-1" {
		t.Errorf("resolved model = %q, want sim-1 (prefix stripped)", lr.Model)
	}
	if lr.MaxTokens != llmDefaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", lr.MaxTokens, llmDefaultMaxTokens)
	}
	if lr.Temperature == nil || *lr.Temperature != 0 {
		t.Errorf("temperature = %v, want explicit 0", lr.Temperature)
	}

	// A binding whose key is buildable at all — proves the request bytes are
	// canonicalizable (no marshal failure on the rendered messages).
	if _, err := cache.Key(cache.KeyInput{SchemaVersion: 1, Executor: b.Executor, Plugin: b.Plugin, Request: b.Request}); err != nil {
		t.Errorf("cache.Key over the binding: %v", err)
	}

	// Nil temperature is non-deterministic (the provider default).
	b2, err := e.CacheBinding(llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "hi"}))
	if err != nil {
		t.Fatalf("CacheBinding (nil temp): %v", err)
	}
	if b2.Deterministic {
		t.Error("absent temperature must not be deterministic")
	}
}

// TestLLMCacheBindingUnresolvable: an unroutable model returns an error (the
// middleware then skips caching and lets Execute classify).
func TestLLMCacheBindingUnresolvable(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})
	if _, err := e.CacheBinding(llmStep(t, map[string]any{"model": "nope/x", "prompt": "hi"})); err == nil {
		t.Fatal("CacheBinding on an unroutable model = nil error, want an error")
	}
	// A nil provider registry also errors.
	if _, err := NewLLMExecutor(nil).CacheBinding(llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "hi"})); err == nil {
		t.Fatal("CacheBinding with nil registry = nil error, want an error")
	}
}

// TestToolCacheBindingUsesToolFlags is the ADR-011 rule: the tool executor is
// side_effectful, but the binding reports the INVOKED TOOL's flags — so a
// pure tool (json_transform) is cacheable/deterministic and a side-effectful
// one (http_request) is neither.
func TestToolCacheBindingUsesToolFlags(t *testing.T) {
	t.Parallel()
	reg, err := tools.NewBuiltins(tools.HTTPOptions{Allowlist: []string{"example.com"}})
	if err != nil {
		t.Fatalf("NewBuiltins: %v", err)
	}
	e := NewToolExecutor(reg)

	pure, err := e.CacheBinding(toolStep(t, "json_transform", json.RawMessage(`{"expr":".","input":1}`)))
	if err != nil {
		t.Fatalf("CacheBinding json_transform: %v", err)
	}
	if !pure.Caps.Cacheable || pure.Caps.SideEffectful || !pure.Deterministic {
		t.Errorf("json_transform binding caps=%+v deterministic=%v, want cacheable+pure+deterministic",
			pure.Caps, pure.Deterministic)
	}
	if pure.Plugin.Name != "json_transform" || pure.Plugin.Kind != plugin.KindTool {
		t.Errorf("plugin ref = %+v, want the tool identity", pure.Plugin)
	}

	side, err := e.CacheBinding(toolStep(t, "http_request", json.RawMessage(`{"url":"https://example.com"}`)))
	if err != nil {
		t.Fatalf("CacheBinding http_request: %v", err)
	}
	if !side.Caps.SideEffectful || side.Deterministic {
		t.Errorf("http_request binding caps=%+v deterministic=%v, want side-effectful+non-deterministic",
			side.Caps, side.Deterministic)
	}
}

// toolStep builds a StepContext for a tool step.
func toolStep(t *testing.T, tool string, input json.RawMessage) StepContext {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"tool": tool, "input": input})
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	return StepContext{StepType: dag.StepTool, Config: raw, Attempt: 1}
}

// TestToolCacheBindingErrors: an unknown tool and a nil registry error.
func TestToolCacheBindingErrors(t *testing.T) {
	t.Parallel()
	reg, err := tools.NewBuiltins(tools.HTTPOptions{})
	if err != nil {
		t.Fatalf("NewBuiltins: %v", err)
	}
	if _, err := NewToolExecutor(reg).CacheBinding(toolStep(t, "nope", nil)); err == nil {
		t.Fatal("CacheBinding on unknown tool = nil error, want an error")
	}
	if _, err := NewToolExecutor(nil).CacheBinding(toolStep(t, "json_transform", nil)); err == nil {
		t.Fatal("CacheBinding with nil registry = nil error, want an error")
	}
}

// TestRetrieveCacheBinding: the retrieve binder reports the retriever's
// identity and the resolved top_k, but Deterministic is always false — a
// corpus-mutable read is opt-in only (ADR-011).
func TestRetrieveCacheBinding(t *testing.T) {
	t.Parallel()
	reg, err := retrievalRegistry(t, "pg_fulltext")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	e := NewRetrieveExecutor(reg)
	b, err := e.CacheBinding(StepContext{StepType: dag.StepRetrieve, Config: retrieveStepConfig("pg_fulltext", "cats", nil)})
	if err != nil {
		t.Fatalf("CacheBinding: %v", err)
	}
	if b.Plugin.Name != "pg_fulltext" || b.Plugin.Kind != plugin.KindRetriever {
		t.Errorf("plugin ref = %+v, want the retriever identity", b.Plugin)
	}
	if b.Deterministic {
		t.Error("a retrieve binding must not be deterministic (corpus-mutable)")
	}
	rr, ok := b.Request.(cache.RetrieveRequest)
	if !ok {
		t.Fatalf("request type = %T, want cache.RetrieveRequest", b.Request)
	}
	if rr.TopK != defaultTopK {
		t.Errorf("resolved top_k = %d, want default %d", rr.TopK, defaultTopK)
	}
}

// ensure the binders satisfy the interface at compile time.
var (
	_ CacheBinder = LLMExecutor{}
	_ CacheBinder = ToolExecutor{}
	_ CacheBinder = RetrieveExecutor{}
)
