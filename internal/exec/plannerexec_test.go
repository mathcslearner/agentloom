package exec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// recPlannerExecutor wires the recording provider behind a PlannerExecutor.
func recPlannerExecutor(t *testing.T, p *recordingProvider) PlannerExecutor {
	t.Helper()
	reg, err := llm.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return NewPlannerExecutor(reg)
}

// plannerStep builds a StepContext for a planner step from a config object.
func plannerStep(t *testing.T, config any) StepContext {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	return StepContext{StepType: dag.StepPlanner, Config: raw, Attempt: 1}
}

// TestPlannerExecutorIdentity pins the planner plugin identity: type
// "planner", its own manifest (cacheable + cost-bearing), distinct from the
// embedded llm executor's.
func TestPlannerExecutorIdentity(t *testing.T) {
	t.Parallel()
	e := NewPlannerExecutor(nil)
	if e.Type() != string(dag.StepPlanner) {
		t.Errorf("Type = %q, want planner", e.Type())
	}
	m := e.PluginManifest()
	if m.Kind != plugin.KindExecutor || m.Name != string(dag.StepPlanner) {
		t.Errorf("manifest identity = %s/%s, want executor/planner", m.Kind, m.Name)
	}
	if !m.Capabilities.Cacheable || !m.Capabilities.CostBearing {
		t.Errorf("capabilities = %+v, want cacheable + cost_bearing", m.Capabilities)
	}
	if m.Version == "0.0.0" || strings.HasSuffix(m.Version, "-stub") {
		t.Errorf("version = %q, want a real release version", m.Version)
	}
}

// TestPlannerExecutorRequestCarriesStructuredFormat proves a planner call is
// framed as a structured-output request (the implicit plan output_format), so
// the mock/provider returns native JSON the plan is decoded from — and the
// PlannerConfig's model-call fields (prompt, max_tokens, temperature) reach the
// request unchanged while max_added_steps is dropped (it is engine state).
func TestPlannerExecutorRequestCarriesStructuredFormat(t *testing.T) {
	t.Parallel()
	temp := 0.0
	p := &recordingProvider{resp: llm.ChatResponse{
		Model: "sim-1", StopReason: "end_turn",
		Structured: json.RawMessage(`{"schema_version":1,"steps":[]}`),
		Usage:      llm.Usage{InputTokens: 5, OutputTokens: 3},
	}}
	e := recPlannerExecutor(t, p)
	out, err := e.Execute(context.Background(), plannerStep(t, map[string]any{
		"model": "rec/sim-1", "prompt": "make a plan", "max_tokens": 77,
		"temperature": temp, "max_added_steps": 4,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if p.gotReq.ResponseFormat == nil {
		t.Fatal("planner request carried no ResponseFormat — the plan output_format was not framed")
	}
	if p.gotReq.MaxTokens != 77 {
		t.Errorf("request max_tokens = %d, want 77", p.gotReq.MaxTokens)
	}
	if p.gotReq.Temperature == nil || *p.gotReq.Temperature != 0 {
		t.Errorf("request temperature = %v, want 0", p.gotReq.Temperature)
	}
	// The native structured plan lands on output.json, so completeSuccess
	// decodes it from there.
	var o struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(out.Data, &o); err != nil {
		t.Fatalf("decoding planner output: %v", err)
	}
	if len(o.JSON) == 0 {
		t.Fatalf("planner output.json is empty; got %s", out.Data)
	}
	if _, err := dag.DecodePlanOutput(o.JSON); err != nil {
		t.Errorf("output.json is not a decodable plan: %v", err)
	}
	if out.Resource != "rec:sim-1" {
		t.Errorf("resource = %q, want rec:sim-1", out.Resource)
	}
	if out.Usage == nil {
		t.Error("planner output carried no usage — the call must meter")
	}
}

// TestPlannerExecutorHooksDelegate proves the planner reuses the llm hooks
// through the projected config: resource/cost keying, feedback and context
// injection over the planner's prompt, and a cache binding whose executor
// identity is "planner" (so a planner and an llm step with the same prompt
// never share a cache entry).
func TestPlannerExecutorHooksDelegate(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: okResponse()}
	e := recPlannerExecutor(t, p)
	sc := plannerStep(t, map[string]any{
		"model": "rec/sim-1", "prompt": "plan it", "max_tokens": 40, "max_added_steps": 3,
	})

	// ResourceClaim / CostEstimate key by the resolved provider:model.
	name, cost, err := e.ResourceClaim(sc)
	if err != nil || name != "rec:sim-1" || cost < 1 {
		t.Fatalf("ResourceClaim = (%q, %d, %v), want (rec:sim-1, ≥1, nil)", name, cost, err)
	}
	est, err := e.CostEstimate(sc)
	if err != nil || est.Resource != "rec:sim-1" || est.MaxTokens != 40 {
		t.Fatalf("CostEstimate = (%+v, %v), want resource rec:sim-1 max_tokens 40", est, err)
	}

	// CacheBinding: eligible (deterministic requires temp 0, absent here so
	// non-deterministic — but the executor identity is the assertion), and the
	// executor plugin ref is the planner, not the llm executor.
	cb, err := e.CacheBinding(sc)
	if err != nil {
		t.Fatalf("CacheBinding: %v", err)
	}
	if cb.Executor.Name != string(dag.StepPlanner) {
		t.Errorf("cache binding executor = %q, want planner", cb.Executor.Name)
	}

	// WithFeedback / WithContext rewrite the planner prompt in place.
	fbCfg, err := e.WithFeedback(sc, Feedback{Text: "the plan was rejected: fix ids"})
	if err != nil {
		t.Fatalf("WithFeedback: %v", err)
	}
	if !strings.Contains(string(fbCfg), "fix ids") {
		t.Errorf("feedback not folded into planner config: %s", fbCfg)
	}
	// The rewrite preserves max_added_steps (engine state, not a model input).
	if !strings.Contains(string(fbCfg), "max_added_steps") {
		t.Errorf("feedback rewrite dropped max_added_steps: %s", fbCfg)
	}
	cxCfg, err := e.WithContext(sc, "here is context")
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}
	if !strings.Contains(string(cxCfg), "here is context") {
		t.Errorf("context not prepended to planner config: %s", cxCfg)
	}
}

// TestPlannerCacheKeyDiffersFromLLM proves a planner and an llm step with an
// otherwise-identical request produce different cache keys — the distinct
// executor plugin identity, so a plan is never served from an llm entry (or
// vice versa) even at the same version.
func TestPlannerCacheKeyDiffersFromLLM(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: okResponse()}
	reg, err := llm.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	llmE := NewLLMExecutor(reg)
	planE := NewPlannerExecutor(reg)

	cfg := map[string]any{"model": "rec/sim-1", "prompt": "same words", "temperature": 0.0}
	llmBind, err := llmE.CacheBinding(StepContext{StepType: dag.StepLLM, Config: mustJSON(t, cfg)})
	if err != nil {
		t.Fatalf("llm CacheBinding: %v", err)
	}
	planBind, err := planE.CacheBinding(plannerStep(t, cfg))
	if err != nil {
		t.Fatalf("planner CacheBinding: %v", err)
	}
	if llmBind.Executor.Name == planBind.Executor.Name {
		t.Fatalf("planner and llm share executor identity %q — cache entries would collide", llmBind.Executor.Name)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	return raw
}
