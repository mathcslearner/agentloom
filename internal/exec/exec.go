// Package exec defines the executor SPI v0 (ticket 4.1): the Executor
// interface the worker's execute pipeline (4.3) invokes for each claimed
// step, the Registry mapping step types to executors, and the five test
// executors M4/M5's fixtures and chaos suites are built from (noop, echo,
// sleep, fail_n_times, and — since 4.7 — counter).
//
// Since ticket 8.1 the executors are plugins (ADR-009): the Registry is
// a typed facade over internal/plugin's generic registry (kind executor,
// name = step type), production executors self-describe via
// SelfDescribing — version, capability flags, a config schema generated
// from the dag package's registered config structs — and Manifests()
// feeds GET /v1/plugins. The execution surface is unchanged: Output is a
// struct so success payloads can gain fields (usage, artifacts), and
// executors decode their own config from raw JSON via the dag package's
// registered config types. The middleware chain arrives in M9.
package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

// Output is what a successful execution produces. Data is the step's
// output payload, stored on the run_steps row and exposed to downstream
// CEL predicates and templates as `output`; nil means the step has no
// output (persisted as JSON null).
type Output struct {
	Data json.RawMessage
	// Usage is the attempt's token accounting, set by executors that call
	// a metered provider (the llm executor, ticket 8.6); nil for every
	// other step type. The engine persists it on the attempt row for
	// M10's cost ledger — output payloads carry it too, but the attempt
	// row is the durable per-try record a retry re-creates.
	Usage *Usage
	// Resource is the ADR-010/ADR-012 resource name this attempt's external
	// call bills to — "<resolved-provider>:<served-model>" for an llm call
	// (the model that actually served it, keyed like the rate limiter), or
	// "tool:<name>" for a tool invocation. Empty means the attempt is not
	// cost-bearing (no provider/tool call, or a step type that never bills):
	// M10's cost ledger (ticket 10.2) prices only attempts that name one.
	// Set by the metered executors on success; the engine's cache middleware
	// carries it across a hit so a cache-served attempt still prices its
	// counterfactual "saved" figure.
	Resource string
	// Repair is the output provenance of a structured-output llm step
	// (ticket 11.3, ADR-013): whether the completion arrived as native
	// structured JSON, was already valid, was deterministically repaired, or
	// could not be repaired. Nil for every step without an output_format. The
	// engine persists it on the attempt row (step_attempts.repair) and the
	// cache middleware carries it across a hit.
	Repair *Repair
}

// Repair is the structured-output provenance recorded on an attempt (ticket
// 11.3, ADR-013). It mirrors Usage: a small, fixed JSON object the engine
// persists so the run-status API and 11.6's metrics can read how an output
// was shaped, without re-deriving it.
type Repair struct {
	// SchemaVersion is the record version (1).
	SchemaVersion int `json:"schema_version"`
	// Status is the provenance: "native" (the provider emitted structured
	// output directly), "raw" (the text was already valid JSON), "repaired"
	// (a deterministic pass fixed it), or "unrepairable" (no pass produced
	// valid JSON — the output is left to the semantic-retry loop, 11.4).
	Status string `json:"status"`
	// Steps names the repair passes that changed the text, in order (empty
	// for native/raw). Structure only — never instance values.
	Steps []string `json:"steps,omitempty"`
	// RawText is the model's pre-repair text, kept only when Status is
	// "repaired" so the repair is diffable (output provenance, 11.3). Empty
	// otherwise.
	RawText string `json:"raw_text,omitempty"`
}

// Usage is one attempt's token accounting, mirroring llm.Usage but kept
// in exec so the executor SPI does not force every caller to import the
// provider package. Executors that meter no tokens leave Output.Usage nil.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CacheHit marks this attempt's usage as counterfactual: the result was
	// served from the response cache (ticket 9.5, ADR-011), so the tokens
	// were NOT spent — they are the "would-have-cost" snapshot the original
	// miss recorded, kept so M10's cost ledger can meter the savings. Set
	// only by the engine's cache middleware on a hit; never by an executor
	// (a fresh execution's usage is real spend). Absent (false) on every
	// real attempt.
	CacheHit bool `json:"cache_hit,omitempty"`
}

// StepContext is everything an executor may see about the step it is
// running. The worker builds one per invocation from the claimed step's
// run_steps row and attempt state; executors must treat it as read-only.
type StepContext struct {
	// StepType is the claimed step's type, as stored on the run_steps row.
	// It selects the config shape Config decodes into.
	StepType dag.StepType

	// Config is the step's config as materialized into run_steps.config
	// (ADR-004) with template rendering applied (ticket 8.2): `${{ ... }}`
	// expressions in string values are resolved against upstream step
	// outputs and run params before the executor sees them. Nil when the
	// definition had no config key. Executors decode it with configAs —
	// decoding stays in the executor, so the worker never needs to know
	// config shapes.
	Config json.RawMessage

	// Input is reserved for a future merged-input payload (e.g. a join
	// fan-in merge). Ticket 8.2 renders templates in place inside Config
	// instead, so Input is currently always nil; executors must tolerate
	// that.
	Input json.RawMessage

	// Attempt is the 1-based attempt number from the claim's attempt row
	// (ADR-004: attempt_count increments at claim). It is the only
	// execution state an executor may branch on — anything in-process would
	// not survive a crash or reclaim.
	Attempt int

	// IdempotencyKey is a stable opaque token for this step's external
	// calls (ticket 5.5): derived deterministically from (run, step), so it
	// is identical across attempts, retries, reclaims, and zombie
	// takeovers, and distinct across steps and runs. Executors pass it to
	// external services that support idempotency keys (M8's http_request
	// sends it as an Idempotency-Key header on non-GET calls); combined
	// with an effect ID it also names entries in the side-effect journal.
	// Empty when the invoker predates 5.5 wiring (unit tests constructing
	// bare StepContexts).
	IdempotencyKey string

	// Effects is the side-effect journal handle bound to this step (ticket
	// 5.5), through which executors make external side effects
	// effectively-once. Nil when no journal is wired (bare unit-test
	// contexts); executors that require it must fail with a permanent
	// error rather than firing unjournaled effects.
	Effects EffectJournal

	// Blackboard is the run-scoped shared-memory handle bound to this step
	// (ticket 12.2, ADR-014): executors read and write versioned entries
	// keyed within the run, attributed to this step and fenced on its claim.
	// Nil when no board is wired (bare unit-test contexts); executors that
	// require it must fail with a permanent error rather than assuming it.
	Blackboard blackboard.Board

	// Logger carries the run/step/attempt log fields stamped by the
	// worker. May be nil (executors fall back to slog.Default()).
	Logger *slog.Logger
}

// EffectJournal is the executor-facing surface of the side-effect journal
// (ticket 5.5, implemented by internal/exec/effects and bound per step by
// the engine). Do wraps one external side effect in the journal protocol:
// record-intent → execute fn → record-result, each journal phase in its
// own short transaction, never holding one across fn. If the effect
// already has a journaled result — this step ran before, through a retry,
// reclaim, or zombie takeover — fn is skipped entirely and the stored
// result is returned: journaled results short-circuit re-execution.
//
// effectID names the effect within the step (one step may journal several
// distinct effects); the same effectID must mean the same effect on every
// attempt. fn runs under the executor's context, so step timeouts and
// cancellation apply. A dangling intent left by a crashed attempt is taken
// over and fn re-executes — the residual at-least-once window the
// idempotency key exists to absorb externally.
type EffectJournal interface {
	Do(ctx context.Context, effectID string, fn func(context.Context) (json.RawMessage, error)) (json.RawMessage, error)
}

// logger returns the context's logger, falling back to slog.Default() so
// executors never nil-check (same convention as obs/log.From).
func (sc StepContext) logger() *slog.Logger {
	if sc.Logger != nil {
		return sc.Logger
	}
	return slog.Default()
}

// Executor runs one step type. Implementations must be safe for
// concurrent Execute calls (one Executor instance serves every claimed
// step of its type across the worker's consumer goroutines) and must
// return promptly once ctx is canceled — the lease heartbeater stops with
// the handler, so overstaying invites a reclaim and a fenced-out write.
type Executor interface {
	// Type is the step type this executor runs, matching the dag package's
	// StepType spelling (e.g. "sleep").
	Type() string

	// Execute runs one attempt of the step. An error return means the
	// attempt failed (retry policy arrives in M5); ctx cancellation should
	// surface as ctx.Err().
	Execute(ctx context.Context, sc StepContext) (Output, error)
}

// ResourceClaimer is the optional executor hook the M9 limiter middleware
// consults before a cost-bearing executor's provider call (ADR-010): it
// names the shared external resource the call draws on and estimates the
// token cost. An executor that implements it (the llm executor; a
// rate-limited tool) is subject to fleet-wide rate limiting on that
// resource; one that does not (noop, echo, sleep, pure tools) bypasses the
// limiter entirely — the middleware is a no-op for steps that name no
// resource.
//
// resource is the ADR-010 name keyed by the *resolved* provider
// ("anthropic:<model>", "mock:<model>", "tool:<name>"), so the limiter keys
// off what the provider actually meters. estTokens is a pre-call estimate
// (the tokens bucket's cost); a requests-only resource returns 0. An error
// return means the binding could not be computed (e.g. an unresolvable
// model): the middleware then skips limiting and lets Execute run, so the
// executor lands the properly classified failure itself rather than the
// limiter duplicating that judgment.
type ResourceClaimer interface {
	ResourceClaim(sc StepContext) (resource string, estTokens int64, err error)
}

// CostEstimator is the optional executor hook the M10 budget middleware
// consults before a cost-bearing executor's provider call (ADR-012): it
// projects the pre-flight pricing inputs so the claim path can compare the
// run's projected spend (run spend + estimate) against the run budget and
// the step's caps before any money is spent. An executor that implements it
// (the llm and tool executors) is subject to budget enforcement; one that
// does not bypasses it — the middleware is a no-op for steps that estimate
// no cost.
//
// The estimate splits input and output tokens so the middleware prices each
// at the model's respective rate (ADR-012's upper-bound Estimate). Its error
// convention mirrors ResourceClaim: a resolution or config failure means the
// estimate could not be computed (an unresolvable model, a corrupt config),
// so the middleware skips the budget check and lets Execute land the
// classified failure — the routing judgment lives in one place. The
// fail-closed unknown-model policy is a separate decision the middleware
// makes from the returned resource, not signaled here.
type CostEstimator interface {
	CostEstimate(sc StepContext) (CostEstimate, error)
}

// CostEstimate is what a CostEstimator projects for the budget middleware:
// the pricing key and the pre-flight token split.
type CostEstimate struct {
	// Resource is the ADR-010/ADR-012 pricing key, by the *resolved* provider
	// ("mock:sim-1", "tool:paid_search"). Empty means the step is not
	// cost-bearing (the middleware then proceeds without a check).
	Resource string
	// InputTokens is the pre-call input-token estimate (roughly chars/4 over
	// the rendered request), priced at the model's input rate. Zero for a
	// flat-priced tool.
	InputTokens int64
	// MaxTokens is the output ceiling priced at the model's output rate — the
	// step's configured max_tokens (defaulted). Zero for a tool. This is also
	// the figure a step-level budget.max_tokens cap is checked against
	// (together with InputTokens), so an oversized request never runs.
	MaxTokens int64
}

// ModelDowngrader is the optional executor hook the M10.4 budget middleware
// consults to route a claim to a cheaper model as the run approaches its
// budget (ADR-012). Only a step with a runtime notion of "model" implements
// it (the llm executor); the middleware skips downgrade for any other step.
//
// ModelFallbacks reports the step's current (primary) model and its ordered
// downgrade chain. WithModel produces a rendered config identical to the
// current one except that the model is replaced with the chosen fallback —
// the middleware feeds that config back through the whole executor pipeline
// (cache key, resource limiter, cost estimate, and the provider call itself
// all derive from the model), so one rewrite re-targets every downstream
// consumer with no other change.
//
// Both methods' error convention mirrors ResourceClaim/CostEstimate: a
// resolution or config failure means the binding could not be computed, so
// the middleware skips downgrade and lets Execute land the classified
// failure — the routing judgment lives in one place.
type ModelDowngrader interface {
	// ModelFallbacks reports the step's current model and its downgrade
	// chain. An empty chain (the common case) means the step never
	// downgrades — the middleware then proceeds to the ordinary budget
	// routing (park/fail).
	ModelFallbacks(sc StepContext) (current string, chain []ModelFallback, err error)
	// WithModel returns sc.Config with its model field replaced by the given
	// model, leaving every other value byte-identical. The middleware calls
	// it to build the re-targeted config it feeds back into the pipeline.
	WithModel(sc StepContext, model string) (json.RawMessage, error)
}

// TokenCounterProvider is the optional executor hook the engine consults to
// pick the token counter for a step's blackboard writes (ticket 12.2,
// ADR-014). An executor with a runtime notion of "model" (the llm executor)
// resolves the model to its tokenizer so an entry it writes is counted with
// the same counter the M12.3 assembly and M12.6 window guardrail will use.
// An executor that does not implement it (every non-model step) gets the
// chars/4 fallback counter — the honest "no model tokenizer here" choice.
//
// An error return (an unresolvable model, a corrupt config) means the
// counter could not be computed; the engine falls back to the chars/4
// counter and lets Execute land the classified failure — the routing
// judgment lives in one place, exactly like the other hooks.
type TokenCounterProvider interface {
	TokenCounter(sc StepContext) (tokens.Counter, error)
}

// ModelFallback is one tier in a step's downgrade chain (ADR-012, ticket
// 10.4): the cheaper Model to route to and its optional soft trigger.
type ModelFallback struct {
	// Model is the fallback model id (routed through the same registry as
	// the primary).
	Model string
	// AtBudgetFraction is the soft trigger: once the run's spend reaches this
	// fraction of its budget, claims route to this tier (or a deeper one).
	// nil means this tier is reached only by a hard projection trigger.
	AtBudgetFraction *float64
}

// Feedback is the critique a semantic retry carries into its re-attempt
// (ADR-013, ticket 11.4): the rendered critique-prompt fragment plus the
// attempt bookkeeping. It rides durably on run_steps between attempts (the
// completion transaction stamps the *next* attempt's feedback, the claim
// copies it onto the attempt row) so the executor sees it in StepContext.
// SchemaVersion/PriorAttempt make the record self-describing for the
// status API; Text is what FeedbackInjector folds into the prompt.
type Feedback struct {
	// SchemaVersion is the record version (1).
	SchemaVersion int `json:"schema_version"`
	// SemanticAttempt is the 1-based semantic attempt this feedback is for —
	// 2 on the first re-attempt, so a validator (validate.Input.Attempt) and
	// the status API can see how deep the semantic loop has gone.
	SemanticAttempt int `json:"semantic_attempt"`
	// MaxAttempts is the step's semantic budget (validation.max_attempts).
	MaxAttempts int `json:"max_attempts"`
	// PriorAttempt is the durable attempt number whose rejected output
	// produced this critique — the diff anchor for "what changed".
	PriorAttempt int `json:"prior_attempt"`
	// Text is the rendered critique-prompt fragment (dag.RenderFeedback). Empty
	// for a step type whose executor injects no text — it still re-attempts,
	// but identically. Structure/critique only, never a secret.
	Text string `json:"text,omitempty"`
}

// FeedbackInjector is the optional executor hook the semantic-retry
// middleware consults to fold a critique into the next attempt's request
// (ADR-013, ticket 11.4). Only a step whose request has a natural prompt to
// extend implements it (the llm executor); any other step type re-attempts
// with its original rendered config (identical request under the semantic
// budget). WithFeedback returns sc.Config with the critique appended (the llm
// executor suffixes the prompt / adds a trailing user message), leaving every
// other value byte-identical — so the augmented request re-keys the response
// cache (a semantic retry is cache-bypassed by construction) and re-prices
// exactly as WithModel does for a downgrade. An error means the binding could
// not be computed; the middleware then proceeds with the un-augmented config
// rather than failing the attempt (the ResourceClaim/WithModel convention).
type FeedbackInjector interface {
	WithFeedback(sc StepContext, fb Feedback) (json.RawMessage, error)
}

// ContextInjector is the optional executor hook the M12 context-assembly
// middleware consults to prepend an assembled context preamble into a step's
// request before it runs (ADR-014, ticket 12.3). Only a step whose request has
// a natural place to prepend text implements it (the llm executor); a step
// type that cannot fails permanently when it carries a `context` spec (the
// engine records the miss). WithContext returns sc.Config with the preamble
// prepended (the llm executor prefixes the prompt / adds a leading user
// message), leaving every other value byte-identical — the raw-message re-key
// mirror of WithFeedback (which appends). Because the prompt/messages feed the
// cache key, the resource estimate, and the provider call, the one rewrite
// re-targets the whole pipeline, so the assembled context is a cache-key input
// by construction (ADR-011/014). An empty preamble returns the config
// unchanged. An error means the rewrite could not be computed (a corrupt
// config); the middleware then records a permanent step failure.
type ContextInjector interface {
	WithContext(sc StepContext, preamble string) (json.RawMessage, error)
}

// PreflightCounter is the optional executor hook the context-assembly and
// (M12.6) provider-window middleware use to count the fully-framed request a
// step will send, using the step's resolved token counter (ADR-014). Only a
// step that issues a provider request implements it (the llm executor). The
// returned count is the whole request's token total — system prompt, message
// framing, tool definitions, structured-output schema — the number the window
// guardrail compares against the model context window and the assembly audit
// records as the pre-flight total. An error means the request could not be
// built (a corrupt config); the middleware treats the count as unavailable
// rather than failing the step (Execute will land any real config failure).
type PreflightCounter interface {
	PreflightTokens(sc StepContext, counter tokens.Counter) (int, error)
}

// CacheBinder is the optional executor hook the M9 response-cache middleware
// consults before a step runs (ADR-011, ticket 9.5): it projects the
// resolved request onto the cache key's inputs and supplies the
// eligibility/determinism signals the policy needs. An executor that
// implements it (the llm, tool, and retrieve executors) participates in the
// response cache; one that does not (noop, echo, sleep, control-flow steps)
// is never cached — the middleware is a no-op for steps with no binder.
//
// The binding carries the two plugin identities that bear on the output (the
// cacheable executor and the concrete external plugin — provider/tool/
// retriever), the CONCRETE plugin's capability flags (ADR-011: a pure tool
// is cacheable even though the tool executor is side-effectful, so the
// invoked tool's flags decide, not the executor's), an executor-supplied
// determinism signal (temperature==0 for llm, the tool's cacheable-and-pure
// flags, false for a corpus-mutable retrieve), and the resolved request body.
//
// A nil binding with a nil error means "this step is not cacheable" (a
// bypass with no error — e.g. the executor has no registry wired). An error
// means the binding could not be computed (an unresolvable model, a corrupt
// config): the middleware then skips caching and lets Execute run and land
// its own classified failure, so the routing judgment lives in one place —
// the same convention ResourceClaim uses.
type CacheBinder interface {
	CacheBinding(sc StepContext) (*CacheBinding, error)
}

// CacheBinding is what a CacheBinder projects for the cache middleware: the
// key inputs plus the policy signals. The middleware combines Caps +
// Deterministic + the step's authored cache policy into a read/write
// decision (cache.Decide) and, when it decides to read or write, builds the
// content key from Executor + Plugin + Request (cache.Key).
type CacheBinding struct {
	// Executor is the cacheable executor plugin's identity (kind/name/version).
	Executor cache.PluginRef
	// Plugin is the concrete external plugin's identity: the model provider,
	// the invoked tool, or the retriever. It is hashed into the key and
	// namespaces the Redis entry (bust-by-prefix, 9.6).
	Plugin cache.PluginRef
	// Caps are the CONCRETE plugin's ADR-009 capability flags — the hard
	// eligibility gate (Cacheable && !SideEffectful) reads these, not the
	// executor's.
	Caps plugin.Capabilities
	// Deterministic is the executor's plugin-specific determinism judgment:
	// it drives the default policy (deterministic → cache read-write; else
	// bypass unless the step opts in).
	Deterministic bool
	// Request is the resolved, rendered request body — one of
	// cache.LLMRequest / cache.ToolRequest / cache.RetrieveRequest.
	Request cache.Request
}

// configAs decodes sc.Config into the typed config struct T registered
// for sc.StepType. An absent config yields a typed nil (callers
// required-field-check it, mirroring how Validate treats a missing config
// key); a config that fails strict decoding, or a T that does not match
// the step type's registered struct, is an *InvalidConfigError. Config
// was already validated at submit time, so a failure here means corrupt
// stored state or a worker/definition version skew — worth a typed error,
// not worth a recovery path.
func configAs[T dag.StepConfig](sc StepContext) (T, error) {
	var zero T
	cfg, err := dag.DecodeStepConfig(sc.StepType, sc.Config)
	if err != nil {
		return zero, &InvalidConfigError{StepType: string(sc.StepType), cause: err}
	}
	if cfg == nil {
		return zero, nil
	}
	typed, ok := cfg.(T)
	if !ok {
		return zero, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("config decoded to %T, executor expected %T", cfg, zero)}
	}
	return typed, nil
}
