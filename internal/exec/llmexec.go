package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/cache"
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

	// The cost/rate-limit resource name (ADR-010/ADR-012): keyed by the
	// resolved provider and the model that actually SERVED the call
	// (resp.Model — a dated variant a step never named prices via the
	// provider wildcard, and 10.4's downgrade prices the actual model). Empty
	// resp.Model falls back to the routed model so the row is never keyed on
	// a bare provider prefix.
	served := resp.Model
	if served == "" {
		served = model
	}
	out, err := marshalLLMOutput(resp, provider.Manifest().Name+":"+served)
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

// ResourceClaim implements ResourceClaimer (ADR-010): the llm step's
// fleet-wide resource is "<provider>:<model>", keyed by the *resolved*
// provider so the limiter meters what the provider actually bills, and its
// token cost is a rough pre-call estimate. A resolution failure (unknown
// model, unconfigured provider) or a corrupt config returns an error, which
// tells the middleware to skip limiting and let Execute land the permanent
// failure — the routing judgment lives in one place.
func (e LLMExecutor) ResourceClaim(sc StepContext) (string, int64, error) {
	cfg, err := configAs[*dag.LLMConfig](sc)
	if err != nil {
		return "", 0, err
	}
	if cfg == nil || cfg.Model == "" {
		return "", 0, fmt.Errorf("missing required field %q", "model")
	}
	if e.providers == nil {
		return "", 0, fmt.Errorf("no model providers configured for model %q", cfg.Model)
	}
	provider, model, err := e.providers.Resolve("", cfg.Model)
	if err != nil {
		return "", 0, fmt.Errorf("resolving model %q: %w", cfg.Model, err)
	}
	return provider.Manifest().Name + ":" + model, estimateLLMTokens(cfg), nil
}

// CacheBinding implements CacheBinder (ADR-011, ticket 9.5): the llm step's
// cache key is built over the resolved provider identity and the resolved
// ChatRequest (model, sampling params, rendered messages). The concrete
// plugin is the model provider, so its manifest supplies the eligibility
// flags; the determinism signal is temperature==0 (an explicit deterministic
// 0, distinct from nil — the provider default is non-deterministic). A
// resolution or config failure returns an error so the middleware skips
// caching and lets Execute land the permanent failure — the same convention
// ResourceClaim uses.
func (e LLMExecutor) CacheBinding(sc StepContext) (*CacheBinding, error) {
	cfg, err := configAs[*dag.LLMConfig](sc)
	if err != nil {
		return nil, err
	}
	if cfg == nil || cfg.Model == "" {
		return nil, fmt.Errorf("missing required field %q", "model")
	}
	if e.providers == nil {
		return nil, fmt.Errorf("no model providers configured for model %q", cfg.Model)
	}
	provider, model, err := e.providers.Resolve("", cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("resolving model %q: %w", cfg.Model, err)
	}
	req, err := buildChatRequest(cfg)
	if err != nil {
		return nil, fmt.Errorf("building chat request: %w", err)
	}
	// The rendered conversation, canonicalized into the key by the cache
	// package's round-trip; a marshal failure is deterministic (corrupt
	// config), so it routes like any other binding failure.
	msgs, err := json.Marshal(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("marshaling messages: %w", err)
	}
	m := provider.Manifest()
	deterministic := cfg.Temperature != nil && *cfg.Temperature == 0
	return &CacheBinding{
		Executor:      cache.PluginRef{Kind: plugin.KindExecutor, Name: string(dag.StepLLM), Version: llmVersion},
		Plugin:        cache.PluginRef{Kind: m.Kind, Name: m.Name, Version: m.Version},
		Caps:          m.Capabilities,
		Deterministic: deterministic,
		Request: cache.LLMRequest{
			Model:       model,
			Temperature: cfg.Temperature,
			MaxTokens:   req.MaxTokens,
			Messages:    msgs,
		},
	}, nil
}

// CostEstimate implements CostEstimator (ADR-012): the llm step's pricing
// resource is "<provider>:<model>" by the *resolved* provider (the pre-flight
// routed model — the ledger later keys the served model), and the pre-call
// token split feeds the budget middleware's upper-bound estimate (input at
// the model's input rate, max_tokens at its output rate). Resolution or
// config failures route like ResourceClaim's: skip the check, let Execute
// land the permanent failure.
func (e LLMExecutor) CostEstimate(sc StepContext) (CostEstimate, error) {
	cfg, err := configAs[*dag.LLMConfig](sc)
	if err != nil {
		return CostEstimate{}, err
	}
	if cfg == nil || cfg.Model == "" {
		return CostEstimate{}, fmt.Errorf("missing required field %q", "model")
	}
	if e.providers == nil {
		return CostEstimate{}, fmt.Errorf("no model providers configured for model %q", cfg.Model)
	}
	provider, model, err := e.providers.Resolve("", cfg.Model)
	if err != nil {
		return CostEstimate{}, fmt.Errorf("resolving model %q: %w", cfg.Model, err)
	}
	return CostEstimate{
		Resource:    provider.Manifest().Name + ":" + model,
		InputTokens: estimateLLMInputTokens(cfg),
		MaxTokens:   estimateLLMMaxTokens(cfg),
	}, nil
}

// estimateLLMInputTokens is the pre-call input-token estimate: roughly four
// characters per token over the rendered prompt and messages.
func estimateLLMInputTokens(cfg *dag.LLMConfig) int64 {
	inputChars := len(cfg.Prompt)
	for _, m := range cfg.Messages {
		inputChars += len(m.Content)
	}
	return int64((inputChars + 3) / 4)
}

// estimateLLMMaxTokens is the completion's max_tokens bound (defaulted when
// absent) — the output-token ceiling the pre-call estimate prices.
func estimateLLMMaxTokens(cfg *dag.LLMConfig) int64 {
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = llmDefaultMaxTokens
	}
	return int64(maxTokens)
}

// estimateLLMTokens is the ADR-010 pre-call token estimate for an llm step:
// roughly four characters per input token over the rendered prompt/messages,
// plus the completion's max_tokens bound (defaulted when absent). It is
// deliberately rough — M12's real token counters refine it, and 9.3
// reconciles the post-call actual−estimate error back onto the bucket so a
// biased estimator cannot drift the fleet past the provider's real budget.
// Never below 1 (the token bucket requires a positive cost).
func estimateLLMTokens(cfg *dag.LLMConfig) int64 {
	est := estimateLLMInputTokens(cfg) + estimateLLMMaxTokens(cfg)
	if est < 1 {
		est = 1
	}
	return est
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

// marshalLLMOutput renders the provider response into the persisted output
// and lifts usage onto the Output for the attempt row. resource is the
// ADR-012 cost key (provider:served-model) carried onto Output.Resource for
// M10's ledger.
func marshalLLMOutput(resp llm.ChatResponse, resource string) (Output, error) {
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
	return Output{Data: data, Usage: &usage, Resource: resource}, nil
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
