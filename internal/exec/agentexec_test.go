package exec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// recAgentExecutor wires the recording provider behind an AgentExecutor.
func recAgentExecutor(t *testing.T, p *recordingProvider) AgentExecutor {
	t.Helper()
	reg, err := llm.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return NewAgentExecutor(reg)
}

// agentStep builds a StepContext for an agent step from a (merged) config
// object — instantiation materializes the merged AgentConfig, so tests pass the
// resolved shape the executor sees at claim time.
func agentStep(t *testing.T, config any) StepContext {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	return StepContext{StepType: dag.StepAgent, Config: raw, Attempt: 1}
}

// TestAgentExecutorIdentity pins the agent plugin identity: type "agent", its
// own manifest (cacheable + cost-bearing), distinct from the embedded llm
// executor's.
func TestAgentExecutorIdentity(t *testing.T) {
	t.Parallel()
	e := NewAgentExecutor(nil)
	if e.Type() != string(dag.StepAgent) {
		t.Errorf("Type = %q, want agent", e.Type())
	}
	m := e.PluginManifest()
	if m.Kind != plugin.KindExecutor || m.Name != string(dag.StepAgent) {
		t.Errorf("manifest identity = %s/%s, want executor/agent", m.Kind, m.Name)
	}
	if !m.Capabilities.Cacheable || !m.Capabilities.CostBearing {
		t.Errorf("capabilities = %+v, want cacheable + cost_bearing", m.Capabilities)
	}
	if m.Version == "0.0.0" || strings.HasSuffix(m.Version, "-stub") {
		t.Errorf("version = %q, want a real release version", m.Version)
	}
}

// TestAgentExecutorProjectsSystemPrompt proves the merged agent config's system
// prompt reaches the provider request (ADR-016) and the model-call fields are
// framed like an llm step — the "agent executes as a fully-configured LLM
// step" acceptance, at the executor layer.
func TestAgentExecutorProjectsSystemPrompt(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: okResponse()}
	e := recAgentExecutor(t, p)
	out, err := e.Execute(context.Background(), agentStep(t, map[string]any{
		"agent": "critic", "system": "you are terse", "model": "rec/sim-1",
		"prompt": "review", "max_tokens": 64,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if p.gotReq.System != "you are terse" {
		t.Errorf("request system = %q, want the role system prompt", p.gotReq.System)
	}
	if p.gotReq.MaxTokens != 64 {
		t.Errorf("request max_tokens = %d, want 64", p.gotReq.MaxTokens)
	}
	if out.Resource != "rec:sim-1" {
		t.Errorf("resource = %q, want rec:sim-1", out.Resource)
	}
	if out.Usage == nil {
		t.Error("agent output carried no usage — the call must meter")
	}
}

// TestAgentExecutorToolAllowlist proves the rejection-only enforcement (ticket
// 14.1): a completion naming a tool inside the allowed toolset succeeds, and one
// naming a tool outside it fails permanently.
func TestAgentExecutorToolAllowlist(t *testing.T) {
	t.Parallel()

	toolResp := func(name string) llm.ChatResponse {
		return llm.ChatResponse{
			Model: "sim-1", StopReason: "tool_use",
			Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{
				ID: "tu_1", Name: name, Input: json.RawMessage(`{}`),
			}}},
			Usage: llm.Usage{InputTokens: 3, OutputTokens: 2},
		}
	}

	t.Run("allowed tool passes", func(t *testing.T) {
		t.Parallel()
		e := recAgentExecutor(t, &recordingProvider{resp: toolResp("json_transform")})
		if _, err := e.Execute(context.Background(), agentStep(t, map[string]any{
			"agent": "critic", "model": "rec/sim-1", "prompt": "x",
			"tools": []string{"json_transform", "http_request"},
		})); err != nil {
			t.Fatalf("Execute with an allowed tool call: %v", err)
		}
	})

	t.Run("disallowed tool fails permanently", func(t *testing.T) {
		t.Parallel()
		e := recAgentExecutor(t, &recordingProvider{resp: toolResp("delete_everything")})
		_, err := e.Execute(context.Background(), agentStep(t, map[string]any{
			"agent": "critic", "model": "rec/sim-1", "prompt": "x",
			"tools": []string{"json_transform"},
		}))
		if err == nil {
			t.Fatal("Execute with a disallowed tool call: want a permanent error")
		}
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Class != dag.ClassPermanent {
			t.Errorf("error = %v, want permanent ClassifiedError", err)
		}
		if !strings.Contains(err.Error(), "delete_everything") {
			t.Errorf("error does not name the rejected tool: %v", err)
		}
	})

	t.Run("empty allowlist rejects any tool call", func(t *testing.T) {
		t.Parallel()
		e := recAgentExecutor(t, &recordingProvider{resp: toolResp("json_transform")})
		_, err := e.Execute(context.Background(), agentStep(t, map[string]any{
			"agent": "critic", "model": "rec/sim-1", "prompt": "x",
		}))
		if err == nil {
			t.Fatal("Execute with no allowed toolset: want a permanent error for any tool call")
		}
	})

	t.Run("no tool calls succeeds regardless of allowlist", func(t *testing.T) {
		t.Parallel()
		e := recAgentExecutor(t, &recordingProvider{resp: okResponse()})
		if _, err := e.Execute(context.Background(), agentStep(t, map[string]any{
			"agent": "critic", "model": "rec/sim-1", "prompt": "x",
		})); err != nil {
			t.Fatalf("Execute with a plain text completion: %v", err)
		}
	})
}

// TestAgentCacheKeyDiffersFromLLM proves an agent and an llm step with an
// otherwise-identical request produce different cache keys — the distinct
// executor plugin identity.
func TestAgentCacheKeyDiffersFromLLM(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: okResponse()}
	reg, err := llm.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	llmE := NewLLMExecutor(reg)
	agentE := NewAgentExecutor(reg)

	llmBind, err := llmE.CacheBinding(StepContext{StepType: dag.StepLLM, Config: mustJSON(t, map[string]any{
		"model": "rec/sim-1", "prompt": "same words", "temperature": 0.0,
	})})
	if err != nil {
		t.Fatalf("llm CacheBinding: %v", err)
	}
	agentBind, err := agentE.CacheBinding(agentStep(t, map[string]any{
		"agent": "critic", "model": "rec/sim-1", "prompt": "same words", "temperature": 0.0,
	}))
	if err != nil {
		t.Fatalf("agent CacheBinding: %v", err)
	}
	if llmBind.Executor.Name == agentBind.Executor.Name {
		t.Fatalf("agent and llm share executor identity %q — cache entries would collide", llmBind.Executor.Name)
	}
}

// TestAgentSystemReKeysCache proves the system prompt is a cache-key input, so
// two agents differing only in system prompt never share a cache entry.
func TestAgentSystemReKeysCache(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: okResponse()}
	e := recAgentExecutor(t, p)

	a, err := e.CacheBinding(agentStep(t, map[string]any{
		"agent": "critic", "model": "rec/sim-1", "prompt": "x", "system": "role A", "temperature": 0.0,
	}))
	if err != nil {
		t.Fatalf("CacheBinding A: %v", err)
	}
	b, err := e.CacheBinding(agentStep(t, map[string]any{
		"agent": "critic", "model": "rec/sim-1", "prompt": "x", "system": "role B", "temperature": 0.0,
	}))
	if err != nil {
		t.Fatalf("CacheBinding B: %v", err)
	}
	ar, ok := a.Request.(cache.LLMRequest)
	if !ok {
		t.Fatalf("agent cache request is %T, want cache.LLMRequest", a.Request)
	}
	br := b.Request.(cache.LLMRequest)
	if ar.System == br.System {
		t.Fatalf("system prompt did not reach the cache request (both %q)", ar.System)
	}
}
