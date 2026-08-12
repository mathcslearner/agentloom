package dag

import "time"

// This file is the definition-contract half of ADR-006: the retry policy
// steps may carry, the error-class vocabulary `retry_on` references, and
// the workflow failure policy. The runtime half — classification,
// backoff computation, dead-lettering — is the engine's (M5.2–5.4);
// this package only defines and validates the authored fields.

// ErrorClass classifies a failed execution attempt (ADR-006). It lives
// here, not in the exec package, because the definition contract
// references it (`retry.retry_on`); the execution layer aliases it.
type ErrorClass string

// The error-class vocabulary (ADR-006 "Error classes").
const (
	// ClassTransient marks a failure that can plausibly heal on its own
	// (network error, provider 5xx/429, contention). The default class
	// for unclassified executor errors.
	ClassTransient ErrorClass = "transient"

	// ClassPermanent marks a failure no identical re-execution can fix
	// (unknown executor type, corrupt config, deterministic content
	// failure). Never retryable.
	ClassPermanent ErrorClass = "permanent"

	// ClassTimeout marks an attempt cancelled at its execution-timeout
	// deadline by a live worker (M5.3) — distinct from a crash, which
	// records no class at all.
	ClassTimeout ErrorClass = "timeout"

	// ClassCancelled marks an attempt cancelled by run-level control
	// flow (cancel, park, drain — M5.6/5.7). Never retryable.
	ClassCancelled ErrorClass = "cancelled"

	// ClassValidationFailed is reserved for M11 (ADR-013): output
	// validation rejected an otherwise-successful result, feeding the
	// semantic-retry loop. Rejected everywhere until then.
	ClassValidationFailed ErrorClass = "validation_failed"
)

// errorClasses enumerates the full vocabulary in documentation order.
var errorClasses = []ErrorClass{
	ClassTransient, ClassPermanent, ClassTimeout, ClassCancelled, ClassValidationFailed,
}

// JitterMode selects how a computed backoff delay is randomized.
type JitterMode string

// Permitted jitter modes (ADR-006).
const (
	// JitterFull draws the actual delay uniformly from [0, computed] —
	// AWS full jitter, the engine default.
	JitterFull JitterMode = "full"

	// JitterNone uses the computed delay exactly.
	JitterNone JitterMode = "none"
)

// jitterModes enumerates the valid JitterMode values.
var jitterModes = []JitterMode{JitterFull, JitterNone}

// FailurePolicy is the workflow-level run disposition when a step
// dead-letters (ADR-006 "Run disposition").
type FailurePolicy string

// Permitted failure policies. An absent `on_failure` means FailFast.
const (
	// FailFast fails the run in the transaction that dead-letters a
	// step. The pre-M5 behavior, and the default.
	FailFast FailurePolicy = "fail_fast"

	// ContinueIndependentBranches keeps the run running: only steps
	// downstream of the dead-lettered step are written off, independent
	// branches finish, and the run terminalizes failed with partial
	// results once every step is terminal.
	ContinueIndependentBranches FailurePolicy = "continue_independent_branches"
)

// failurePolicies enumerates the valid FailurePolicy values.
var failurePolicies = []FailurePolicy{FailFast, ContinueIndependentBranches}

// Retry-policy bounds (ADR-006 "Validation bounds"), enforced by
// Validate alongside the ADR-003 limits.
const (
	// MaxRetryAttempts caps `retry.max_attempts`.
	MaxRetryAttempts = 100

	// MaxBackoffInitial caps `retry.backoff.initial`.
	MaxBackoffInitial = time.Hour

	// MaxBackoffCap caps `retry.backoff.cap`.
	MaxBackoffCap = 24 * time.Hour

	// MaxBackoffMultiplier caps `retry.backoff.multiplier`.
	MaxBackoffMultiplier = 100

	// MaxStepTimeout caps the step-envelope `timeout` field (ticket 5.3).
	// Same ceiling as the backoff cap: an execution bound past a day is an
	// authoring mistake, not a workload.
	MaxStepTimeout = 24 * time.Hour
)

// RetryPolicy is a step's authored retry policy (ADR-006). Every field
// is optional; the engine merges absent fields over its documented
// defaults at run instantiation (`max_attempts` 3, initial 1s, cap 1m,
// multiplier 2, jitter full, retry_on [transient, timeout]), so this
// struct records only what the author explicitly set.
type RetryPolicy struct {
	// MaxAttempts is the total execution attempts including the first;
	// 1 means no retries. Zero means the key was absent (engine
	// default), mirroring Edge.MaxIterations.
	MaxAttempts int `json:"max_attempts,omitempty"`

	// Backoff shapes the delay between attempts; nil means engine
	// defaults.
	Backoff *BackoffSpec `json:"backoff,omitempty"`

	// Jitter randomizes computed delays; empty means the key was
	// absent (engine default: full).
	Jitter JitterMode `json:"jitter,omitempty"`

	// RetryOn lists the error classes retried for this step; nil means
	// the key was absent (engine default: transient and timeout). A
	// class outside the list is treated as permanent for this step.
	RetryOn []ErrorClass `json:"retry_on,omitempty"`
}

// BackoffSpec is the exponential-backoff shape: the delay before retry
// n (1-based) is min(cap, initial × multiplier^(n−1)), jittered per the
// policy's Jitter mode.
type BackoffSpec struct {
	// Initial is the first delay, a Go duration string ("500ms", "1s").
	// Required when a backoff block is present.
	Initial string `json:"initial,omitempty"`

	// Cap bounds every computed delay, a Go duration string. Required
	// when a backoff block is present (ADR-006: an uncapped exponential
	// is a latent multi-hour stall).
	Cap string `json:"cap,omitempty"`

	// Multiplier is the exponential base; zero means the key was
	// absent (engine default 2). 1 means constant backoff.
	Multiplier float64 `json:"multiplier,omitempty"`
}

// The engine defaults (ADR-006 "Engine defaults"), applied field-wise by
// ResolveRetryPolicy when `retry` or any of its fields is absent.
const (
	// DefaultRetryMaxAttempts is the default total attempt budget.
	DefaultRetryMaxAttempts = 3

	// DefaultBackoffInitial is the default first delay.
	DefaultBackoffInitial = "1s"

	// DefaultBackoffCap is the default bound on every computed delay.
	DefaultBackoffCap = "1m"

	// DefaultBackoffMultiplier is the default exponential base.
	DefaultBackoffMultiplier = 2.0

	// DefaultRetryJitter is the default jitter mode.
	DefaultRetryJitter = JitterFull
)

// DefaultRetryOn returns the default retryable classes — everything
// retryable is retried by default. A fresh slice per call, so callers may
// own the result.
func DefaultRetryOn() []ErrorClass {
	return []ErrorClass{ClassTransient, ClassTimeout}
}

// ResolvedRetryPolicy is a step's *effective* retry policy: the authored
// fields merged over the engine defaults, every field concrete. It is
// materialized as JSON onto run_steps at instantiation (ticket 5.2,
// ADR-006 "Where the policy lives at runtime") so the failure path never
// reparses the definition snapshot and a worker upgrade cannot change an
// in-flight run's policy. Durations stay Go duration strings — readable
// in the row, parseable on the failure path, and guaranteed parseable by
// validation (authored) or by construction (defaults).
type ResolvedRetryPolicy struct {
	// MaxAttempts is the total attempt budget, counting the first.
	MaxAttempts int `json:"max_attempts"`

	// Backoff is the fully-resolved backoff shape.
	Backoff ResolvedBackoff `json:"backoff"`

	// Jitter is the delay randomization mode.
	Jitter JitterMode `json:"jitter"`

	// RetryOn lists the error classes retried for this step. A class
	// outside the list is treated as permanent for this step.
	RetryOn []ErrorClass `json:"retry_on"`
}

// ResolvedBackoff is BackoffSpec with every field concrete.
type ResolvedBackoff struct {
	Initial    string  `json:"initial"`
	Cap        string  `json:"cap"`
	Multiplier float64 `json:"multiplier"`
}

// ResolveRetryPolicy merges an authored policy (nil = no `retry` block)
// over the engine defaults, field-wise: an explicit block overrides only
// what it sets. Validation bounds (Validate) are assumed to hold — this
// is a pure merge, not a validator.
func ResolveRetryPolicy(p *RetryPolicy) ResolvedRetryPolicy {
	r := ResolvedRetryPolicy{
		MaxAttempts: DefaultRetryMaxAttempts,
		Backoff: ResolvedBackoff{
			Initial:    DefaultBackoffInitial,
			Cap:        DefaultBackoffCap,
			Multiplier: DefaultBackoffMultiplier,
		},
		Jitter:  DefaultRetryJitter,
		RetryOn: DefaultRetryOn(),
	}
	if p == nil {
		return r
	}
	if p.MaxAttempts != 0 {
		r.MaxAttempts = p.MaxAttempts
	}
	if p.Backoff != nil {
		// Validation requires initial and cap whenever a backoff block is
		// present; only the multiplier is optional within the block.
		r.Backoff.Initial = p.Backoff.Initial
		r.Backoff.Cap = p.Backoff.Cap
		if p.Backoff.Multiplier != 0 {
			r.Backoff.Multiplier = p.Backoff.Multiplier
		}
	}
	if p.Jitter != "" {
		r.Jitter = p.Jitter
	}
	if p.RetryOn != nil {
		r.RetryOn = append([]ErrorClass(nil), p.RetryOn...)
	}
	return r
}

// Retries reports whether class is retryable under this policy.
func (r ResolvedRetryPolicy) Retries(class ErrorClass) bool {
	for _, c := range r.RetryOn {
		if c == class {
			return true
		}
	}
	return false
}
