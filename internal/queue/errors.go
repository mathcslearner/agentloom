package queue

import (
	"errors"
	"fmt"
)

// ErrBadEnvelope is the sentinel every envelope decode failure unwraps to.
// Test with errors.Is; errors.As an *UnknownVersionError or
// *MalformedEnvelopeError for the diagnosis. Per ADR-005, a consumer that
// hits this must NOT ack: the entry stays in the PEL and its rising
// delivery count walks it into the poison path, where it is dead-lettered
// with its contents preserved instead of silently dropped.
var ErrBadEnvelope = errors.New("bad envelope")

// ErrNoGroup is returned by introspection when the consumer group does not
// exist on the stream (Redis NOGROUP). Call EnsureGroup first.
var ErrNoGroup = errors.New("consumer group does not exist")

// UnknownVersionError reports an envelope whose version field parsed as an
// integer this decoder does not speak. Under ADR-005's
// consumers-decode-before-producers-emit policy this indicates a deployment
// ordering bug, not a normal condition.
type UnknownVersionError struct {
	// Version is the envelope's declared version.
	Version int
}

func (e *UnknownVersionError) Error() string {
	return fmt.Sprintf("unknown envelope version %d (decoder speaks %d)", e.Version, EnvelopeVersion)
}

func (e *UnknownVersionError) Unwrap() error { return ErrBadEnvelope }

// MalformedEnvelopeError reports an envelope missing a required field or
// carrying an unparseable value (including a missing or non-integer
// version field, per ADR-005).
type MalformedEnvelopeError struct {
	// Field is the offending envelope field.
	Field string
	cause error
}

func (e *MalformedEnvelopeError) Error() string {
	if e.cause == nil {
		return fmt.Sprintf("malformed envelope: field %q", e.Field)
	}
	return fmt.Sprintf("malformed envelope: field %q: %v", e.Field, e.cause)
}

func (e *MalformedEnvelopeError) Unwrap() error { return ErrBadEnvelope }
