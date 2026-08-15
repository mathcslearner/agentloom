package validate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// funcProvider is an in-test llm.Provider that delegates Chat to a closure,
// so a test drives exactly what the judge sees back. Registered under name
// "mock" so config models "mock/<model>" route to it (the served model the
// closure inspects is req.Model, the namespace-stripped form).
type funcProvider struct {
	fn   func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)
	reqs []llm.ChatRequest
}

func (p *funcProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	p.reqs = append(p.reqs, req)
	return p.fn(ctx, req)
}

func (p *funcProvider) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind: plugin.KindModelProvider, Name: "mock", Version: "1.0.0",
		Capabilities: plugin.Capabilities{Cacheable: true, CostBearing: true},
	}
}

func judgeRegistry(t *testing.T, p *funcProvider) *llm.Registry {
	t.Helper()
	reg, err := llm.NewRegistry(p)
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	return reg
}

// answer builds a successful judge ChatResponse with the given served model
// and a text {score, rationale} answer.
func answer(model string, score float64, rationale string) llm.ChatResponse {
	body, _ := json.Marshal(map[string]any{"score": score, "rationale": rationale})
	return llm.ChatResponse{
		Model: model, StopReason: "end_turn",
		Blocks: []llm.Block{llm.TextBlock(string(body))},
		Usage:  llm.Usage{InputTokens: 42, OutputTokens: 7},
	}
}

func judgeInput(config string, value string) validate.Input {
	return validate.Input{
		StepType: dag.StepLLM,
		Output:   json.RawMessage(`{"text":` + mustJSONString(value) + `}`),
		Value:    json.RawMessage(mustJSONString(value)),
		Config:   json.RawMessage(config),
		Attempt:  1,
	}
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestJudgePassAndUsage: a score at/above the threshold passes, and the
// verdict carries the rationale, a score, and the judge's usage (the engine's
// overhead-ledger input) keyed on the resolved resource.
func TestJudgePassAndUsage(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return answer(req.Model, 0.9, "clear and complete"), nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"is it good?","threshold":0.7}`
	verdict, err := v.Validate(context.Background(), judgeInput(config, "the answer"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !verdict.Passed() {
		t.Fatalf("verdict = %+v, want pass", verdict)
	}
	if verdict.Score == nil || *verdict.Score != 0.9 {
		t.Errorf("score = %v, want 0.9", verdict.Score)
	}
	if len(verdict.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(verdict.Results))
	}
	r := verdict.Results[0]
	if r.Rationale != "clear and complete" {
		t.Errorf("rationale = %q", r.Rationale)
	}
	if r.Usage == nil {
		t.Fatal("result usage is nil; the engine needs it to ledger overhead")
	}
	if r.Usage.Resource != "mock:judge-1" {
		t.Errorf("usage resource = %q, want mock:judge-1", r.Usage.Resource)
	}
	if r.Usage.InputTokens != 42 || r.Usage.OutputTokens != 7 {
		t.Errorf("usage tokens = %d/%d, want 42/7", r.Usage.InputTokens, r.Usage.OutputTokens)
	}
}

// TestJudgeFailBelowThreshold: a below-threshold score fails with a
// rubric_below_threshold issue whose message carries the model's rationale —
// the critique 11.4's semantic retry folds into the next prompt.
func TestJudgeFailBelowThreshold(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return answer(req.Model, 0.3, "too terse; missing the summary"), nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"needs a summary","threshold":0.7}`
	verdict, err := v.Validate(context.Background(), judgeInput(config, "one liner"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if verdict.Passed() {
		t.Fatal("expected a fail verdict below threshold")
	}
	if len(verdict.Issues) != 1 || verdict.Issues[0].Code != "rubric_below_threshold" {
		t.Fatalf("issues = %+v, want one rubric_below_threshold", verdict.Issues)
	}
	if !strings.Contains(verdict.Issues[0].Message, "too terse") {
		t.Errorf("issue message %q does not carry the rationale", verdict.Issues[0].Message)
	}
	if verdict.Results[0].Status != validate.StatusFail {
		t.Errorf("result status = %q, want fail", verdict.Results[0].Status)
	}
}

// TestJudgeThresholdBoundary: a score exactly at the threshold passes (>=).
func TestJudgeThresholdBoundary(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return answer(req.Model, 0.5, "borderline"), nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"x","threshold":0.5}`
	verdict, err := v.Validate(context.Background(), judgeInput(config, "y"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !verdict.Passed() {
		t.Errorf("score == threshold should pass; got %+v", verdict)
	}
}

// TestJudgeStructuredAnswer: a provider that answers natively (Structured
// set) is parsed from the structured payload, not the text.
func TestJudgeStructuredAnswer(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{
			Model: req.Model, StopReason: "end_turn",
			Structured: json.RawMessage(`{"score":0.8,"rationale":"native"}`),
			Usage:      llm.Usage{InputTokens: 10, OutputTokens: 3},
		}, nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"x","threshold":0.7}`
	verdict, err := v.Validate(context.Background(), judgeInput(config, "y"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !verdict.Passed() || verdict.Results[0].Rationale != "native" {
		t.Errorf("structured answer not parsed: %+v", verdict)
	}
}

// TestJudgeRepairsFencedAnswer: a free-text answer wrapped in a Markdown code
// fence is repaired by the deterministic JSON-repair pass.
func TestJudgeRepairsFencedAnswer(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{
			Model: req.Model, StopReason: "end_turn",
			Blocks: []llm.Block{llm.TextBlock("```json\n{\"score\": 0.9, \"rationale\": \"good\"}\n```")},
			Usage:  llm.Usage{InputTokens: 1, OutputTokens: 1},
		}, nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"x","threshold":0.7}`
	verdict, err := v.Validate(context.Background(), judgeInput(config, "y"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !verdict.Passed() {
		t.Errorf("fenced answer not repaired: %+v", verdict)
	}
}

// TestJudgeMalformedAnswerFail: a judge answer that cannot be parsed under
// on_error:fail (the default) is a PERMANENT *validate.Error carrying the
// billed usage (the call happened) — the engine meters it as overhead and
// dead-letters/retries per policy.
func TestJudgeMalformedAnswerFail(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{
			Model: req.Model, StopReason: "end_turn",
			Blocks: []llm.Block{llm.TextBlock("I think it's pretty good, honestly.")},
			Usage:  llm.Usage{InputTokens: 5, OutputTokens: 9},
		}, nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"x","threshold":0.7}`
	_, err := v.Validate(context.Background(), judgeInput(config, "y"))
	var ve *validate.Error
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *validate.Error", err)
	}
	if ve.Class != dag.ClassPermanent {
		t.Errorf("class = %q, want permanent (a malformed answer is not availability)", ve.Class)
	}
	if ve.Usage == nil || ve.Usage.Resource != "mock:judge-1" {
		t.Errorf("error usage = %+v, want billed usage on mock:judge-1", ve.Usage)
	}
}

// TestJudgeMalformedAnswerSkip: the same malformed answer under on_error:skip
// suppresses the failure — a PASS verdict whose result records the error and
// still carries the billed usage.
func TestJudgeMalformedAnswerSkip(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{
			Model: req.Model, Blocks: []llm.Block{llm.TextBlock("meh")},
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 9},
		}, nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"x","threshold":0.7,"on_error":"skip"}`
	verdict, err := v.Validate(context.Background(), judgeInput(config, "y"))
	if err != nil {
		t.Fatalf("Validate: %v (skip should not error)", err)
	}
	if !verdict.Passed() {
		t.Fatal("on_error:skip should degrade to a pass")
	}
	r := verdict.Results[0]
	if r.Status != validate.StatusError || r.Error == "" {
		t.Errorf("result = %+v, want status error with a message", r)
	}
	if r.Usage == nil {
		t.Error("billed usage should still ride on a skipped-error result")
	}
}

// TestJudgeProviderErrorFail: every model erroring under on_error:fail is a
// TRANSIENT error with NO usage (nothing billed) — retry may reach a healthy
// provider.
func TestJudgeProviderErrorFail(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{}, &llm.Error{Provider: "mock", Class: dag.ClassTransient, Message: "503"}
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"x","threshold":0.7}`
	_, err := v.Validate(context.Background(), judgeInput(config, "y"))
	var ve *validate.Error
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *validate.Error", err)
	}
	if ve.Class != dag.ClassTransient {
		t.Errorf("class = %q, want transient", ve.Class)
	}
	if ve.Usage != nil {
		t.Errorf("no usage should be metered when nothing billed; got %+v", ve.Usage)
	}
}

// TestJudgeProviderErrorSkip: a provider error under on_error:skip passes with
// an error result and no usage.
func TestJudgeProviderErrorSkip(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{}, &llm.Error{Provider: "mock", Class: dag.ClassTransient, Message: "503"}
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"x","threshold":0.7,"on_error":"skip"}`
	verdict, err := v.Validate(context.Background(), judgeInput(config, "y"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !verdict.Passed() || verdict.Results[0].Status != validate.StatusError {
		t.Errorf("want pass with error result; got %+v", verdict)
	}
	if verdict.Results[0].Usage != nil {
		t.Error("no usage when the provider call did not bill")
	}
}

// TestJudgeFallbackChain: the primary model errors, the fallback serves — the
// verdict is the fallback's, and its usage bills to the fallback resource.
func TestJudgeFallbackChain(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		if req.Model == "judge-1" {
			return llm.ChatResponse{}, &llm.Error{Provider: "mock", Class: dag.ClassTransient, Message: "429"}
		}
		return answer(req.Model, 0.95, "fallback graded it"), nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","fallback_models":["mock/judge-2"],"rubric":"x","threshold":0.7}`
	verdict, err := v.Validate(context.Background(), judgeInput(config, "y"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !verdict.Passed() {
		t.Fatalf("fallback should have served a pass; got %+v", verdict)
	}
	if verdict.Results[0].Usage.Resource != "mock:judge-2" {
		t.Errorf("usage resource = %q, want mock:judge-2 (the served fallback)", verdict.Results[0].Usage.Resource)
	}
	if len(prov.reqs) != 2 {
		t.Errorf("provider calls = %d, want 2 (primary then fallback)", len(prov.reqs))
	}
}

// TestJudgeContextCancelPassesThrough: a caller-context cancellation is
// returned unwrapped (never a *validate.Error), so the engine keeps the
// timeout/cancelled judgment.
func TestJudgeContextCancelPassesThrough(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	prov := &funcProvider{fn: func(ctx context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
		<-ctx.Done()
		return llm.ChatResponse{}, ctx.Err()
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"x","threshold":0.7}`
	go cancel()
	_, err := v.Validate(ctx, judgeInput(config, "y"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled unwrapped", err)
	}
	var ve *validate.Error
	if errors.As(err, &ve) {
		t.Error("a context cancellation must not be wrapped as a *validate.Error")
	}
}

// TestJudgeRequestShape pins the request the judge sends: a system prompt, a
// user message carrying the rubric and the candidate output, a structured-
// output request, temperature 0, and the default max_tokens.
func TestJudgeRequestShape(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return answer(req.Model, 1, "ok"), nil
	}}
	v := validate.NewLLMJudge(judgeRegistry(t, prov))
	config := `{"model":"mock/judge-1","rubric":"MUST cite a source","threshold":0.7}`
	if _, err := v.Validate(context.Background(), judgeInput(config, "the candidate text")); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	req := prov.reqs[0]
	if req.System == "" {
		t.Error("judge request has no system prompt")
	}
	if req.ResponseFormat == nil {
		t.Error("judge request should ask for structured output")
	}
	if req.Temperature == nil || *req.Temperature != 0 {
		t.Errorf("judge temperature = %v, want 0", req.Temperature)
	}
	if req.MaxTokens != 512 {
		t.Errorf("judge max_tokens = %d, want default 512", req.MaxTokens)
	}
	user := req.Messages[len(req.Messages)-1].Blocks[0].Text
	if !strings.Contains(user, "MUST cite a source") || !strings.Contains(user, "the candidate text") {
		t.Errorf("user message missing rubric or output: %q", user)
	}
}

// TestJudgeConfigPreflight is the CompileConfig matrix: every content error a
// config schema cannot express is a permanent config error before any spend.
func TestJudgeConfigPreflight(t *testing.T) {
	t.Parallel()
	prov := &funcProvider{fn: func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return answer(req.Model, 1, "ok"), nil
	}}
	// A validator registry over the judge, so ValidateConfig runs the
	// pre-flight (CompileConfig) gate exactly as the engine's Resolve does.
	reg, err := validate.NewRegistry(validate.NewLLMJudge(judgeRegistry(t, prov)))
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}

	cases := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{"valid", `{"model":"mock/judge-1","rubric":"x","threshold":0.7}`, false},
		{"valid with fallback", `{"model":"mock/judge-1","fallback_models":["mock/judge-2"],"rubric":"x","threshold":0.5}`, false},
		{"missing model", `{"rubric":"x","threshold":0.7}`, true},
		{"blank rubric", `{"model":"mock/judge-1","rubric":"   ","threshold":0.7}`, true},
		{"threshold too high", `{"model":"mock/judge-1","rubric":"x","threshold":1.5}`, true},
		{"threshold negative", `{"model":"mock/judge-1","rubric":"x","threshold":-0.1}`, true},
		{"duplicate fallback", `{"model":"mock/judge-1","fallback_models":["mock/judge-1"],"rubric":"x","threshold":0.7}`, true},
		{"bad on_error", `{"model":"mock/judge-1","rubric":"x","threshold":0.7,"on_error":"warn"}`, true},
		{"bad timeout", `{"model":"mock/judge-1","rubric":"x","threshold":0.7,"timeout":"soon"}`, true},
		{"unroutable model", `{"model":"nope/whoknows","rubric":"x","threshold":0.7}`, true},
		{"unknown field", `{"model":"mock/judge-1","rubric":"x","threshold":0.7,"extra":true}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := reg.ValidateConfig(llmJudgeNameT, json.RawMessage(tc.config))
			if tc.wantErr && err == nil {
				t.Errorf("config %s: want a config error, got nil", tc.config)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("config %s: want ok, got %v", tc.config, err)
			}
		})
	}
}

// llmJudgeNameT is the validator name (unexported constant duplicated for the
// external test).
const llmJudgeNameT = "llm_judge"
