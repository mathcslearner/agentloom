package llm_test

// Ticket 8.4: the OpenAI provider mirrors 8.3's Anthropic coverage —
// golden wire-request pinning, the taxonomy matrix, malformed-200
// paths, transport/context/validation paths, and the secret-hygiene
// assertion. Shared helpers (testKey, serveFixture, requireJSONEqual)
// live in anthropic_test.go, same test package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// openaiFixture reads one recorded response body from testdata/openai.
func openaiFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "openai", name)) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return raw
}

// newOpenAIProvider spins an httptest server around handler and builds
// an OpenAI provider pointed at it.
func newOpenAIProvider(t *testing.T, key string, handler http.Handler) *llm.OpenAI {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := llm.NewOpenAI(llm.OpenAIConfig{
		APIKey:     key,
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	return provider
}

func openaiBasicRequest() llm.ChatRequest {
	return llm.ChatRequest{
		Model:     "gpt-4o",
		MaxTokens: 256,
		Messages:  []llm.Message{llm.UserText("What is the capital of France?")},
	}
}

func openaiFullRequest() llm.ChatRequest {
	temp := 0.2
	return llm.ChatRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		System:    "Answer concisely.",
		Messages: []llm.Message{
			llm.UserText("Weather in Paris?"),
			{Role: llm.RoleAssistant, Blocks: []llm.Block{
				llm.TextBlock("I'll check the weather for you."),
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{
					ID:    "toolu_01A09q90qw90lq917835lq9",
					Name:  "get_weather",
					Input: json.RawMessage(`{"location": "Paris"}`),
				}},
			}},
			{Role: llm.RoleUser, Blocks: []llm.Block{
				{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{
					ToolUseID: "toolu_01A09q90qw90lq917835lq9",
					Content:   "18C, clear",
					IsError:   true,
				}},
			}},
		},
		Tools: []llm.ToolDef{{
			Name:        "get_weather",
			Description: "Get the current weather for a location.",
			InputSchema: json.RawMessage(`{"type": "object", "properties": {"location": {"type": "string"}}, "required": ["location"]}`),
		}},
		Temperature: &temp,
	}
}

// TestOpenAIChatSuccess drives the basic text call: the outgoing wire
// request is pinned against its golden fixture (headers + body — the
// system-message folding and max_completion_tokens mapping 8.6 builds
// on), and the decoded response carries the block and exact usage with
// the finish reason normalized onto the unified vocabulary.
func TestOpenAIChatSuccess(t *testing.T) {
	t.Parallel()

	key := testKey()
	var captured []byte
	var gotHeaders http.Header
	var gotMethod, gotPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotHeaders = r.Header.Clone()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		captured = raw
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(openaiFixture(t, "success.json"))
	})
	provider := newOpenAIProvider(t, key, handler)

	resp, err := provider.Chat(context.Background(), openaiBasicRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/chat/completions" {
		t.Errorf("request = %s %s, want POST /v1/chat/completions", gotMethod, gotPath)
	}
	if got := gotHeaders.Get("authorization"); got != "Bearer "+key {
		t.Errorf("authorization header = %q, want the bearer-prefixed key", got)
	}
	if got := gotHeaders.Get("content-type"); got != "application/json" {
		t.Errorf("content-type header = %q, want application/json", got)
	}
	requireJSONEqual(t, captured, openaiFixture(t, "request_basic.json"))

	want := llm.ChatResponse{
		Model:      "gpt-4o-2024-08-06",
		StopReason: "end_turn",
		Blocks:     []llm.Block{llm.TextBlock("The capital of France is Paris.")},
		Usage:      llm.Usage{InputTokens: 14, OutputTokens: 9},
	}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("response = %+v, want %+v", resp, want)
	}
	if resp.Text() != "The capital of France is Paris." {
		t.Errorf("Text() = %q", resp.Text())
	}
}

// TestOpenAIChatToolCalls drives the full-shape request (system,
// temperature, tools, tool_use/tool_result history) against the
// tool-calls fixture, pinning both the golden wire request — the
// tool_result → tool-role fan-out and JSON-string arguments — and the
// tool-call decoding.
func TestOpenAIChatToolCalls(t *testing.T) {
	t.Parallel()

	var captured []byte
	provider := newOpenAIProvider(t, testKey(),
		serveFixture(t, http.StatusOK, openaiFixture(t, "success_tool_calls.json"), nil, &captured))

	resp, err := provider.Chat(context.Background(), openaiFullRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}
	requireJSONEqual(t, captured, openaiFixture(t, "request_full.json"))

	if resp.StopReason != "tool_use" {
		t.Errorf("stop reason = %q, want tool_use (normalized from tool_calls)", resp.StopReason)
	}
	if resp.Usage != (llm.Usage{InputTokens: 302, OutputTokens: 57}) {
		t.Errorf("usage = %+v, want {302 57}", resp.Usage)
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("ToolUses() returned %d calls, want 1", len(uses))
	}
	if uses[0].ID != "call_abc123" || uses[0].Name != "get_weather" {
		t.Errorf("tool use = %+v", uses[0])
	}
	requireJSONEqual(t, uses[0].Input, []byte(`{"location": "Paris", "unit": "celsius"}`))
	if resp.Text() != "I'll check the weather for you." {
		t.Errorf("Text() = %q", resp.Text())
	}
}

// TestOpenAIChatRefusal pins that a refusal is surfaced as the
// completion's text, not dropped into an empty response.
func TestOpenAIChatRefusal(t *testing.T) {
	t.Parallel()

	provider := newOpenAIProvider(t, testKey(),
		serveFixture(t, http.StatusOK, openaiFixture(t, "success_refusal.json"), nil, nil))
	resp, err := provider.Chat(context.Background(), openaiBasicRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}
	if resp.Text() != "I'm sorry, but I can't help with that." {
		t.Errorf("Text() = %q, want the refusal text", resp.Text())
	}
	if resp.Usage != (llm.Usage{InputTokens: 20, OutputTokens: 11}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// TestOpenAIErrorMapping is the taxonomy matrix: every recorded provider
// error shape mapped onto its ADR-006 class, code, retry-after, and
// request id — identical mapping rules to Anthropic (same classifier).
func TestOpenAIErrorMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		status         int
		body           []byte
		headers        map[string]string
		wantClass      dag.ErrorClass
		wantCode       string
		wantRetryAfter time.Duration
		wantRequestID  string
	}{
		{
			name:   "rate limit 429 with retry-after seconds",
			status: http.StatusTooManyRequests,
			body:   openaiFixture(t, "rate_limit_429.json"),
			headers: map[string]string{
				"retry-after":  "30",
				"x-request-id": "req_openai_abc",
			},
			wantClass:      dag.ClassTransient,
			wantCode:       "rate_limit_exceeded",
			wantRetryAfter: 30 * time.Second,
			wantRequestID:  "req_openai_abc",
		},
		{
			name:   "rate limit 429 with retry-after-ms taking precedence",
			status: http.StatusTooManyRequests,
			body:   openaiFixture(t, "rate_limit_429.json"),
			headers: map[string]string{
				"retry-after":    "30",
				"retry-after-ms": "1500",
			},
			wantClass:      dag.ClassTransient,
			wantCode:       "rate_limit_exceeded",
			wantRetryAfter: 1500 * time.Millisecond,
		},
		{
			name:           "rate limit 429 with unparseable retry-after",
			status:         http.StatusTooManyRequests,
			body:           openaiFixture(t, "rate_limit_429.json"),
			headers:        map[string]string{"retry-after": "soonish"},
			wantClass:      dag.ClassTransient,
			wantCode:       "rate_limit_exceeded",
			wantRetryAfter: 0,
		},
		{
			name:      "server error 500",
			status:    http.StatusInternalServerError,
			body:      openaiFixture(t, "server_error_500.json"),
			wantClass: dag.ClassTransient,
			wantCode:  "server_error", // code null → falls back to type
		},
		{
			name:      "invalid request 400",
			status:    http.StatusBadRequest,
			body:      openaiFixture(t, "invalid_request_400.json"),
			wantClass: dag.ClassPermanent,
			wantCode:  "invalid_request_error", // code null → falls back to type
		},
		{
			name:      "context length 400",
			status:    http.StatusBadRequest,
			body:      openaiFixture(t, "context_length_400.json"),
			wantClass: dag.ClassPermanent,
			wantCode:  "context_length_exceeded",
		},
		{
			name:      "authentication 401",
			status:    http.StatusUnauthorized,
			body:      openaiFixture(t, "auth_401.json"),
			wantClass: dag.ClassPermanent,
			wantCode:  "invalid_api_key",
		},
		{
			name:      "model not found 404",
			status:    http.StatusNotFound,
			body:      openaiFixture(t, "model_not_found_404.json"),
			wantClass: dag.ClassPermanent,
			wantCode:  "model_not_found",
		},
		{
			name:      "unknown 4xx with undecodable body",
			status:    http.StatusTeapot,
			body:      []byte("<html>teapot</html>"),
			wantClass: dag.ClassPermanent,
		},
		{
			name:      "unknown 5xx with undecodable body",
			status:    http.StatusServiceUnavailable,
			body:      []byte("upstream connect error"),
			wantClass: dag.ClassTransient,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := newOpenAIProvider(t, testKey(), serveFixture(t, tc.status, tc.body, tc.headers, nil))
			_, err := provider.Chat(context.Background(), openaiBasicRequest())
			var perr *llm.Error
			if !errors.As(err, &perr) {
				t.Fatalf("Chat error = %v (%T), want *llm.Error", err, err)
			}
			if perr.Provider != llm.ProviderOpenAI {
				t.Errorf("Provider = %q, want %q", perr.Provider, llm.ProviderOpenAI)
			}
			if perr.Class != tc.wantClass {
				t.Errorf("Class = %q, want %q", perr.Class, tc.wantClass)
			}
			if perr.Status != tc.status {
				t.Errorf("Status = %d, want %d", perr.Status, tc.status)
			}
			if perr.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", perr.Code, tc.wantCode)
			}
			if perr.RetryAfter != tc.wantRetryAfter {
				t.Errorf("RetryAfter = %v, want %v", perr.RetryAfter, tc.wantRetryAfter)
			}
			if perr.RequestID != tc.wantRequestID {
				t.Errorf("RequestID = %q, want %q", perr.RequestID, tc.wantRequestID)
			}
		})
	}
}

// TestOpenAIFinishReasonNormalization pins the finish_reason → unified
// stop-reason mapping, including the unknown-passthrough rule.
func TestOpenAIFinishReasonNormalization(t *testing.T) {
	t.Parallel()

	cases := []struct{ finish, want string }{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"function_call", "tool_use"},
		{"content_filter", "content_filter"}, // unknown → verbatim
	}
	for _, tc := range cases {
		t.Run(tc.finish, func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(`{"model":"gpt-4o","choices":[{"index":0,"finish_reason":%q,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, tc.finish)
			provider := newOpenAIProvider(t, testKey(), serveFixture(t, http.StatusOK, []byte(body), nil, nil))
			resp, err := provider.Chat(context.Background(), openaiBasicRequest())
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if resp.StopReason != tc.want {
				t.Errorf("stop reason = %q, want %q", resp.StopReason, tc.want)
			}
		})
	}
}

// TestOpenAIMalformedSuccess pins the malformed-200 paths: not JSON, a
// 200 without usage, a 200 with no choices, and a tool call this build
// cannot represent — each a transient *Error, never a silent partial
// response.
func TestOpenAIMalformedSuccess(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		body        []byte
		wantMention string
	}{
		{"not JSON", []byte("definitely not json"), "undecodable success body"},
		{
			"missing usage",
			[]byte(`{"model":"gpt-4o","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`),
			"no usage",
		},
		{
			"no choices",
			[]byte(`{"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`),
			"no choices",
		},
		{
			"unsupported tool-call type",
			[]byte(`{"model":"gpt-4o","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"hologram","function":{"name":"x","arguments":"{}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`),
			"hologram",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := newOpenAIProvider(t, testKey(), serveFixture(t, http.StatusOK, tc.body, nil, nil))
			_, err := provider.Chat(context.Background(), openaiBasicRequest())
			var perr *llm.Error
			if !errors.As(err, &perr) {
				t.Fatalf("Chat error = %v (%T), want *llm.Error", err, err)
			}
			if perr.Class != dag.ClassTransient {
				t.Errorf("Class = %q, want transient", perr.Class)
			}
			if !strings.Contains(err.Error(), tc.wantMention) {
				t.Errorf("error %q does not mention %q", err, tc.wantMention)
			}
		})
	}
}

// TestOpenAITransportError pins the no-response path: a connection
// failure is a transient *Error with Status 0.
func TestOpenAITransportError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.NotFoundHandler())
	provider, err := llm.NewOpenAI(llm.OpenAIConfig{
		APIKey:     testKey(),
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	server.Close() // the dial now fails

	_, err = provider.Chat(context.Background(), openaiBasicRequest())
	var perr *llm.Error
	if !errors.As(err, &perr) {
		t.Fatalf("Chat error = %v (%T), want *llm.Error", err, err)
	}
	if perr.Class != dag.ClassTransient || perr.Status != 0 {
		t.Errorf("got class %q status %d, want transient status 0", perr.Class, perr.Status)
	}
	if perr.Unwrap() == nil {
		t.Error("transport error carries no cause")
	}
}

// TestOpenAIContextCancelled pins the pass-through rule (ADR-006 rows
// 3/8): a context abort surfaces as a wrapped context error, never as
// *llm.Error.
func TestOpenAIContextCancelled(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	provider := newOpenAIProvider(t, testKey(), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, err := provider.Chat(ctx, openaiBasicRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat error = %v, want a context.Canceled chain", err)
	}
	var perr *llm.Error
	if errors.As(err, &perr) {
		t.Errorf("context abort classified as *llm.Error (%+v); the engine owns that judgment", perr)
	}
}

// TestOpenAIClientSideValidation pins that an invalid request is a
// permanent *Error decided before any network round-trip.
func TestOpenAIClientSideValidation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	provider := newOpenAIProvider(t, testKey(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	invalid := openaiBasicRequest()
	invalid.Model = ""
	_, err := provider.Chat(context.Background(), invalid)
	var perr *llm.Error
	if !errors.As(err, &perr) {
		t.Fatalf("Chat error = %v (%T), want *llm.Error", err, err)
	}
	if perr.Class != dag.ClassPermanent {
		t.Errorf("Class = %q, want permanent", perr.Class)
	}
	if perr.Status != 0 {
		t.Errorf("Status = %d, want 0 (no call made)", perr.Status)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("provider made %d HTTP calls for an invalid request, want 0", got)
	}
}

// TestOpenAINoKeyMaterialInErrors is the secret-hygiene assertion: the
// configured API key appears in no error string, formatted rendering, or
// link of any error chain, across every error path — with the 400
// fixture's marker message and the request id as positive controls.
func TestOpenAINoKeyMaterialInErrors(t *testing.T) {
	t.Parallel()

	key := testKey()
	collect := func(status int, body []byte) error {
		t.Helper()
		provider := newOpenAIProvider(t, key, serveFixture(t, status, body, map[string]string{"x-request-id": "req_hygiene"}, nil))
		_, err := provider.Chat(context.Background(), openaiBasicRequest())
		if err == nil {
			t.Fatal("expected an error")
		}
		return err
	}

	errs := map[string]error{
		"auth 401":        collect(http.StatusUnauthorized, openaiFixture(t, "auth_401.json")),
		"invalid 400":     collect(http.StatusBadRequest, openaiFixture(t, "invalid_request_400.json")),
		"rate limit 429":  collect(http.StatusTooManyRequests, openaiFixture(t, "rate_limit_429.json")),
		"malformed 200":   collect(http.StatusOK, []byte("not json")),
		"undecodable 503": collect(http.StatusServiceUnavailable, []byte("<html></html>")),
	}

	deadServer := httptest.NewServer(http.NotFoundHandler())
	deadProvider, err := llm.NewOpenAI(llm.OpenAIConfig{APIKey: key, BaseURL: deadServer.URL, HTTPClient: deadServer.Client()})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	deadServer.Close()
	if _, terr := deadProvider.Chat(context.Background(), openaiBasicRequest()); terr != nil {
		errs["transport"] = terr
	}
	invalid := openaiBasicRequest()
	invalid.MaxTokens = 0
	liveProvider := newOpenAIProvider(t, key, http.NotFoundHandler())
	if _, verr := liveProvider.Chat(context.Background(), invalid); verr != nil {
		errs["client validation"] = verr
	}

	for name, e := range errs {
		for chain := e; chain != nil; chain = errors.Unwrap(chain) {
			for _, rendering := range []string{chain.Error(), fmt.Sprintf("%+v", chain), fmt.Sprintf("%#v", chain)} {
				if strings.Contains(rendering, key) || strings.Contains(rendering, "hygienemarker") {
					t.Errorf("%s: key material leaked into error output: %s", name, rendering)
				}
			}
		}
	}

	// Positive controls: provider-authored content DOES flow through.
	if !strings.Contains(errs["invalid 400"].Error(), "MARKER_PROVIDER_MESSAGE") {
		t.Errorf("positive control missing: 400 error %q does not carry the fixture's marker message", errs["invalid 400"])
	}
	if !strings.Contains(errs["auth 401"].Error(), "req_hygiene") {
		t.Errorf("positive control missing: 401 error %q does not carry the request id", errs["auth 401"])
	}
}

// TestOpenAIConstruction pins boot-time key enforcement and the
// manifest's ADR-009 conformance.
func TestOpenAIConstruction(t *testing.T) {
	t.Parallel()

	if _, err := llm.NewOpenAI(llm.OpenAIConfig{}); err == nil {
		t.Error("NewOpenAI without a key: want error, got nil")
	}

	provider, err := llm.NewOpenAI(llm.OpenAIConfig{APIKey: testKey()})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	m := provider.Manifest()
	if err := m.Validate(); err != nil {
		t.Errorf("Manifest fails validation: %v", err)
	}
	want := plugin.Manifest{
		Kind:         plugin.KindModelProvider,
		Name:         llm.ProviderOpenAI,
		Version:      "1.0.0",
		Description:  m.Description,
		Capabilities: plugin.Capabilities{Cacheable: true, CostBearing: true},
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("Manifest = %+v, want %+v", m, want)
	}
}
