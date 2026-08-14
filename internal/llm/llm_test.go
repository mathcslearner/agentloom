package llm_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// TestChatRequestValidate pins the client-side shape rules shared by
// every provider: each rejection names the offending path, and the
// valid full-shape request passes.
func TestChatRequestValidate(t *testing.T) {
	t.Parallel()

	if err := fullRequest().Validate(); err != nil {
		t.Errorf("full request: unexpected error: %v", err)
	}
	if err := basicRequest().Validate(); err != nil {
		t.Errorf("basic request: unexpected error: %v", err)
	}

	mutate := func(fn func(*llm.ChatRequest)) llm.ChatRequest {
		r := fullRequest()
		fn(&r)
		return r
	}
	cases := []struct {
		name        string
		req         llm.ChatRequest
		wantMention string
	}{
		{"missing model", mutate(func(r *llm.ChatRequest) { r.Model = "" }), "model"},
		{"zero max_tokens", mutate(func(r *llm.ChatRequest) { r.MaxTokens = 0 }), "max_tokens"},
		{"negative max_tokens", mutate(func(r *llm.ChatRequest) { r.MaxTokens = -5 }), "max_tokens"},
		{"no messages", mutate(func(r *llm.ChatRequest) { r.Messages = nil }), "messages"},
		{
			"bad role",
			mutate(func(r *llm.ChatRequest) { r.Messages[0].Role = "system" }),
			"role",
		},
		{
			"empty message",
			mutate(func(r *llm.ChatRequest) { r.Messages[0].Blocks = nil }),
			"blocks",
		},
		{
			"empty text block",
			mutate(func(r *llm.ChatRequest) { r.Messages[0].Blocks[0].Text = "" }),
			"text block",
		},
		{
			"unknown block type",
			mutate(func(r *llm.ChatRequest) { r.Messages[0].Blocks[0].Type = "thinking" }),
			"block type",
		},
		{
			"text block with tool fields",
			mutate(func(r *llm.ChatRequest) { r.Messages[0].Blocks[0].ToolUse = &llm.ToolUse{ID: "x", Name: "y"} }),
			"non-text",
		},
		{
			"tool_use without id",
			mutate(func(r *llm.ChatRequest) { r.Messages[1].Blocks[1].ToolUse.ID = "" }),
			"id and name",
		},
		{
			"tool_use with invalid input JSON",
			mutate(func(r *llm.ChatRequest) { r.Messages[1].Blocks[1].ToolUse.Input = json.RawMessage("{oops") }),
			"valid JSON",
		},
		{
			"tool_result without tool_use_id",
			mutate(func(r *llm.ChatRequest) { r.Messages[2].Blocks[0].ToolResult.ToolUseID = "" }),
			"tool_use_id",
		},
		{
			"tool without name",
			mutate(func(r *llm.ChatRequest) { r.Tools[0].Name = "" }),
			"name",
		},
		{
			"tool without input schema",
			mutate(func(r *llm.ChatRequest) { r.Tools[0].InputSchema = nil }),
			"schema",
		},
		{
			"tool with non-object input schema",
			mutate(func(r *llm.ChatRequest) { r.Tools[0].InputSchema = json.RawMessage(`"not an object"`) }),
			"schema",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.req.Validate()
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMention) {
				t.Errorf("error %q does not mention %q", err, tc.wantMention)
			}
		})
	}
}

// TestChatResponseAccessors pins Text (text blocks only, concatenated
// in order) and ToolUses (calls only, in order).
func TestChatResponseAccessors(t *testing.T) {
	t.Parallel()

	resp := llm.ChatResponse{Blocks: []llm.Block{
		llm.TextBlock("one "),
		{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "a", Name: "first"}},
		llm.TextBlock("two"),
		{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "b", Name: "second"}},
	}}
	if got := resp.Text(); got != "one two" {
		t.Errorf("Text() = %q, want %q", got, "one two")
	}
	uses := resp.ToolUses()
	if len(uses) != 2 || uses[0].Name != "first" || uses[1].Name != "second" {
		t.Errorf("ToolUses() = %+v", uses)
	}

	empty := llm.ChatResponse{}
	if empty.Text() != "" || empty.ToolUses() != nil {
		t.Errorf("empty response accessors = %q / %+v, want zero values", empty.Text(), empty.ToolUses())
	}
}
