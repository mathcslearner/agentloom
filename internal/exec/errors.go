package exec

import (
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// ErrorClass aliases the dag package's contract type (ADR-006): the
// definition contract references the vocabulary (`retry.retry_on`), so it
// lives there; the execution layer speaks it through this alias.
type ErrorClass = dag.ErrorClass

// ClassifiedError is the typed wrapper an executor returns when it knows
// its error's class better than the engine's default (unclassified errors
// default to transient — ADR-006 "Classification"). errors.As unwraps it
// anywhere in a wrap chain. Executors classify only transient/permanent:
// the engine assigns timeout and cancelled itself from context state
// (M5.3/5.6), and validation_failed is reserved until M11.
type ClassifiedError struct {
	// Class is the declared error class.
	Class ErrorClass
	// Err is the wrapped cause. Required.
	Err error
}

func (e *ClassifiedError) Error() string {
	return fmt.Sprintf("[%s] %v", e.Class, e.Err)
}

func (e *ClassifiedError) Unwrap() error { return e.Err }

// Transientf builds a ClassifiedError marked transient: the failure can
// plausibly heal on its own (network error, provider 5xx/429, contention)
// and deserves a retry per the step's policy.
func Transientf(format string, args ...any) error {
	return &ClassifiedError{Class: dag.ClassTransient, Err: fmt.Errorf(format, args...)}
}

// Permanentf builds a ClassifiedError marked permanent: no identical
// re-execution can succeed (bad request shape, nonexistent model, a
// deterministic content failure), so retrying only wastes budget.
func Permanentf(format string, args ...any) error {
	return &ClassifiedError{Class: dag.ClassPermanent, Err: fmt.Errorf(format, args...)}
}

// ErrUnknownType is the sentinel every registry miss unwraps to. Test
// with errors.Is; errors.As an *UnknownTypeError for the offending type.
// The worker treats it as a permanent step failure, never a retry: no
// number of retries makes an unregistered executor appear.
var ErrUnknownType = errors.New("unknown executor type")

// ErrInvalidConfig is the sentinel every executor-side config decode
// failure unwraps to. Config was validated at submit time, so hitting
// this at execution time means corrupt stored state or a version skew
// between the worker and the definition — also permanent, never a retry.
var ErrInvalidConfig = errors.New("invalid step config")

// UnknownTypeError reports a registry lookup for a step type with no
// registered executor.
type UnknownTypeError struct {
	// Type is the step type that missed.
	Type string
}

func (e *UnknownTypeError) Error() string {
	return fmt.Sprintf("no executor registered for step type %q", e.Type)
}

func (e *UnknownTypeError) Unwrap() error { return ErrUnknownType }

// InvalidConfigError reports a step config that failed executor-side
// decoding, or decoded to a different struct than the executor expects.
type InvalidConfigError struct {
	// StepType is the step type whose config was rejected.
	StepType string
	cause    error
}

func (e *InvalidConfigError) Error() string {
	if e.cause == nil {
		return fmt.Sprintf("invalid config for step type %q", e.StepType)
	}
	return fmt.Sprintf("invalid config for step type %q: %v", e.StepType, e.cause)
}

func (e *InvalidConfigError) Unwrap() error { return ErrInvalidConfig }
