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

	"github.com/mathcslearner/agentloom/internal/dag"
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
}

// Usage is one attempt's token accounting, mirroring llm.Usage but kept
// in exec so the executor SPI does not force every caller to import the
// provider package. Executors that meter no tokens leave Output.Usage nil.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
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
