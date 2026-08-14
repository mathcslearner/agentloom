package exec

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// recordingProvider is a controllable llm.Provider for executor unit
// tests: it captures the request the executor built and returns a scripted
// response or error, so tests assert both directions offline. Registered
// under name "rec" and reached via the namespace model form "rec/<model>".
type recordingProvider struct {
	gotReq llm.ChatRequest
	resp   llm.ChatResponse
	err    error
}

func (p *recordingProvider) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	p.gotReq = req
	if p.err != nil {
		return llm.ChatResponse{}, p.err
	}
	return p.resp, nil
}

func (*recordingProvider) Manifest() plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindModelProvider, Name: "rec", Version: "1.0.0", Description: "test recorder"}
}

// recExecutor wires the recording provider behind an LLMExecutor.
func recExecutor(t *testing.T, p *recordingProvider) LLMExecutor {
	t.Helper()
	reg, err := llm.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return NewLLMExecutor(reg)
}

// llmStep builds a StepContext for an llm step from a config object.
func llmStep(t *testing.T, config any) StepContext {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	return StepContext{StepType: dag.StepLLM, Config: raw, Attempt: 1}
}

func okResponse() llm.ChatResponse {
	return llm.ChatResponse{
		Model:      "sim-1",
		StopReason: "end_turn",
		Blocks:     []llm.Block{llm.TextBlock("hello back")},
		Usage:      llm.Usage{InputTokens: 11, OutputTokens: 7},
	}
}

func TestLLMExecutorPromptRequestAndOutput(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: okResponse()}
	e := recExecutor(t, p)

	temp := 0.0
	out, err := e.Execute(context.Background(), llmStep(t, map[string]any{
		"model": "rec/sim-1", "prompt": "say hi", "max_tokens": 55, "temperature": temp,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Request: the prompt became one user message, the model was routed to
	// its canonical (prefix-stripped) id, and max_tokens/temperature passed
	// through verbatim.
	if p.gotReq.Model != "sim-1" {
		t.Errorf("routed model = %q, want sim-1", p.gotReq.Model)
	}
	if p.gotReq.MaxTokens != 55 {
		t.Errorf("max_tokens = %d, want 55", p.gotReq.MaxTokens)
	}
	if p.gotReq.Temperature == nil || *p.gotReq.Temperature != 0.0 {
		t.Errorf("temperature = %v, want 0.0", p.gotReq.Temperature)
	}
	if len(p.gotReq.Messages) != 1 || p.gotReq.Messages[0].Role != llm.RoleUser ||
		p.gotReq.Messages[0].Blocks[0].Text != "say hi" {
		t.Errorf("messages = %+v, want one user text 'say hi'", p.gotReq.Messages)
	}

	// Output: the persisted shape plus usage on the Output for the attempt row.
	var got llmOutput
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("unmarshaling output: %v", err)
	}
	if got.Model != "sim-1" || got.StopReason != "end_turn" || got.Text != "hello back" {
		t.Errorf("output = %+v, want model=sim-1 stop=end_turn text='hello back'", got)
	}
	if got.Usage != (Usage{InputTokens: 11, OutputTokens: 7}) {
		t.Errorf("output usage = %+v", got.Usage)
	}
	if out.Usage == nil || *out.Usage != (Usage{InputTokens: 11, OutputTokens: 7}) {
		t.Errorf("Output.Usage = %+v, want {11,7}", out.Usage)
	}
}

func TestLLMExecutorMaxTokensDefaultAndMessages(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: okResponse()}
	e := recExecutor(t, p)

	// max_tokens omitted → the executor's default; messages map onto roles.
	_, err := e.Execute(context.Background(), llmStep(t, map[string]any{
		"model": "rec/sim-1",
		"messages": []map[string]string{
			{"role": "user", "content": "u1"},
			{"role": "assistant", "content": "a1"},
			{"role": "user", "content": "u2"},
		},
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if p.gotReq.MaxTokens != llmDefaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", p.gotReq.MaxTokens, llmDefaultMaxTokens)
	}
	if len(p.gotReq.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(p.gotReq.Messages))
	}
	if p.gotReq.Messages[1].Role != llm.RoleAssistant || p.gotReq.Messages[1].Blocks[0].Text != "a1" {
		t.Errorf("messages[1] = %+v, want assistant 'a1'", p.gotReq.Messages[1])
	}
}

func TestLLMExecutorToolCallsInOutput(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{resp: llm.ChatResponse{
		Model:      "sim-1",
		StopReason: "tool_use",
		Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{
			ID: "tu_1", Name: "search", Input: json.RawMessage(`{"q":"go"}`),
		}}},
		Usage: llm.Usage{InputTokens: 3, OutputTokens: 2},
	}}
	e := recExecutor(t, p)

	out, err := e.Execute(context.Background(), llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "search"}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got llmOutput
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("unmarshaling output: %v", err)
	}
	// Text is always present (empty here — no text blocks), so a downstream
	// `.output.text` reference never misses.
	if got.Text != "" {
		t.Errorf("text = %q, want empty", got.Text)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "tu_1" || got.ToolCalls[0].Name != "search" ||
		string(got.ToolCalls[0].Input) != `{"q":"go"}` {
		t.Errorf("tool_calls = %+v", got.ToolCalls)
	}
}

func TestLLMExecutorProviderErrorClassMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want dag.ErrorClass
	}{
		{"transient 429", &llm.Error{Provider: "rec", Class: dag.ClassTransient, Status: 429}, dag.ClassTransient},
		{"permanent 400", &llm.Error{Provider: "rec", Class: dag.ClassPermanent, Status: 400}, dag.ClassPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := recExecutor(t, &recordingProvider{err: tc.err})
			_, err := e.Execute(context.Background(), llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "x"}))
			var ce *ClassifiedError
			if !errors.As(err, &ce) {
				t.Fatalf("error %v is not a ClassifiedError", err)
			}
			if ce.Class != tc.want {
				t.Errorf("class = %q, want %q", ce.Class, tc.want)
			}
			// The provider error stays reachable in the chain.
			var pe *llm.Error
			if !errors.As(err, &pe) {
				t.Error("wrapped *llm.Error no longer reachable via errors.As")
			}
		})
	}
}

func TestLLMExecutorContextErrorPassthrough(t *testing.T) {
	t.Parallel()
	// A context error must pass through unclassified so the engine judges
	// timeout vs. cancelled from context state (ADR-006 rows 3/8).
	e := recExecutor(t, &recordingProvider{err: context.Canceled})
	_, err := e.Execute(context.Background(), llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "x"}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		t.Errorf("context error was wrapped in a ClassifiedError: %v", err)
	}
}

func TestLLMExecutorRoutingFailuresPermanent(t *testing.T) {
	t.Parallel()
	// Only "rec" is registered, so both a model with no vendor rule
	// (UnknownModelError) and one routing to an unregistered provider
	// (ProviderUnavailableError) fail — deterministically, hence permanent.
	cases := map[string]string{
		"unknown model":        "no-such-vendor-xyz", // no prefix rule → UnknownModelError
		"unavailable provider": "gpt-4o",             // routes to openai, not registered → ProviderUnavailable
	}
	for name, model := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &recordingProvider{resp: okResponse()}
			e := recExecutor(t, p)
			_, err := e.Execute(context.Background(), llmStep(t, map[string]any{"model": model, "prompt": "x"}))
			if !isPermanent(err) {
				t.Errorf("error %v is not classified permanent", err)
			}
			// A routing failure must never reach the provider.
			if p.gotReq.Model != "" {
				t.Errorf("provider was called (model %q) despite a routing failure", p.gotReq.Model)
			}
		})
	}
}

func TestLLMExecutorNilRegistryPermanent(t *testing.T) {
	t.Parallel()
	e := NewLLMExecutor(nil)
	_, err := e.Execute(context.Background(), llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "x"}))
	if !isPermanent(err) {
		t.Errorf("nil-registry error %v is not classified permanent", err)
	}
}

func TestLLMExecutorMissingModelInvalidConfig(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})
	_, err := e.Execute(context.Background(), llmStep(t, map[string]any{"prompt": "x"}))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("error = %v, want ErrInvalidConfig", err)
	}
}

// isPermanent reports whether err carries a permanent ClassifiedError.
func isPermanent(err error) bool {
	var ce *ClassifiedError
	return errors.As(err, &ce) && ce.Class == dag.ClassPermanent
}
