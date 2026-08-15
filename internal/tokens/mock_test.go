package tokens

import (
	"context"
	"testing"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// TestMockCounterMatchesProviderUsage is the property that lets M12's
// mock-driven exit fixture run offline in CI with real accuracy assertions:
// the mock counter's CountRequest equals the InputTokens the mock provider
// reports for the same request, exactly. If either side drifts, this fails.
func TestMockCounterMatchesProviderUsage(t *testing.T) {
	mock, err := llm.NewMock(llm.MockConfig{})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	counter := newMockCounter()

	reqs := []llm.ChatRequest{
		{Model: "sim-1", Messages: []llm.Message{llm.UserText("Hello there, mock.")}, MaxTokens: 64},
		{
			Model:     "sim-1",
			System:    "You are a deterministic mock used in tests.",
			Messages:  []llm.Message{llm.UserText("Summarize the plan."), llm.AssistantText("Here is a summary."), llm.UserText("Now go deeper.")},
			MaxTokens: 128,
		},
		{
			Model: "sim-1",
			Messages: []llm.Message{
				{Role: llm.RoleUser, Blocks: []llm.Block{
					llm.TextBlock("Consider this result:"),
					{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "t1", Content: "42 rows affected"}},
				}},
			},
			MaxTokens: 64,
		},
	}
	for i, req := range reqs {
		resp, err := mock.Chat(context.Background(), req)
		if err != nil {
			t.Fatalf("req %d: mock.Chat: %v", i, err)
		}
		want := int(resp.Usage.InputTokens)
		if got := counter.CountRequest(req); got != want {
			t.Errorf("req %d: mockCounter.CountRequest = %d, mock provider InputTokens = %d", i, got, want)
		}
	}
}
