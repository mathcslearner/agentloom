package validate

import (
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// Error is a classified validator failure — a transport failure of the
// validation stage itself, NOT a fail verdict. A validator returns one when
// it cannot render a judgment: an llm_judge's provider call errored (11.5),
// a validator's own dependency is unreachable. The engine wraps it into an
// exec.ClassifiedError so the ADR-006 retry engine honors the declared
// class; a fail verdict, by contrast, is a successful validation with a
// negative result and routes as validation_failed (ADR-013's decision
// table).
//
// Secret hygiene is structural (the tools/llm precedent): the type has no
// field that can hold a validated output or a rubric, so payloads cannot
// reach an error even by accident.
//
// Context cancellation and deadline expiry are deliberately NOT an Error:
// validators pass ctx errors through unwrapped so the engine judges timeout
// vs. cancelled from context state (ADR-006 rows 3/8).
type Error struct {
	// Validator names the validator that failed ("llm_judge").
	Validator string
	// Class is the ADR-006 class: transient or permanent. Validators never
	// declare timeout/cancelled (the engine's) nor validation_failed (that
	// is a fail verdict, not an error).
	Class dag.ErrorClass
	// Message describes the failure without leaking output or rubric values.
	Message string
	// Usage is the judge call's token accounting when a cost-bearing
	// validator billed a provider call before erroring (11.5): an llm_judge
	// whose model answered but produced a malformed rubric answer under
	// `on_error: fail` spent real money that the engine must still meter as
	// overhead on the serving step (ADR-012 rule 4). Nil when nothing billed
	// (a provider error, a client-side validation failure). Like the rest of
	// Error it carries no output or rubric — only counts and identifiers.
	Usage *ValidatorUsage
	// cause is the wrapped underlying error, when one exists.
	cause error
}

// WithUsage attaches a cost-bearing validator's token accounting to the
// error, for the case where the judge call billed before the failure (a
// malformed answer under `on_error: fail`). Returns the receiver so it can
// be chained onto a Transientf/Permanentf constructor.
func (e *Error) WithUsage(u *ValidatorUsage) *Error {
	e.Usage = u
	return e
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Validator, e.Class)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.cause }

// Transientf builds a transient *Error for validator v. Exported (the
// retrieval precedent) so validators in other packages — the llm_judge
// (11.5) lives in internal/validate, but future out-of-package validators
// register through the SPI — can classify their failures.
func Transientf(v string, cause error, format string, args ...any) *Error {
	return &Error{Validator: v, Class: dag.ClassTransient, Message: fmt.Sprintf(format, args...), cause: cause}
}

// Permanentf builds a permanent *Error for validator v.
func Permanentf(v string, cause error, format string, args ...any) *Error {
	return &Error{Validator: v, Class: dag.ClassPermanent, Message: fmt.Sprintf(format, args...), cause: cause}
}

// ErrUnknownValidator is the sentinel every validator-registry miss unwraps
// to. Test with errors.Is; errors.As an *UnknownValidatorError for the
// offending name. The engine maps it permanent — no retry conjures a
// validator the deployment did not register (ADR-013 pre-flight gate).
var ErrUnknownValidator = errors.New("unknown validator")

// UnknownValidatorError reports a chain naming a validator with no
// registered implementation.
type UnknownValidatorError struct {
	// Name is the validator name that missed.
	Name string
}

func (e *UnknownValidatorError) Error() string {
	return fmt.Sprintf("no validator registered under name %q", e.Name)
}

func (e *UnknownValidatorError) Unwrap() error { return ErrUnknownValidator }

// ErrInvalidConfig is the sentinel every validator-config validation
// failure unwraps to. A chain entry's config is checked against the named
// validator's compiled config schema at claim, pre-flight; a violation is a
// permanent failure (the same config fails identically on every attempt),
// and the validator's Validate is never reached — the tool-args gate,
// lifted to validators (ADR-013).
var ErrInvalidConfig = errors.New("invalid validator config")

// ConfigValidationError reports a chain entry's config failing its
// validator's config JSON Schema.
type ConfigValidationError struct {
	// Validator is the validator whose schema rejected the config.
	Validator string
	// Detail is the validator's human-readable violation list — structure
	// (missing/typed/unknown fields), not secret values, so it is safe to
	// surface.
	Detail string
}

func (e *ConfigValidationError) Error() string {
	return fmt.Sprintf("config for validator %q failed schema validation: %s", e.Validator, e.Detail)
}

func (e *ConfigValidationError) Unwrap() error { return ErrInvalidConfig }
