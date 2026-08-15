package exec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// toolConfig marshals a tool step config for the ResourceClaim tests.
func toolConfig(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	return raw
}

// TestLLMResourceClaim: the llm executor names "<provider>:<model>" by the
// resolved provider and estimates chars/4 + max_tokens.
func TestLLMResourceClaim(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})

	// prompt "hello" = 5 chars → (5+3)/4 = 2 input tokens; + max_tokens 100.
	res, est, err := e.ResourceClaim(llmStep(t, map[string]any{
		"model": "rec/sim-1", "prompt": "hello", "max_tokens": 100,
	}))
	if err != nil {
		t.Fatalf("ResourceClaim: %v", err)
	}
	if res != "rec:sim-1" {
		t.Errorf("resource = %q, want rec:sim-1", res)
	}
	if est != 102 {
		t.Errorf("est tokens = %d, want 102 (2 input + 100 max)", est)
	}
}

// TestLLMResourceClaimDefaultMaxTokens: absent max_tokens estimates against
// the executor's default bound.
func TestLLMResourceClaimDefaultMaxTokens(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})
	_, est, err := e.ResourceClaim(llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "hi"}))
	if err != nil {
		t.Fatalf("ResourceClaim: %v", err)
	}
	// "hi" = 2 chars → (2+3)/4 = 1; + default 1024.
	if est != 1+llmDefaultMaxTokens {
		t.Errorf("est = %d, want %d", est, 1+llmDefaultMaxTokens)
	}
}

// TestLLMResourceClaimRoutingFailure: an unroutable model returns an error so
// the middleware skips limiting and lets Execute classify.
func TestLLMResourceClaimRoutingFailure(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})
	if _, _, err := e.ResourceClaim(llmStep(t, map[string]any{"model": "unknown/model", "prompt": "x"})); err == nil {
		t.Fatal("ResourceClaim for an unroutable model: want error, got nil")
	}
}

// TestToolResourceClaim: a tool step names "tool:<name>" with a zero token
// estimate (requests-only governance).
func TestToolResourceClaim(t *testing.T) {
	t.Parallel()
	e := NewToolExecutor(nil) // ResourceClaim reads config only, no registry needed

	raw := toolConfig(t, map[string]any{"tool": "http_request", "input": map[string]any{}})
	res, est, err := e.ResourceClaim(StepContext{StepType: dag.StepTool, Config: raw, Attempt: 1})
	if err != nil {
		t.Fatalf("ResourceClaim: %v", err)
	}
	if res != "tool:http_request" {
		t.Errorf("resource = %q, want tool:http_request", res)
	}
	if est != 0 {
		t.Errorf("est = %d, want 0 (tools meter no tokens)", est)
	}
}

// TestToolResourceClaimMissingTool: a config missing the tool field errors.
func TestToolResourceClaimMissingTool(t *testing.T) {
	t.Parallel()
	e := NewToolExecutor(nil)
	raw := toolConfig(t, map[string]any{"input": map[string]any{}})
	if _, _, err := e.ResourceClaim(StepContext{StepType: dag.StepTool, Config: raw, Attempt: 1}); err == nil ||
		!strings.Contains(err.Error(), "tool") {
		t.Fatalf("ResourceClaim missing tool: want tool error, got %v", err)
	}
}
