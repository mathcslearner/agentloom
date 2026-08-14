package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// llmVersion is the production llm executor's plugin version (ADR-009).
// It replaces the dev stub's 0.1.0-stub in place; the bump to 1.0.0 is
// the behavior change M9's response cache keys on — a run that executed
// against the stub must not read a cache entry written by the real one.
const llmVersion = "1.0.0"

// llmDefaultMaxTokens bounds a completion when the step config omits
// max_tokens. The provider interface requires a positive bound
// (llm.ChatRequest.Validate), and a mandatory bound keeps M10 budgets
// estimable before the call; validation makes max_tokens optional, so
// the executor supplies this default when it is absent.
const llmDefaultMaxTokens = 1024

// LLMExecutor runs llm steps (ticket 8.6): it renders the step config
// (already template-resolved by 8.2) into a unified ChatRequest, routes
// the model to a provider through the llm.Registry, makes exactly one
// Chat call, and persists the completion (text and/or tool-call payload)
// plus the provider's token usage. It never retries — the M5 engine owns
// retry, informed by the ADR-006 class this executor maps provider
// errors onto.
type LLMExecutor struct {
	providers *llm.Registry
}

// NewLLMExecutor builds the llm executor over a provider registry. A nil
// or empty registry is valid — a worker that runs no llm steps boots
// keyless (config.LLMConfig leaves the registry empty when no provider
// key is set) — and any llm step then fails permanent at resolve time
// with a diagnosable ProviderUnavailable/UnknownModel error.
func NewLLMExecutor(providers *llm.Registry) LLMExecutor {
	return LLMExecutor{providers: providers}
}

// Type implements Executor.
func (LLMExecutor) Type() string { return string(dag.StepLLM) }

// PluginManifest implements SelfDescribing (ADR-009). Cacheable and
// cost-bearing: non-deterministic completions are exactly what M9's
// response cache reuses (keyed on temperature==0 by default), and
// provider calls are what M10 meters.
func (LLMExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepLLM, llmVersion,
		"One model call through the provider interface (Anthropic, OpenAI, or the mock).",
		plugin.Capabilities{Cacheable: true, CostBearing: true})
}

// Execute runs one llm attempt. Provider failures surface classified via
// the M5 retry engine: an *llm.Error's declared class (transient /
// permanent) is honored; routing failures (unknown model, unconfigured
// provider) are permanent (deterministic — no retry can conjure a
// provider); context cancellation and deadline expiry pass through
// unclassified so the engine keeps the timeout/cancelled judgment
// (ADR-006 rows 3/8).
func (e LLMExecutor) Execute(ctx context.Context, sc StepContext) (Output, error) {
	cfg, err := configAs[*dag.LLMConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if cfg == nil || cfg.Model == "" {
		// Config was validated at submit time (1.3 requires model); hitting
		// this means corrupt stored state or a version skew — permanent,
		// like any other config-decode miss.
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "model")}
	}

	req, err := buildChatRequest(cfg)
	if err != nil {
		return Output{}, Permanentf("building chat request: %w", err)
	}

	if e.providers == nil {
		// A keyless worker (no provider key, mock disabled) has no routable
		// providers — deterministic, so a permanent failure, not a retry.
		return Output{}, Permanentf("no model providers configured for model %q", cfg.Model)
	}
	provider, model, err := e.providers.Resolve("", cfg.Model)
	if err != nil {
		// UnknownModelError / ProviderUnavailableError are both
		// deterministic functions of stored config and worker
		// configuration (ADR-009): no identical retry can succeed.
		return Output{}, Permanentf("resolving model %q: %w", cfg.Model, err)
	}
	req.Model = model

	resp, err := provider.Chat(ctx, req)
	if err != nil {
		return Output{}, classifyProviderError(err)
	}

	out, err := marshalLLMOutput(resp)
	if err != nil {
		return Output{}, err
	}
	sc.logger().InfoContext(ctx, "llm step completed",
		slog.String("model", resp.Model),
		slog.String("stop_reason", resp.StopReason),
		slog.Int64("input_tokens", resp.Usage.InputTokens),
		slog.Int64("output_tokens", resp.Usage.OutputTokens))
	return out, nil
}

// buildChatRequest maps the (already template-rendered) step config onto
// the unified ChatRequest. Model is filled in by the caller after
// routing so the provider sees its canonical (prefix-stripped) model id.
func buildChatRequest(cfg *dag.LLMConfig) (llm.ChatRequest, error) {
	msgs, err := buildMessages(cfg)
	if err != nil {
		return llm.ChatRequest{}, err
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = llmDefaultMaxTokens
	}
	return llm.ChatRequest{
		Messages:    msgs,
		MaxTokens:   maxTokens,
		Temperature: cfg.Temperature,
	}, nil
}

// buildMessages turns the config's prompt XOR messages into the unified
// message list. Validation (1.3) guarantees exactly one is present, but
// the executor tolerates a hand-built config: a bare prompt becomes one
// user message; a messages array maps each entry's role onto the two
// conversational roles.
func buildMessages(cfg *dag.LLMConfig) ([]llm.Message, error) {
	if cfg.Prompt != "" {
		return []llm.Message{llm.UserText(cfg.Prompt)}, nil
	}
	if len(cfg.Messages) == 0 {
		return nil, fmt.Errorf("requires one of %q or %q", "prompt", "messages")
	}
	msgs := make([]llm.Message, 0, len(cfg.Messages))
	for i, m := range cfg.Messages {
		role := llm.Role(m.Role)
		if !role.Valid() {
			return nil, fmt.Errorf("messages[%d]: role %q is not %q or %q", i, m.Role, llm.RoleUser, llm.RoleAssistant)
		}
		if m.Content == "" {
			return nil, fmt.Errorf("messages[%d]: content is empty", i)
		}
		msgs = append(msgs, llm.Message{Role: role, Blocks: []llm.Block{llm.TextBlock(m.Content)}})
	}
	return msgs, nil
}

// llmToolCall is one model-issued tool call in the persisted output.
type llmToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// llmOutput is the llm step's persisted output shape, read by downstream
// steps through templating (`${{ steps.<id>.output.text }}`) and CEL. Text
// is always present (empty when the completion carried no text blocks) so
// a downstream text reference never misses; ToolCalls appears only when
// the model called tools.
type llmOutput struct {
	Model      string        `json:"model"`
	StopReason string        `json:"stop_reason"`
	Text       string        `json:"text"`
	ToolCalls  []llmToolCall `json:"tool_calls,omitempty"`
	Usage      Usage         `json:"usage"`
}

// marshalLLMOutput renders the provider response into the persisted
// output and lifts usage onto the Output for the attempt row.
func marshalLLMOutput(resp llm.ChatResponse) (Output, error) {
	var calls []llmToolCall
	for _, tu := range resp.ToolUses() {
		calls = append(calls, llmToolCall{ID: tu.ID, Name: tu.Name, Input: tu.Input})
	}
	usage := Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	data, err := json.Marshal(llmOutput{
		Model:      resp.Model,
		StopReason: resp.StopReason,
		Text:       resp.Text(),
		ToolCalls:  calls,
		Usage:      usage,
	})
	if err != nil {
		return Output{}, fmt.Errorf("marshaling llm output: %w", err)
	}
	return Output{Data: data, Usage: &usage}, nil
}

// classifyProviderError maps a provider failure onto the executor's
// classified-error contract. An *llm.Error carries its ADR-006 class
// (providers only ever classify transient/permanent), which the engine's
// classifyFailure then honors as declared. A context error is returned
// unwrapped so the engine judges timeout vs. cancelled from context
// state (ADR-006 rows 3/8) — never wrapped in a ClassifiedError.
func classifyProviderError(err error) error {
	var pe *llm.Error
	if errors.As(err, &pe) {
		return &ClassifiedError{Class: pe.Class, Err: err}
	}
	// Context cancellation/deadline (or any other pass-through) keeps its
	// identity so errors.Is against context.Canceled/DeadlineExceeded still
	// holds in the engine.
	return err
}
