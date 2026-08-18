package llm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// TestParseMockScript round-trips a script through ParseMockScript → NewMock and
// drives the resulting provider, proving a scripted offline mock (ticket 14.5)
// matches rules and returns scripted structured/text/error outcomes.
func TestParseMockScript(t *testing.T) {
	t.Parallel()

	script := []byte(`{
		"seed": 7,
		"rules": [
			{"substring": "Review the latest draft", "respond": [
				{"structured": {"verdict": "revise", "notes": ["add detail"]}},
				{"structured": {"verdict": "approve"}}
			]},
			{"substring": "output-quality judge", "respond": [
				{"text": "{\"score\": 0.4, \"rationale\": \"terse\"}"},
				{"text": "{\"score\": 0.9, \"rationale\": \"good\"}"}
			]},
			{"substring": "boom", "respond": [
				{"status": 500, "code": "server_error", "message": "kaboom"}
			]}
		],
		"default": {"text": "echoed"}
	}`)

	cfg, err := llm.ParseMockScript(script)
	if err != nil {
		t.Fatalf("ParseMockScript: %v", err)
	}
	if cfg.Seed != 7 {
		t.Errorf("seed = %d, want 7", cfg.Seed)
	}
	if len(cfg.Rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(cfg.Rules))
	}

	mock, err := llm.NewMock(cfg)
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	ctx := context.Background()

	// The critic rule returns a scripted structured verdict, sticky-last.
	ask := func(prompt string) llm.ChatResponse {
		resp, err := mock.Chat(ctx, llm.ChatRequest{
			Model:     "mock/sim-1",
			MaxTokens: 64,
			Messages:  []llm.Message{llm.UserText(prompt)},
		})
		if err != nil {
			t.Fatalf("Chat(%q): %v", prompt, err)
		}
		return resp
	}
	first := ask("Review the latest draft against the brief")
	if got := string(first.Structured); !strings.Contains(got, `"revise"`) {
		t.Errorf("first critic response = %q, want a revise verdict", got)
	}
	second := ask("Review the latest draft against the brief")
	if got := string(second.Structured); !strings.Contains(got, `"approve"`) {
		t.Errorf("second critic response = %q, want an approve verdict", got)
	}

	// The judge rule returns scripted text.
	judge := ask("You are a strict output-quality judge. Grade this.")
	var text string
	for _, b := range judge.Blocks {
		if b.Type == llm.BlockText {
			text += b.Text
		}
	}
	if !strings.Contains(text, "0.4") {
		t.Errorf("first judge response = %q, want score 0.4", text)
	}

	// A scripted error outcome is classified.
	if _, err := mock.Chat(ctx, llm.ChatRequest{
		Model:     "mock/sim-1",
		MaxTokens: 64,
		Messages:  []llm.Message{llm.UserText("boom")},
	}); err == nil {
		t.Error("scripted 500 outcome: want an error, got nil")
	}

	// The default is used when no rule matches.
	def := ask("something unrelated")
	var deftext string
	for _, b := range def.Blocks {
		if b.Type == llm.BlockText {
			deftext += b.Text
		}
	}
	if deftext != "echoed" {
		t.Errorf("default response = %q, want %q", deftext, "echoed")
	}
}

// TestParseMockScriptRejectsUnknownField proves strict decode: a typo in a
// field name fails at parse (boot), not silently.
func TestParseMockScriptRejectsUnknownField(t *testing.T) {
	t.Parallel()

	if _, err := llm.ParseMockScript([]byte(`{"rules": [{"substr": "x", "respond": [{"text": "y"}]}]}`)); err == nil {
		t.Error("unknown field 'substr': want a parse error, got nil")
	}
	if _, err := llm.ParseMockScript([]byte(`not json`)); err == nil {
		t.Error("malformed JSON: want a parse error, got nil")
	}
	// A structurally parseable but semantically invalid rule (no responses) is
	// caught by NewMock, not ParseMockScript.
	cfg, err := llm.ParseMockScript([]byte(`{"rules": [{"substring": "x", "respond": []}]}`))
	if err != nil {
		t.Fatalf("ParseMockScript should not validate rule semantics: %v", err)
	}
	if _, err := llm.NewMock(cfg); err == nil {
		t.Error("rule with no responses: want NewMock to reject it, got nil")
	}
}
