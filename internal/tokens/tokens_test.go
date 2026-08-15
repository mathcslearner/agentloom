package tokens

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// TestOpenAICountBareText pins the counter against tiktoken directly — OpenAI's
// real tokenizer — for a range of content types. Since the counter *is* that
// tokenizer, this is exact ground truth, not an approximation.
func TestOpenAICountBareText(t *testing.T) {
	c, err := newOpenAICounter("gpt-4o")
	if err != nil {
		t.Fatalf("newOpenAICounter: %v", err)
	}
	enc, err := getEncoding(encO200kBase)
	if err != nil {
		t.Fatalf("getEncoding: %v", err)
	}
	for _, s := range []string{
		"", "Hello", "Hello, world!", "The capital of France is Paris.",
		`{"name":"Ada","age":37,"tags":["math","code"]}`, "日本語のトークンは長い",
	} {
		want := len(enc.EncodeOrdinary(s))
		if got := c.Count(s); got != want {
			t.Errorf("Count(%q) = %d, want %d", s, got, want)
		}
	}
}

// TestOpenAICountRequestExact hand-verifies CountRequest against
// independently-computed totals for small requests: reply priming (3) +
// per-message framing (3) + tiktoken content counts. This validates the
// framing math without going through the same code path as the counter.
func TestOpenAICountRequestExact(t *testing.T) {
	c, err := newOpenAICounter("gpt-4o")
	if err != nil {
		t.Fatalf("newOpenAICounter: %v", err)
	}
	cases := []struct {
		name string
		req  llm.ChatRequest
		want int // hand-computed: 3 reply + Σ(3 perMessage + content)
	}{
		{
			name: "single_user_hello",
			req:  llm.ChatRequest{Model: "gpt-4o", Messages: []llm.Message{llm.UserText("Hello")}},
			want: 3 + (3 + 1),
		},
		{
			name: "system_plus_user",
			req: llm.ChatRequest{
				Model:    "gpt-4o",
				System:   "You are a helpful assistant.",
				Messages: []llm.Message{llm.UserText("The capital of France is Paris.")},
			},
			want: 3 + (3 + 6) + (3 + 7),
		},
		{
			name: "three_turns",
			req: llm.ChatRequest{
				Model: "gpt-4o",
				Messages: []llm.Message{
					llm.UserText("Hello, world!"),
					llm.AssistantText("The capital of France is Paris."),
					llm.UserText("You are a helpful assistant."),
				},
			},
			want: 3 + (3 + 4) + (3 + 7) + (3 + 6),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.CountRequest(tc.req); got != tc.want {
				t.Errorf("CountRequest = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCountRequestFramesToolsAndSchema checks the non-text components of a
// request (tool defs, tool_use/tool_result blocks, response format) each add
// their content plus the documented per-tool constant, cross-checked against an
// independent framing walk.
func TestCountRequestFramesToolsAndSchema(t *testing.T) {
	c, err := newOpenAICounter("gpt-4o")
	if err != nil {
		t.Fatalf("newOpenAICounter: %v", err)
	}
	req := llm.ChatRequest{
		Model:  "gpt-4o",
		System: "You are a tool-using assistant.",
		Messages: []llm.Message{
			llm.UserText("What is the weather in Paris?"),
			{Role: llm.RoleAssistant, Blocks: []llm.Block{{
				Type:    llm.BlockToolUse,
				ToolUse: &llm.ToolUse{ID: "t1", Name: "get_weather", Input: json.RawMessage(`{"city":"Paris"}`)},
			}}},
			{Role: llm.RoleUser, Blocks: []llm.Block{{
				Type:       llm.BlockToolResult,
				ToolResult: &llm.ToolResult{ToolUseID: "t1", Content: "18°C and sunny"},
			}}},
		},
		Tools: []llm.ToolDef{{
			Name:        "get_weather",
			Description: "Get the current weather for a city.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ResponseFormat: &llm.ResponseFormat{Name: "answer", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	want := refCount(t, c.Count, req)
	if got := c.CountRequest(req); got != want {
		t.Errorf("CountRequest = %d, want (independent walk) %d", got, want)
	}
}

// refCount is an independent reimplementation of the framing walk used to
// cross-check countRequest: it must agree component-for-component.
func refCount(t *testing.T, count func(string) int, req llm.ChatRequest) int {
	t.Helper()
	total := openAIFraming.reply
	if req.System != "" {
		total += openAIFraming.perMessage + count(req.System)
	}
	for _, m := range req.Messages {
		total += openAIFraming.perMessage
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockText:
				total += count(b.Text)
			case llm.BlockToolUse:
				total += count(b.ToolUse.Name) + count(string(b.ToolUse.Input))
			case llm.BlockToolResult:
				total += count(b.ToolResult.Content)
			}
		}
	}
	for _, tl := range req.Tools {
		total += openAIFraming.perTool + count(tl.Name) + count(tl.Description) + count(string(tl.InputSchema))
	}
	if req.ResponseFormat != nil {
		total += count(string(req.ResponseFormat.Schema))
		if req.ResponseFormat.Name != "" {
			total += count(req.ResponseFormat.Name)
		}
	}
	return total
}

// TestAnthropicEstimateScalesBase checks the Anthropic counter is the o200k
// base count scaled by the calibration factor and rounded.
func TestAnthropicEstimateScalesBase(t *testing.T) {
	c, err := newAnthropicCounter()
	if err != nil {
		t.Fatalf("newAnthropicCounter: %v", err)
	}
	enc, _ := getEncoding(encO200kBase)
	s := "The quick brown fox jumps over the lazy dog, repeatedly and with great enthusiasm."
	base := len(enc.EncodeOrdinary(s))
	want := int(float64(base)*AnthropicCalibrationFactor + 0.5)
	if got := c.Count(s); got != want {
		t.Errorf("Count = %d, want round(%d*%g) = %d", got, base, AnthropicCalibrationFactor, want)
	}
	if c.Count("") != 0 {
		t.Errorf("Count(\"\") = %d, want 0", c.Count(""))
	}
}

// TestFallbackCounter checks the chars/4 fallback (ceil, never below 1 for
// non-empty).
func TestFallbackCounter(t *testing.T) {
	c := newFallbackCounter()
	cases := map[string]int{"": 0, "a": 1, "abcd": 1, "abcde": 2, "12345678": 2}
	for s, want := range cases {
		if got := c.Count(s); got != want {
			t.Errorf("Count(%q) = %d, want %d", s, got, want)
		}
	}
}

// TestDeterminism checks the same request counts identically across repeated
// and concurrent calls (a stored count must be reproducible).
func TestDeterminism(t *testing.T) {
	c, err := newOpenAICounter("gpt-4o")
	if err != nil {
		t.Fatalf("newOpenAICounter: %v", err)
	}
	req := llm.ChatRequest{
		Model:    "gpt-4o",
		System:   "You are a helpful assistant.",
		Messages: []llm.Message{llm.UserText("Count these tokens deterministically, please.")},
	}
	base := c.CountRequest(req)
	var wg sync.WaitGroup
	errs := make(chan int, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := c.CountRequest(req); got != base {
				errs <- got
			}
		}()
	}
	wg.Wait()
	close(errs)
	for got := range errs {
		t.Fatalf("concurrent CountRequest = %d, want %d (non-deterministic)", got, base)
	}
}

// TestIDsAreStable pins the counter fingerprints; a change here is intentional
// and invalidates stored counts (bump the version constant deliberately).
func TestIDsAreStable(t *testing.T) {
	oa, _ := newOpenAICounter("gpt-4o")
	if got := oa.ID(); got != "openai/o200k_base@1" {
		t.Errorf("openai gpt-4o ID = %q", got)
	}
	legacy, _ := newOpenAICounter("gpt-4")
	if got := legacy.ID(); got != "openai/cl100k_base@1" {
		t.Errorf("openai gpt-4 ID = %q", got)
	}
	an, _ := newAnthropicCounter()
	if got := an.ID(); got != "anthropic/estimate@1;factor=1.15" {
		t.Errorf("anthropic ID = %q", got)
	}
	if got := newMockCounter().ID(); got != "mock/estimate@1" {
		t.Errorf("mock ID = %q", got)
	}
	if got := newFallbackCounter().ID(); got != "fallback/chars4@1" {
		t.Errorf("fallback ID = %q", got)
	}
}

func BenchmarkCountRequest(b *testing.B) {
	c, err := newOpenAICounter("gpt-4o")
	if err != nil {
		b.Fatalf("newOpenAICounter: %v", err)
	}
	req := llm.ChatRequest{
		Model:  "gpt-4o",
		System: "You are a helpful assistant that answers concisely.",
		Messages: []llm.Message{
			llm.UserText("Summarize the history of distributed systems in three sentences."),
			llm.AssistantText("Distributed systems emerged to scale computation beyond one machine..."),
			llm.UserText("Now explain the CAP theorem."),
		},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = c.CountRequest(req)
	}
}
