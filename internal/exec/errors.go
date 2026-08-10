package exec

import (
	"errors"
	"fmt"
)

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
