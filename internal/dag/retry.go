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
