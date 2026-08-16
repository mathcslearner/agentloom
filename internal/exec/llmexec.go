package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/jsonrepair"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/tokens"
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
	// tokenReg selects the token counter for a model's blackboard writes
	// (ticket 12.2, TokenCounterProvider). Built internally so the many
	// NewLLMExecutor call sites need no new argument; the counters are pure
	// and offline, and the fallback-warning logger is left off here (the
	// engine owns the logged selection for its own paths).
	tokenReg *tokens.Registry
}

// NewLLMExecutor builds the llm executor over a provider registry. A nil
// or empty registry is valid — a worker that runs no llm steps boots
// keyless (config.LLMConfig leaves the registry empty when no provider
// key is set) — and any llm step then fails permanent at resolve time
// with a diagnosable ProviderUnavailable/UnknownModel error.
func NewLLMExecutor(providers *llm.Registry) LLMExecutor {
	return LLMExecutor{providers: providers, tokenReg: tokens.NewRegistry(nil)}
}

// llmConfigView decodes a claimed step's config into the effective LLMConfig
// the whole request-building pipeline runs on. For an llm step it is the
// LLMConfig verbatim; for a planner step (ticket 13.3, ADR-015) the
// PlannerConfig is projected onto an LLMConfig — the same model-call fields
// plus the implicit plan output_format — so the planner reuses every llm hook
// (routing, request framing, caching, limiting, cost, feedback, context,
// window guard) with no duplicated pipeline. The planner's per-expansion cap
// (max_added_steps) is engine state, not a model-call input, so it is dropped
// here. A nil config (no config key) decodes to nil, as for llm.
func llmConfigView(sc StepContext) (*dag.LLMConfig, error) {
	if sc.StepType == dag.StepPlanner {
		c, err := configAs[*dag.PlannerConfig](sc)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, nil
		}
		return &dag.LLMConfig{
			Model:        c.Model,
			Prompt:       c.Prompt,
			Messages:     c.Messages,
			MaxTokens:    c.MaxTokens,
			Temperature:  c.Temperature,
			OutputFormat: plannerOutputFormat(),
		}, nil
	}
	return configAs[*dag.LLMConfig](sc)
}

// plannerOutputFormat is the implicit structured-output policy every planner
// step's model call carries (ticket 13.3): a plain-JSON native request with a
// deterministic repair pass, so the completion is shaped into output.json (the
// PlanOutput the engine decodes and applies). It is deliberately schema-less
// at the provider layer — the full PlanOutput schema is enforced engine-side
// by the implicit validator, keeping the provider request provider-independent
// (OpenAI strict mode rejects the schema's $ref/$defs, and Anthropic's forced
// tool cannot be validated offline). A fresh pointer per call keeps the shared
// package state immutable.
func plannerOutputFormat() *dag.OutputFormat {
	return &dag.OutputFormat{Type: dag.OutputFormatJSON, Mode: dag.OutputFormatAuto}
}

// executorVersion is the plugin version for an llm-family executor's cache-key
// identity: planner and llm are distinct plugins (different Name), so their
// entries never collide even at the same version string.
func executorVersion(st dag.StepType) string {
	if st == dag.StepPlanner {
		return plannerVersion
	}
	return llmVersion
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
	cfg, err := llmConfigView(sc)
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
	out, err := marshalLLMOutput(resp, cfg.OutputFormat, provider.Manifest().Name+":"+served)
	if err != nil {
		return Output{}, err
	}
	logAttrs := []any{
		slog.String("model", resp.Model),
		slog.String("stop_reason", resp.StopReason),
		slog.Int64("input_tokens", resp.Usage.InputTokens),
		slog.Int64("output_tokens", resp.Usage.OutputTokens),
	}
	if out.Repair != nil {
		logAttrs = append(logAttrs, slog.String("output_provenance", out.Repair.Status))
	}
	sc.logger().InfoContext(ctx, "llm step completed", logAttrs...)
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
	cfg, err := llmConfigView(sc)
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
	est := e.estimateInputTokens(provider.Manifest().Name, model, cfg) + estimateLLMMaxTokens(cfg)
	if est < 1 {
		est = 1 // the token bucket requires a positive cost
	}
	return provider.Manifest().Name + ":" + model, est, nil
}

// TokenCounter implements TokenCounterProvider (ticket 12.2, ADR-014): the
// counter for this step's blackboard writes is the one for its resolved
// (provider, model), so an entry an llm step publishes is counted with the
// same tokenizer M12.3 assembly and M12.6 guardrails will use. A resolution
// or config failure returns an error, telling the engine to fall back to the
// chars/4 counter and let Execute land the classified failure.
func (e LLMExecutor) TokenCounter(sc StepContext) (tokens.Counter, error) {
	cfg, err := llmConfigView(sc)
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
	counter, _ := e.tokenReg.Select(provider.Manifest().Name, model)
	return counter, nil
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
	cfg, err := llmConfigView(sc)
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
	// The structured-output policy re-keys the entry (ticket 11.3): the same
	// request yields different output under a different format. Marshaled from
	// the authored block so the key covers type, schema, and mode.
	var outputFormat json.RawMessage
	if cfg.OutputFormat != nil {
		of, merr := json.Marshal(cfg.OutputFormat)
		if merr != nil {
			return nil, fmt.Errorf("marshaling output_format: %w", merr)
		}
		outputFormat = of
	}
	return &CacheBinding{
		Executor:      cache.PluginRef{Kind: plugin.KindExecutor, Name: string(sc.StepType), Version: executorVersion(sc.StepType)},
		Plugin:        cache.PluginRef{Kind: m.Kind, Name: m.Name, Version: m.Version},
		Caps:          m.Capabilities,
		Deterministic: deterministic,
		Request: cache.LLMRequest{
			Model:        model,
			Temperature:  cfg.Temperature,
			MaxTokens:    req.MaxTokens,
			Messages:     msgs,
			OutputFormat: outputFormat,
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
	cfg, err := llmConfigView(sc)
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
		InputTokens: e.estimateInputTokens(provider.Manifest().Name, model, cfg),
		MaxTokens:   estimateLLMMaxTokens(cfg),
	}, nil
}

// ModelFallbacks implements ModelDowngrader (ADR-012, ticket 10.4): the
// step's current (primary) model and its authored downgrade chain, projected
// onto the exec-level ModelFallback type so the engine needs no dag import.
// A config-decode miss routes like the other binding hooks: skip the
// downgrade, let Execute land the classified failure.
func (e LLMExecutor) ModelFallbacks(sc StepContext) (string, []ModelFallback, error) {
	cfg, err := llmConfigView(sc)
	if err != nil {
		return "", nil, err
	}
	if cfg == nil || cfg.Model == "" {
		return "", nil, fmt.Errorf("missing required field %q", "model")
	}
	if len(cfg.ModelFallbacks) == 0 {
		return cfg.Model, nil, nil
	}
	chain := make([]ModelFallback, len(cfg.ModelFallbacks))
	for i, f := range cfg.ModelFallbacks {
		chain[i] = ModelFallback{Model: f.Model, AtBudgetFraction: f.AtBudgetFraction}
	}
	return cfg.Model, chain, nil
}

// WithModel implements ModelDowngrader: it rewrites only the rendered
// config's "model" field, leaving every other value byte-identical (a
// raw-message re-key, not a struct round-trip), so the re-targeted config
// the engine feeds back through the pipeline differs from the original by
// exactly the model — which is what makes the fallback's cache key and
// resource bucket differ while nothing else does.
func (e LLMExecutor) WithModel(sc StepContext, model string) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(sc.Config) > 0 {
		if err := json.Unmarshal(sc.Config, &fields); err != nil {
			return nil, fmt.Errorf("decoding llm config for downgrade: %w", err)
		}
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("encoding downgrade model: %w", err)
	}
	fields["model"] = m
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("re-encoding llm config for downgrade: %w", err)
	}
	return out, nil
}

// WithFeedback implements FeedbackInjector (ADR-013, ticket 11.4): it folds a
// semantic-retry critique into the rendered config's request, leaving every
// other value byte-identical (a raw-message re-key, not a struct round-trip,
// exactly like WithModel). A prompt-form config gets the critique appended to
// its prompt with a blank-line separator; a messages-form config gets a
// trailing user message carrying the critique. Empty feedback text (a step
// with no critique to inject) returns the config unchanged. Because the
// prompt/messages feed the cache key, the resource estimate, and the provider
// call, the one rewrite re-targets the whole pipeline — so a semantic retry is
// a different cache key by construction.
func (e LLMExecutor) WithFeedback(sc StepContext, fb Feedback) (json.RawMessage, error) {
	if strings.TrimSpace(fb.Text) == "" {
		return sc.Config, nil
	}
	fields := map[string]json.RawMessage{}
	if len(sc.Config) > 0 {
		if err := json.Unmarshal(sc.Config, &fields); err != nil {
			return nil, fmt.Errorf("decoding llm config for feedback: %w", err)
		}
	}
	// messages form wins when present (a config carries exactly one of prompt
	// or messages — validated at submit time).
	if raw, ok := fields["messages"]; ok && len(raw) > 0 {
		var msgs []dag.LLMMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("decoding llm messages for feedback: %w", err)
		}
		msgs = append(msgs, dag.LLMMessage{Role: "user", Content: fb.Text})
		encoded, err := json.Marshal(msgs)
		if err != nil {
			return nil, fmt.Errorf("re-encoding llm messages for feedback: %w", err)
		}
		fields["messages"] = encoded
	} else {
		var prompt string
		if raw, ok := fields["prompt"]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &prompt); err != nil {
				return nil, fmt.Errorf("decoding llm prompt for feedback: %w", err)
			}
		}
		if prompt != "" {
			prompt += "\n\n" + fb.Text
		} else {
			prompt = fb.Text
		}
		encoded, err := json.Marshal(prompt)
		if err != nil {
			return nil, fmt.Errorf("re-encoding llm prompt for feedback: %w", err)
		}
		fields["prompt"] = encoded
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("re-encoding llm config for feedback: %w", err)
	}
	return out, nil
}

// WithContext implements ContextInjector (ADR-014, ticket 12.3): it prepends
// an assembled context preamble to the rendered config's request, leaving
// every other value byte-identical (a raw-message re-key, the WithFeedback
// mirror). A messages-form config gets a leading user message carrying the
// preamble; a prompt-form config gets the preamble prefixed to its prompt with
// a blank-line separator. An empty preamble (every source skipped) returns the
// config unchanged. Because the prompt/messages feed the cache key, the
// resource estimate, and the provider call, the one rewrite re-targets the
// whole pipeline, so the assembled context is a cache-key input by
// construction.
func (e LLMExecutor) WithContext(sc StepContext, preamble string) (json.RawMessage, error) {
	if preamble == "" {
		return sc.Config, nil
	}
	fields := map[string]json.RawMessage{}
	if len(sc.Config) > 0 {
		if err := json.Unmarshal(sc.Config, &fields); err != nil {
			return nil, fmt.Errorf("decoding llm config for context: %w", err)
		}
	}
	// messages form wins when present (a config carries exactly one of prompt
	// or messages — validated at submit time). The preamble becomes a LEADING
	// user message (context precedes the task), the mirror of feedback's
	// trailing message.
	if raw, ok := fields["messages"]; ok && len(raw) > 0 {
		var msgs []dag.LLMMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("decoding llm messages for context: %w", err)
		}
		msgs = append([]dag.LLMMessage{{Role: "user", Content: preamble}}, msgs...)
		encoded, err := json.Marshal(msgs)
		if err != nil {
			return nil, fmt.Errorf("re-encoding llm messages for context: %w", err)
		}
		fields["messages"] = encoded
	} else {
		var prompt string
		if raw, ok := fields["prompt"]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &prompt); err != nil {
				return nil, fmt.Errorf("decoding llm prompt for context: %w", err)
			}
		}
		if prompt != "" {
			prompt = preamble + "\n\n" + prompt
		} else {
			prompt = preamble
		}
		encoded, err := json.Marshal(prompt)
		if err != nil {
			return nil, fmt.Errorf("re-encoding llm prompt for context: %w", err)
		}
		fields["prompt"] = encoded
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("re-encoding llm config for context: %w", err)
	}
	return out, nil
}

// PreflightTokens implements PreflightCounter (ADR-014): it builds the request
// this step would send from its (already assembled and rendered) config and
// returns the counter's count of the whole framed request — the pre-flight
// total the context_assembled audit records and the 12.6 window guardrail
// checks. Model routing is not needed: the counter frames the request's
// content, not its model id.
func (e LLMExecutor) PreflightTokens(sc StepContext, counter tokens.Counter) (int, error) {
	cfg, err := llmConfigView(sc)
	if err != nil {
		return 0, err
	}
	if cfg == nil {
		return 0, fmt.Errorf("missing llm config")
	}
	req, err := buildChatRequest(cfg)
	if err != nil {
		return 0, fmt.Errorf("building chat request: %w", err)
	}
	return counter.CountRequest(req), nil
}

// estimateInputTokens is the pre-call input-token estimate for the resolved
// (provider, model): the real token counter's count of the whole framed
// request (ticket 12.6), refining M9.2's chars/4 heuristic. The counter frames
// system/messages/tool blocks exactly as the provider bills them, so the
// limiter's token debit and the budget's input-token estimate track real usage
// (on the mock and OpenAI, exactly; on Anthropic, the calibrated estimate). A
// request-build failure (corrupt config) falls back to the chars/4 estimate —
// the middleware then skips the check anyway once Execute lands the failure.
func (e LLMExecutor) estimateInputTokens(provider, model string, cfg *dag.LLMConfig) int64 {
	req, err := buildChatRequest(cfg)
	if err != nil {
		return estimateLLMInputTokensChars(cfg)
	}
	// Select always returns a usable counter (the chars/4 fallback family when
	// the provider/model is unknown), so no nil check is needed.
	counter, _ := e.tokenReg.Select(provider, model)
	return int64(counter.CountRequest(req))
}

// estimateLLMInputTokensChars is the legacy chars/4 input-token estimate,
// retained only as a fallback when the request cannot be built for the real
// counter (a corrupt config the caller will fail anyway).
func estimateLLMInputTokensChars(cfg *dag.LLMConfig) int64 {
	inputChars := len(cfg.Prompt)
	for _, m := range cfg.Messages {
		inputChars += len(m.Content)
	}
	if cfg.OutputFormat != nil {
		inputChars += len(cfg.OutputFormat.Schema)
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
		Messages:       msgs,
		MaxTokens:      maxTokens,
		Temperature:    cfg.Temperature,
		ResponseFormat: responseFormatFor(cfg.OutputFormat),
	}, nil
}

// responseFormatFor maps the step's authored output_format onto the provider
// request's ResponseFormat (ticket 11.3). Returns nil for a plain-text step
// or a repair_only format (repair_only never changes the request — only the
// post-call processing), so the provider request is byte-identical to today's
// unless native structured output is requested.
func responseFormatFor(of *dag.OutputFormat) *llm.ResponseFormat {
	if of == nil || of.Mode == dag.OutputFormatRepairOnly {
		return nil
	}
	rf := &llm.ResponseFormat{Name: "structured_output"}
	if of.Type == dag.OutputFormatJSONSchema {
		rf.Schema = of.Schema
	}
	return rf
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
// a downstream text reference never misses; JSON appears only for a
// structured-output step whose output parsed (native or repaired), so
// `${{ steps.<id>.output.json.field }}` flows structured data downstream;
// ToolCalls appears only when the model called tools.
type llmOutput struct {
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Text       string          `json:"text"`
	JSON       json.RawMessage `json:"json,omitempty"`
	ToolCalls  []llmToolCall   `json:"tool_calls,omitempty"`
	Usage      Usage           `json:"usage"`
}

// marshalLLMOutput renders the provider response into the persisted output
// and lifts usage onto the Output for the attempt row. When the step
// declared an output_format (ticket 11.3), the completion is shaped into
// structured JSON — natively when the provider supplied it, otherwise through
// a deterministic JSON-repair pass over the text — and the shaping is
// recorded as Output.Repair provenance. resource is the ADR-012 cost key
// (provider:served-model) carried onto Output.Resource for M10's ledger.
func marshalLLMOutput(resp llm.ChatResponse, of *dag.OutputFormat, resource string) (Output, error) {
	var calls []llmToolCall
	for _, tu := range resp.ToolUses() {
		calls = append(calls, llmToolCall{ID: tu.ID, Name: tu.Name, Input: tu.Input})
	}
	usage := Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	out := llmOutput{
		Model:      resp.Model,
		StopReason: resp.StopReason,
		Text:       resp.Text(),
		ToolCalls:  calls,
		Usage:      usage,
	}
	var provenance *Repair
	if of != nil {
		out.Text, out.JSON, provenance = shapeStructured(resp)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return Output{}, fmt.Errorf("marshaling llm output: %w", err)
	}
	return Output{Data: data, Usage: &usage, Resource: resource, Repair: provenance}, nil
}

// shapeStructured resolves a structured-output completion (ticket 11.3) into
// the persisted (text, json) pair and its repair provenance:
//
//   - the provider emitted native structured JSON → status "native", text is
//     the compact JSON, json is that value;
//   - otherwise the completion text is run through the deterministic repair
//     pass → status raw/repaired (text becomes the canonical JSON, json is
//     the value) or unrepairable (text is left as the raw model output, json
//     is absent so the implicit json_schema validator raises invalid_json).
//
// The default target for the engine's validate stage is /text, so overwriting
// text with the canonical JSON makes both the implicit validator and any
// author-supplied /text validator see the repaired structure.
func shapeStructured(resp llm.ChatResponse) (text string, structured json.RawMessage, prov *Repair) {
	if len(resp.Structured) > 0 {
		canonical := compactJSON(resp.Structured)
		return string(canonical), canonical, &Repair{SchemaVersion: 1, Status: "native"}
	}
	raw := resp.Text()
	r := jsonrepair.Repair(raw)
	switch r.Status {
	case jsonrepair.StatusRaw:
		return string(r.Value), r.Value, &Repair{SchemaVersion: 1, Status: string(r.Status)}
	case jsonrepair.StatusRepaired:
		return string(r.Value), r.Value, &Repair{SchemaVersion: 1, Status: string(r.Status), Steps: r.Steps, RawText: raw}
	default:
		// Unrepairable: keep the raw text so the implicit validator reports a
		// structural issue over the actual model output (better feedback for
		// the semantic retry, 11.4) and downstream `.json` misses.
		return raw, nil, &Repair{SchemaVersion: 1, Status: string(r.Status), Steps: r.Steps}
	}
}

// compactJSON returns the compact form of raw JSON, or raw unchanged if it is
// not compactable (already validated upstream in practice).
func compactJSON(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return json.RawMessage(buf.Bytes())
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
