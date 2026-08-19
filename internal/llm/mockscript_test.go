package llm_test

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestParseMockScriptDistributions parses the load-oriented latency/token/
// injection wire fields (ticket 19.1) into a valid MockConfig and drives the
// provider, proving the global token distribution and per-outcome overrides
// reach the reported usage.
func TestParseMockScriptDistributions(t *testing.T) {
	t.Parallel()

	script := []byte(`{
		"seed": 11,
		"latency": {"p50": "120ms", "p99": "800ms"},
		"tokens": {"input": {"min": 400, "max": 500}, "output": {"fixed": 128}},
		"inject": {"rate_429": 0.0, "retry_after": "250ms"},
		"rules": [
			{"substring": "cheap", "respond": [
				{"text": "ok", "latency": {"fixed": "5ms"}, "tokens": {"input": {"fixed": 10}, "output": {"fixed": 20}}}
			]},
			{"substring": "pinned", "respond": [
				{"text": "ok", "usage": {"input": 3, "output": 4}}
			]}
		],
		"default": {"text": "d"}
	}`)

	cfg, err := llm.ParseMockScript(script)
	if err != nil {
		t.Fatalf("ParseMockScript: %v", err)
	}
	if cfg.Latency.P50 != 120*time.Millisecond || cfg.Latency.P99 != 800*time.Millisecond {
		t.Errorf("global latency = %+v, want p50 120ms / p99 800ms", cfg.Latency)
	}
	if cfg.Tokens == nil || cfg.Tokens.Input.Min != 400 || cfg.Tokens.Output.Fixed != 128 {
		t.Errorf("global tokens = %+v, want input[400,500] output fixed 128", cfg.Tokens)
	}
	if cfg.Inject.RetryAfter != 250*time.Millisecond {
		t.Errorf("inject retry_after = %s, want 250ms", cfg.Inject.RetryAfter)
	}

	// Use an instant sleep so the p50 latency doesn't slow the test.
	sl := &instantSleep{}
	cfg.Sleep = sl.sleep
	mock, err := llm.NewMock(cfg)
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	ctx := context.Background()
	ask := func(prompt string) llm.ChatResponse {
		resp, err := mock.Chat(ctx, llm.ChatRequest{Model: "mock/sim-1", MaxTokens: 64, Messages: []llm.Message{llm.UserText(prompt)}})
		if err != nil {
			t.Fatalf("Chat(%q): %v", prompt, err)
		}
		return resp
	}
	cheap := ask("cheap request")
	if cheap.Usage.InputTokens != 10 || cheap.Usage.OutputTokens != 20 {
		t.Errorf("cheap usage = %+v, want {10,20} from the per-rule token override", cheap.Usage)
	}
	pinned := ask("pinned request")
	if pinned.Usage.InputTokens != 3 || pinned.Usage.OutputTokens != 4 {
		t.Errorf("pinned usage = %+v, want {3,4} from the explicit usage override", pinned.Usage)
	}
	def := ask("unrelated")
	if def.Usage.InputTokens < 400 || def.Usage.InputTokens >= 500 || def.Usage.OutputTokens != 128 {
		t.Errorf("default usage = %+v, want global distribution input[400,500) output 128", def.Usage)
	}
}

// TestParseMockScriptRejectsBadDuration: a malformed duration string fails at
// parse.
func TestParseMockScriptRejectsBadDuration(t *testing.T) {
	t.Parallel()
	if _, err := llm.ParseMockScript([]byte(`{"latency": {"p50": "notaduration"}}`)); err == nil {
		t.Error("bad p50 duration: want a parse error, got nil")
	}
	if _, err := llm.ParseMockScript([]byte(`{"inject": {"retry_after": "10furlongs"}}`)); err == nil {
		t.Error("bad retry_after duration: want a parse error, got nil")
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
