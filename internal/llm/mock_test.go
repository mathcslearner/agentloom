package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// mockReq builds a single-user-text request the mock's default echo and
// substring rules match against.
func mockReq(text string) llm.ChatRequest {
	return llm.ChatRequest{Model: "sim-1", MaxTokens: 64, Messages: []llm.Message{llm.UserText(text)}}
}

// instantSleep is an injected latency implementation that records every
// requested wait without actually sleeping, so latency draws are
// observable and time stays controlled.
type instantSleep struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *instantSleep) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return nil
}

func (s *instantSleep) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

// TestMockDefaultEcho: with no rules the mock deterministically echoes the
// last user text and returns non-zero usage on every success.
func TestMockDefaultEcho(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	resp, err := m.Chat(context.Background(), mockReq("hello world"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(resp.Text(), "hello world") {
		t.Errorf("echo = %q, want it to contain the prompt", resp.Text())
	}
	if resp.Model != "sim-1" {
		t.Errorf("model = %q, want the request's model echoed back", resp.Model)
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Errorf("usage = %+v, want positive token counts on success", resp.Usage)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop reason = %q, want end_turn", resp.StopReason)
	}
}

// TestMockStructuredEcho: under a ResponseFormat request the default echo
// answers with native structured JSON (ticket 11.3), not "[mock] ..." text.
func TestMockStructuredEcho(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	req := mockReq("hello world")
	req.ResponseFormat = &llm.ResponseFormat{}
	resp, err := m.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Structured) == 0 {
		t.Fatal("Structured is empty; expected a native structured echo under a ResponseFormat request")
	}
	var payload map[string]string
	if err := json.Unmarshal(resp.Structured, &payload); err != nil {
		t.Fatalf("Structured is not valid JSON: %v (%s)", err, resp.Structured)
	}
	if payload["echo"] != "hello world" {
		t.Errorf("structured echo = %+v, want echo of the prompt", payload)
	}
	if resp.Text() != "" {
		t.Errorf("Text() = %q, want empty for a native structured response", resp.Text())
	}
}

// TestMockScriptedStructured: an explicit Structured outcome returns native
// structured JSON.
func TestMockScriptedStructured(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{
		Default: &llm.MockOutcome{Structured: json.RawMessage(`{"title":"scripted"}`)},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	resp, err := m.Chat(context.Background(), mockReq("anything"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if string(resp.Structured) != `{"title":"scripted"}` {
		t.Errorf("Structured = %q, want the scripted JSON", resp.Structured)
	}
}

// TestMockFixedResponse: a scripted fixed response with explicit usage.
func TestMockFixedResponse(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{
		Default: &llm.MockOutcome{Text: "canned", Usage: &llm.Usage{InputTokens: 7, OutputTokens: 3}},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	resp, err := m.Chat(context.Background(), mockReq("anything"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text() != "canned" {
		t.Errorf("text = %q, want canned", resp.Text())
	}
	if resp.Usage != (llm.Usage{InputTokens: 7, OutputTokens: 3}) {
		t.Errorf("usage = %+v, want the explicit override", resp.Usage)
	}
}

// TestMockSequenceStickyLast: a per-rule sequence returns its entries in
// order, then repeats the last one forever.
func TestMockSequenceStickyLast(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{
		Rules: []llm.MockRule{{
			Substring: "go",
			Respond:   []llm.MockOutcome{{Text: "first"}, {Text: "second"}},
		}},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	want := []string{"first", "second", "second", "second"}
	for i, w := range want {
		resp, err := m.Chat(context.Background(), mockReq("go"))
		if err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
		if resp.Text() != w {
			t.Errorf("call %d text = %q, want %q", i, resp.Text(), w)
		}
	}
}

// TestMockConditionalOnPrompt: substring and regex rules select responses
// by prompt content; first matching rule wins.
func TestMockConditionalOnPrompt(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{
		Rules: []llm.MockRule{
			{Substring: "weather", Respond: []llm.MockOutcome{{Text: "sunny"}}},
			{Pattern: `\bprice\b`, Respond: []llm.MockOutcome{{Text: "$42"}}},
		},
		Default: &llm.MockOutcome{Text: "fallback"},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	cases := map[string]string{
		"what is the weather": "sunny",
		"the price of tea":    "$42",
		"pricey":              "fallback", // \bprice\b does not match "pricey"
		"unrelated":           "fallback",
	}
	for prompt, want := range cases {
		resp, err := m.Chat(context.Background(), mockReq(prompt))
		if err != nil {
			t.Fatalf("Chat %q: %v", prompt, err)
		}
		if resp.Text() != want {
			t.Errorf("prompt %q → %q, want %q", prompt, resp.Text(), want)
		}
	}
}

// TestMockOnCallOrdinal: an OnCall rule fires on exactly the Nth global
// call regardless of prompt.
func TestMockOnCallOrdinal(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{
		Rules:   []llm.MockRule{{OnCall: 2, Respond: []llm.MockOutcome{{Text: "the-second-call"}}}},
		Default: &llm.MockOutcome{Text: "other"},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	want := []string{"other", "the-second-call", "other"}
	for i, w := range want {
		resp, err := m.Chat(context.Background(), mockReq("x"))
		if err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
		if resp.Text() != w {
			t.Errorf("call %d = %q, want %q", i, resp.Text(), w)
		}
	}
}

// TestMockScriptedError: a scripted non-zero status surfaces as a
// classified *Error carrying status, code, and RetryAfter.
func TestMockScriptedError(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{
		Rules: []llm.MockRule{
			{Substring: "boom", Respond: []llm.MockOutcome{{Status: 429, RetryAfter: 3 * time.Second}}},
			{Substring: "bad", Respond: []llm.MockOutcome{{Status: 400, Code: "invalid_request_error", Message: "nope"}}},
		},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}

	_, err = m.Chat(context.Background(), mockReq("boom"))
	var perr *llm.Error
	if !errors.As(err, &perr) {
		t.Fatalf("boom error = %v (%T), want *llm.Error", err, err)
	}
	if perr.Class != dag.ClassTransient || perr.Status != 429 || perr.RetryAfter != 3*time.Second {
		t.Errorf("boom = %+v, want transient 429 retry-after 3s", perr)
	}
	if perr.Provider != llm.ProviderMock {
		t.Errorf("provider = %q, want mock", perr.Provider)
	}

	_, err = m.Chat(context.Background(), mockReq("bad"))
	if !errors.As(err, &perr) {
		t.Fatalf("bad error = %v (%T), want *llm.Error", err, err)
	}
	if perr.Class != dag.ClassPermanent || perr.Status != 400 || perr.Code != "invalid_request_error" {
		t.Errorf("bad = %+v, want permanent 400 invalid_request_error", perr)
	}
}

// TestMockLatencyDraws: fixed and per-outcome latency overrides are
// observed through the injected sleep.
func TestMockLatencyDraws(t *testing.T) {
	t.Parallel()
	sl := &instantSleep{}
	m, err := llm.NewMock(llm.MockConfig{
		Latency: llm.LatencySpec{Fixed: 100 * time.Millisecond},
		Rules: []llm.MockRule{{
			Substring: "slow",
			Respond:   []llm.MockOutcome{{Text: "ok", Latency: &llm.LatencySpec{Fixed: 5 * time.Second}}},
		}},
		Sleep: sl.sleep,
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	if _, err := m.Chat(context.Background(), mockReq("normal")); err != nil {
		t.Fatalf("Chat normal: %v", err)
	}
	if _, err := m.Chat(context.Background(), mockReq("slow one")); err != nil {
		t.Fatalf("Chat slow: %v", err)
	}
	got := sl.recorded()
	want := []time.Duration{100 * time.Millisecond, 5 * time.Second}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("recorded waits = %v, want %v", got, want)
	}
}

// TestMockHangUntilCancelled: a Hang outcome blocks until ctx is
// cancelled and returns an unclassified wrapped context error — the
// engine keeps the timeout/cancel judgment (ADR-006).
func TestMockHangUntilCancelled(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{Default: &llm.MockOutcome{Hang: true}})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = m.Chat(ctx, mockReq("x"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want a context.DeadlineExceeded chain", err)
	}
	var perr *llm.Error
	if errors.As(err, &perr) {
		t.Errorf("hang classified as *llm.Error (%+v); the engine owns that judgment", perr)
	}
}

// TestMockInjectionDeterministicAndClassed: fractional injection rates
// produce a reproducible pattern of 429s under a seed, all classified
// transient.
func TestMockInjectionDeterministicAndClassed(t *testing.T) {
	t.Parallel()
	build := func() *llm.Mock {
		m, err := llm.NewMock(llm.MockConfig{
			Seed:   99,
			Inject: llm.MockInjection{Rate429: 0.5, RetryAfter: time.Second},
		})
		if err != nil {
			t.Fatalf("NewMock: %v", err)
		}
		return m
	}
	pattern := func(m *llm.Mock) string {
		var b strings.Builder
		for i := 0; i < 40; i++ {
			_, err := m.Chat(context.Background(), mockReq("x"))
			var perr *llm.Error
			switch {
			case err == nil:
				b.WriteByte('.')
			case errors.As(err, &perr) && perr.Status == 429 && perr.Class == dag.ClassTransient:
				b.WriteByte('4')
			default:
				t.Fatalf("call %d unexpected error %v", i, err)
			}
		}
		return b.String()
	}
	a, b := pattern(build()), pattern(build())
	if a != b {
		t.Errorf("injection pattern not reproducible under seed:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, "4") || !strings.Contains(a, ".") {
		t.Errorf("pattern %q lacks both injected and clean calls — rate not honored", a)
	}
}

// TestMockDeterministicTranscript: same seed + same call sequence →
// identical transcript (responses, errors, latency draws); a different
// seed diverges (positive control).
func TestMockDeterministicTranscript(t *testing.T) {
	t.Parallel()
	cfg := func(seed int64, sl *instantSleep) llm.MockConfig {
		return llm.MockConfig{
			Seed:    seed,
			Inject:  llm.MockInjection{Rate500: 0.3},
			Latency: llm.LatencySpec{Min: 10 * time.Millisecond, Max: 200 * time.Millisecond},
			Sleep:   sl.sleep,
		}
	}
	transcript := func(seed int64) string {
		sl := &instantSleep{}
		m, err := llm.NewMock(cfg(seed, sl))
		if err != nil {
			t.Fatalf("NewMock: %v", err)
		}
		var b strings.Builder
		for i := 0; i < 30; i++ {
			resp, err := m.Chat(context.Background(), mockReq(fmt.Sprintf("prompt-%d", i)))
			if err != nil {
				var perr *llm.Error
				errors.As(err, &perr)
				fmt.Fprintf(&b, "E%d;", perr.Status)
				continue
			}
			fmt.Fprintf(&b, "%s/%d;", resp.Text(), resp.Usage.OutputTokens)
		}
		fmt.Fprintf(&b, "waits=%v", sl.recorded())
		return b.String()
	}
	if a, b := transcript(7), transcript(7); a != b {
		t.Errorf("transcript not deterministic under identical seed:\n%s\n%s", a, b)
	}
	if a, b := transcript(7), transcript(8); a == b {
		t.Error("distinct seeds produced identical transcripts — seed not driving the draws")
	}
}

// tripwireTransport fails the test if the mock ever performs an HTTP
// call. Installed as http.DefaultTransport for the duration of the run.
type tripwireTransport struct {
	t      *testing.T
	tapped atomic.Bool
}

func (tr *tripwireTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.tapped.Store(true)
	tr.t.Errorf("mock provider made an HTTP call to %s — it must be fully offline", r.URL)
	return nil, errors.New("tripwire: no network allowed")
}

// TestMockZeroExternalCalls asserts the offline guarantee structurally
// and dynamically: the whole scripting/injection/latency matrix runs with
// http.DefaultTransport swapped for a tripwire that fails on any use.
func TestMockZeroExternalCalls(t *testing.T) {
	// Not parallel: it mutates the process-global http.DefaultTransport.
	tr := &tripwireTransport{t: t}
	saved := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = saved })

	sl := &instantSleep{}
	m, err := llm.NewMock(llm.MockConfig{
		Seed:    3,
		Rules:   []llm.MockRule{{Substring: "err", Respond: []llm.MockOutcome{{Status: 500}}}},
		Inject:  llm.MockInjection{Rate429: 0.2},
		Latency: llm.LatencySpec{Min: time.Millisecond, Max: 10 * time.Millisecond},
		Sleep:   sl.sleep,
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	for i := 0; i < 50; i++ {
		prompt := "ok"
		if i%5 == 0 {
			prompt = "err"
		}
		_, _ = m.Chat(context.Background(), mockReq(prompt))
	}
	if tr.tapped.Load() {
		t.Error("tripwire tapped: the mock is not fully offline")
	}
}

// TestMockValidationBeforeAnyDraw: an invalid request is a permanent
// error decided before any counter or draw advances — so the transcript
// after it is unperturbed.
func TestMockValidationBeforeAnyDraw(t *testing.T) {
	t.Parallel()
	m, err := llm.NewMock(llm.MockConfig{Rules: []llm.MockRule{{OnCall: 1, Respond: []llm.MockOutcome{{Text: "first-call"}}}}})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	invalid := mockReq("x")
	invalid.Model = ""
	_, err = m.Chat(context.Background(), invalid)
	var perr *llm.Error
	if !errors.As(err, &perr) || perr.Class != dag.ClassPermanent || perr.Status != 0 {
		t.Fatalf("invalid request error = %v, want permanent *llm.Error status 0", err)
	}
	// The OnCall:1 rule still fires on the first VALID call — the invalid
	// one consumed no ordinal.
	resp, err := m.Chat(context.Background(), mockReq("x"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text() != "first-call" {
		t.Errorf("text = %q, want first-call (invalid request must not consume the call ordinal)", resp.Text())
	}
}

// TestMockConstructionValidation pins the config-error matrix and the
// manifest's ADR-009 conformance (kind, name, flags: cacheable, free).
func TestMockConstructionValidation(t *testing.T) {
	t.Parallel()
	bad := []llm.MockConfig{
		{Latency: llm.LatencySpec{Min: 2 * time.Second, Max: time.Second}},
		{Latency: llm.LatencySpec{Fixed: -1}},
		{Inject: llm.MockInjection{Rate429: 1.5}},
		{Inject: llm.MockInjection{Rate500: -0.1}},
		{Rules: []llm.MockRule{{Substring: "x"}}},                                        // no responses
		{Rules: []llm.MockRule{{Pattern: "(", Respond: []llm.MockOutcome{{Text: "x"}}}}}, // bad regex
	}
	for i, c := range bad {
		if _, err := llm.NewMock(c); err == nil {
			t.Errorf("config %d: NewMock accepted a malformed script", i)
		}
	}

	m, err := llm.NewMock(llm.MockConfig{})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	want := plugin.Manifest{
		Kind:         plugin.KindModelProvider,
		Name:         llm.ProviderMock,
		Version:      "1.0.0",
		Description:  "Deterministic offline simulation provider for tests and load.",
		Capabilities: plugin.Capabilities{Cacheable: true},
	}
	got := m.Manifest()
	if err := got.Validate(); err != nil {
		t.Errorf("manifest fails validation: %v", err)
	}
	if got.Kind != want.Kind || got.Name != want.Name || got.Version != want.Version || got.Capabilities != want.Capabilities {
		t.Errorf("manifest = %+v, want %+v", got, want)
	}
	if got.Capabilities.CostBearing || got.Capabilities.SideEffectful {
		t.Errorf("mock must be free and side-effect-free, got %+v", got.Capabilities)
	}
}
