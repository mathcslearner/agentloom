package llm_test

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

// testKey builds an sk-ant-shaped key at runtime — never a committed
// literal (the 6.1 CI secret grep) — with a marker distinctive enough
// that the hygiene test can search error output for it.
func testKey() string {
	return "sk-ant-" + strings.Repeat("hygienemarker", 3)
}

// fixture reads one recorded response body from testdata/anthropic.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "anthropic", name)) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return raw
}

// newProvider spins an httptest server around handler and builds an
// Anthropic provider pointed at it.
func newProvider(t *testing.T, key string, handler http.Handler) *llm.Anthropic {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := llm.NewAnthropic(llm.AnthropicConfig{
		APIKey:     key,
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	return provider
}

// serveFixture is a handler answering every request with one recorded
// body, optionally capturing the outgoing request body.
func serveFixture(t *testing.T, status int, body []byte, headers map[string]string, captured *[]byte) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading request body: %v", err)
			}
			*captured = raw
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func basicRequest() llm.ChatRequest {
	return llm.ChatRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 256,
		Messages:  []llm.Message{llm.UserText("What is the capital of France?")},
	}
}

func fullRequest() llm.ChatRequest {
	temp := 0.2
	return llm.ChatRequest{
		Model:     "claude-sonnet-5",
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

// requireJSONEqual compares two JSON documents structurally.
func requireJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gv, wv any
	if err := json.Unmarshal(got, &gv); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &wv); err != nil {
		t.Fatalf("want is not JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(gv, wv) {
		t.Errorf("JSON mismatch:\ngot:  %s\nwant: %s", got, want)
	}
}

// TestAnthropicChatSuccess drives the basic text call against the
// recorded success fixture: the outgoing wire request is pinned against
// its golden fixture (headers and body — the contract 8.4/8.6 build
// on), and the decoded response carries the blocks and exact usage.
func TestAnthropicChatSuccess(t *testing.T) {
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
		_, _ = w.Write(fixture(t, "success.json"))
	})
	provider := newProvider(t, key, handler)

	resp, err := provider.Chat(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/messages" {
		t.Errorf("request = %s %s, want POST /v1/messages", gotMethod, gotPath)
	}
	if got := gotHeaders.Get("x-api-key"); got != key {
		t.Errorf("x-api-key header = %q, want the configured key", got)
	}
	if got := gotHeaders.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version header = %q, want 2023-06-01", got)
	}
	if got := gotHeaders.Get("content-type"); got != "application/json" {
		t.Errorf("content-type header = %q, want application/json", got)
	}
	requireJSONEqual(t, captured, fixture(t, "request_basic.json"))

	want := llm.ChatResponse{
		Model:      "claude-sonnet-5",
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

// TestAnthropicChatToolUse drives the full-shape request (system,
// temperature, tools, tool_use/tool_result history) against the
// tool-use fixture, pinning both the golden wire request and the
// tool-call decoding.
func TestAnthropicChatToolUse(t *testing.T) {
	t.Parallel()

	var captured []byte
	provider := newProvider(t, testKey(),
		serveFixture(t, http.StatusOK, fixture(t, "success_tool_use.json"), nil, &captured))

	resp, err := provider.Chat(context.Background(), fullRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}
	requireJSONEqual(t, captured, fixture(t, "request_full.json"))

	if resp.StopReason != "tool_use" {
		t.Errorf("stop reason = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage != (llm.Usage{InputTokens: 302, OutputTokens: 57}) {
		t.Errorf("usage = %+v, want {302 57}", resp.Usage)
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("ToolUses() returned %d calls, want 1", len(uses))
	}
	if uses[0].ID != "toolu_01A09q90qw90lq917835lq9" || uses[0].Name != "get_weather" {
		t.Errorf("tool use = %+v", uses[0])
	}
	requireJSONEqual(t, uses[0].Input, []byte(`{"location": "Paris", "unit": "celsius"}`))
	if resp.Text() != "I'll check the weather for you." {
		t.Errorf("Text() = %q", resp.Text())
	}
}

// TestAnthropicStructuredOutput asserts the ticket 11.3 native structured
// path: a ResponseFormat with a schema is encoded as a forced synthetic tool
// (golden request), and the matching tool_use response is lifted onto
// ChatResponse.Structured rather than surfaced as a user-visible tool call.
func TestAnthropicStructuredOutput(t *testing.T) {
	t.Parallel()

	var captured []byte
	provider := newProvider(t, testKey(),
		serveFixture(t, http.StatusOK, fixture(t, "success_structured.json"), nil, &captured))

	req := llm.ChatRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 256,
		Messages:  []llm.Message{llm.UserText("Extract the title.")},
		ResponseFormat: &llm.ResponseFormat{
			Schema: json.RawMessage(`{"type": "object", "properties": {"title": {"type": "string"}}, "required": ["title"]}`),
		},
	}
	resp, err := provider.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}
	requireJSONEqual(t, captured, fixture(t, "request_structured.json"))

	if string(resp.Structured) == "" {
		t.Fatal("Structured is empty; forced tool_use was not lifted")
	}
	requireJSONEqual(t, resp.Structured, []byte(`{"title": "Hello"}`))
	if len(resp.ToolUses()) != 0 {
		t.Errorf("structured tool leaked as a user-visible tool call: %+v", resp.ToolUses())
	}
	if resp.Text() != "" {
		t.Errorf("Text() = %q, want empty for a pure structured response", resp.Text())
	}
}

// TestAnthropicErrorMapping is the ticket's taxonomy matrix: every
// recorded provider error shape mapped onto its ADR-006 class, code,
// retry-after, and request id.
func TestAnthropicErrorMapping(t *testing.T) {
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
			name:   "rate limit 429 with retry-after",
			status: http.StatusTooManyRequests,
			body:   fixture(t, "rate_limit_429.json"),
			headers: map[string]string{
				"retry-after": "30",
				"request-id":  "req_011CSHoEeqs5DhSbbPBGiEjT",
			},
			wantClass:      dag.ClassTransient,
			wantCode:       "rate_limit_error",
			wantRetryAfter: 30 * time.Second,
			wantRequestID:  "req_011CSHoEeqs5DhSbbPBGiEjT",
		},
		{
			name:           "rate limit 429 with unparseable retry-after",
			status:         http.StatusTooManyRequests,
			body:           fixture(t, "rate_limit_429.json"),
			headers:        map[string]string{"retry-after": "soonish"},
			wantClass:      dag.ClassTransient,
			wantCode:       "rate_limit_error",
			wantRetryAfter: 0,
		},
		{
			name:           "overloaded 529",
			status:         529,
			body:           fixture(t, "overloaded_529.json"),
			headers:        map[string]string{"retry-after": "5"},
			wantClass:      dag.ClassTransient,
			wantCode:       "overloaded_error",
			wantRetryAfter: 5 * time.Second,
		},
		{
			name:      "api error 500",
			status:    http.StatusInternalServerError,
			body:      fixture(t, "api_error_500.json"),
			wantClass: dag.ClassTransient,
			wantCode:  "api_error",
		},
		{
			name:      "invalid request 400",
			status:    http.StatusBadRequest,
			body:      fixture(t, "invalid_request_400.json"),
			wantClass: dag.ClassPermanent,
			wantCode:  "invalid_request_error",
		},
		{
			name:      "authentication 401",
			status:    http.StatusUnauthorized,
			body:      fixture(t, "auth_401.json"),
			wantClass: dag.ClassPermanent,
			wantCode:  "authentication_error",
		},
		{
			name:      "not found 404",
			status:    http.StatusNotFound,
			body:      fixture(t, "not_found_404.json"),
			wantClass: dag.ClassPermanent,
			wantCode:  "not_found_error",
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
			provider := newProvider(t, testKey(), serveFixture(t, tc.status, tc.body, tc.headers, nil))
			_, err := provider.Chat(context.Background(), basicRequest())
			var perr *llm.Error
			if !errors.As(err, &perr) {
				t.Fatalf("Chat error = %v (%T), want *llm.Error", err, err)
			}
			if perr.Provider != llm.ProviderAnthropic {
				t.Errorf("Provider = %q, want %q", perr.Provider, llm.ProviderAnthropic)
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

// TestAnthropicMalformedSuccess pins the malformed-200 paths: a body
// that is not JSON, a 200 without usage (the ticket mandates usage on
// every success), and a content block this build cannot represent —
// each a transient *Error, never a silent partial response.
func TestAnthropicMalformedSuccess(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		body        []byte
		wantMention string
	}{
		{"not JSON", []byte("definitely not json"), "undecodable success body"},
		{
			"missing usage",
			[]byte(`{"model": "claude-sonnet-5", "stop_reason": "end_turn", "content": [{"type": "text", "text": "hi"}]}`),
			"no usage",
		},
		{
			"unsupported block type",
			[]byte(`{"model": "claude-sonnet-5", "stop_reason": "end_turn", "content": [{"type": "hologram"}], "usage": {"input_tokens": 1, "output_tokens": 1}}`),
			"hologram",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := newProvider(t, testKey(), serveFixture(t, http.StatusOK, tc.body, nil, nil))
			_, err := provider.Chat(context.Background(), basicRequest())
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

// TestAnthropicTransportError pins the no-response path: a connection
// failure is a transient *Error with Status 0.
func TestAnthropicTransportError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.NotFoundHandler())
	provider, err := llm.NewAnthropic(llm.AnthropicConfig{
		APIKey:     testKey(),
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	server.Close() // the dial now fails

	_, err = provider.Chat(context.Background(), basicRequest())
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

// TestAnthropicContextCancelled pins the pass-through rule (ADR-006
// rows 3/8): a context abort is NOT classified by the provider — it
// surfaces as a wrapped context error, never as *llm.Error, so the
// engine judges timeout vs. cancelled from context state.
func TestAnthropicContextCancelled(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	provider := newProvider(t, testKey(), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Consume the body: the server starts watching for a client
		// disconnect only once the request is read, and the disconnect is
		// what cancels r.Context() here.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	// Registered after newProvider's server.Close cleanup, so this runs
	// FIRST (cleanups are LIFO): Close waits for in-flight handlers, so
	// releasing them must come before it.
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, err := provider.Chat(ctx, basicRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat error = %v, want a context.Canceled chain", err)
	}
	var perr *llm.Error
	if errors.As(err, &perr) {
		t.Errorf("context abort classified as *llm.Error (%+v); the engine owns that judgment", perr)
	}
}

// TestAnthropicClientSideValidation pins that an invalid request is a
// permanent *Error decided before any network round-trip.
func TestAnthropicClientSideValidation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	provider := newProvider(t, testKey(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	invalid := basicRequest()
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

// TestAnthropicNoKeyMaterialInErrors is the ticket's secret-hygiene
// assertion: the configured API key appears in no error string, no
// formatted rendering, and no link of any error chain, across every
// error path — with the 400 fixture's marker message as the positive
// control proving the assertion would catch leaked content.
func TestAnthropicNoKeyMaterialInErrors(t *testing.T) {
	t.Parallel()

	key := testKey()
	collect := func(status int, body []byte) error {
		t.Helper()
		provider := newProvider(t, key, serveFixture(t, status, body, map[string]string{"request-id": "req_hygiene"}, nil))
		_, err := provider.Chat(context.Background(), basicRequest())
		if err == nil {
			t.Fatal("expected an error")
		}
		return err
	}

	errs := map[string]error{
		"auth 401":        collect(http.StatusUnauthorized, fixture(t, "auth_401.json")),
		"invalid 400":     collect(http.StatusBadRequest, fixture(t, "invalid_request_400.json")),
		"rate limit 429":  collect(http.StatusTooManyRequests, fixture(t, "rate_limit_429.json")),
		"malformed 200":   collect(http.StatusOK, []byte("not json")),
		"undecodable 503": collect(http.StatusServiceUnavailable, []byte("<html></html>")),
	}

	// Transport and validation errors too.
	deadServer := httptest.NewServer(http.NotFoundHandler())
	deadProvider, err := llm.NewAnthropic(llm.AnthropicConfig{APIKey: key, BaseURL: deadServer.URL, HTTPClient: deadServer.Client()})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	deadServer.Close()
	if _, terr := deadProvider.Chat(context.Background(), basicRequest()); terr != nil {
		errs["transport"] = terr
	}
	invalid := basicRequest()
	invalid.MaxTokens = 0
	liveProvider := newProvider(t, key, http.NotFoundHandler())
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

	// Positive control: provider-authored content DOES flow through, so
	// the assertions above have teeth.
	if !strings.Contains(errs["invalid 400"].Error(), "MARKER_PROVIDER_MESSAGE") {
		t.Errorf("positive control missing: 400 error %q does not carry the fixture's marker message", errs["invalid 400"])
	}
	if !strings.Contains(errs["auth 401"].Error(), "req_hygiene") {
		t.Errorf("positive control missing: 401 error %q does not carry the request id", errs["auth 401"])
	}
}

// TestAnthropicConstruction pins boot-time key enforcement and the
// manifest's ADR-009 conformance.
func TestAnthropicConstruction(t *testing.T) {
	t.Parallel()

	if _, err := llm.NewAnthropic(llm.AnthropicConfig{}); err == nil {
		t.Error("NewAnthropic without a key: want error, got nil")
	}

	provider, err := llm.NewAnthropic(llm.AnthropicConfig{APIKey: testKey()})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	m := provider.Manifest()
	if err := m.Validate(); err != nil {
		t.Errorf("Manifest fails validation: %v", err)
	}
	want := plugin.Manifest{
		Kind:         plugin.KindModelProvider,
		Name:         llm.ProviderAnthropic,
		Version:      "1.0.0",
		Description:  m.Description,
		Capabilities: plugin.Capabilities{Cacheable: true, CostBearing: true},
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("Manifest = %+v, want %+v", m, want)
	}
}
