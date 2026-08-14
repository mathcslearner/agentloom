package llm_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// TestAnthropicLiveSmoke is the ticket's optional live smoke test: one
// tiny real Messages API call, run only when explicitly asked for with
// LIVE_LLM_TESTS=1 and a key in the environment. It is not part of any
// CI job — recorded fixtures are the contract; this exists to catch
// wire-format drift by hand:
//
//	LIVE_LLM_TESTS=1 AGENTLOOM_ANTHROPIC_API_KEY=... go test ./internal/llm -run LiveSmoke -v
func TestAnthropicLiveSmoke(t *testing.T) {
	if os.Getenv("LIVE_LLM_TESTS") != "1" {
		t.Skip("live provider tests disabled; set LIVE_LLM_TESTS=1 to enable")
	}
	key := os.Getenv("AGENTLOOM_ANTHROPIC_API_KEY")
	if key == "" {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}
	if key == "" {
		t.Skip("LIVE_LLM_TESTS=1 but no AGENTLOOM_ANTHROPIC_API_KEY / ANTHROPIC_API_KEY in the environment")
	}

	provider, err := llm.NewAnthropic(llm.AnthropicConfig{APIKey: key})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 32,
		Messages:  []llm.Message{llm.UserText("Reply with exactly one word: pong")},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text() == "" {
		t.Error("live completion returned no text")
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Errorf("usage = %+v, want positive input and output tokens", resp.Usage)
	}
	t.Logf("live smoke: model %s, stop %s, usage %+v, text %q",
		resp.Model, resp.StopReason, resp.Usage, resp.Text())
}

// TestOpenAILiveSmoke is the OpenAI counterpart: one tiny real Chat
// Completions call, gated identically and in no CI job.
//
//	LIVE_LLM_TESTS=1 AGENTLOOM_OPENAI_API_KEY=... go test ./internal/llm -run OpenAILiveSmoke -v
func TestOpenAILiveSmoke(t *testing.T) {
	if os.Getenv("LIVE_LLM_TESTS") != "1" {
		t.Skip("live provider tests disabled; set LIVE_LLM_TESTS=1 to enable")
	}
	key := os.Getenv("AGENTLOOM_OPENAI_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if key == "" {
		t.Skip("LIVE_LLM_TESTS=1 but no AGENTLOOM_OPENAI_API_KEY / OPENAI_API_KEY in the environment")
	}

	provider, err := llm.NewOpenAI(llm.OpenAIConfig{APIKey: key})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Model:     "gpt-4o-mini",
		MaxTokens: 32,
		Messages:  []llm.Message{llm.UserText("Reply with exactly one word: pong")},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text() == "" {
		t.Error("live completion returned no text")
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Errorf("usage = %+v, want positive input and output tokens", resp.Usage)
	}
	t.Logf("live smoke: model %s, stop %s, usage %+v, text %q",
		resp.Model, resp.StopReason, resp.Usage, resp.Text())
}
