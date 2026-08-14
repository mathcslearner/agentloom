package retrieval

import (
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// Error is a classified retriever failure: the ADR-006 class the M5 retry
// engine consumes, plus a short message. The retrieve executor wraps it
// into an exec.ClassifiedError so the engine honors the declared class.
//
// Secret hygiene is structural (the internal/tools and internal/llm
// precedent): the type has no field that can hold a query or document
// content, so corpus text cannot reach an error even by accident.
//
// Context cancellation and deadline expiry are deliberately NOT an Error:
// retrievers pass ctx errors through unwrapped so the engine judges
// timeout vs. cancelled from context state (ADR-006 rows 3/8).
type Error struct {
	// Retriever names the retriever that failed ("pg_fulltext").
	Retriever string
	// Class is the ADR-006 class: transient (datastore hiccup) or
	// permanent (deterministic corpus/query problem). Retrievers never
	// classify timeout/cancelled (the engine's) nor validation_failed
	// (reserved until M11).
	Class dag.ErrorClass
	// Message describes the failure without leaking query or content.
	Message string
	// cause is the wrapped underlying error, when one exists.
	cause error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Retriever, e.Class)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.cause }

// Transientf builds a transient *Error for retriever r wrapping cause.
func Transientf(r string, cause error, format string, args ...any) *Error {
	return &Error{Retriever: r, Class: dag.ClassTransient, Message: fmt.Sprintf(format, args...), cause: cause}
}

// Permanentf builds a permanent *Error for retriever r wrapping cause.
func Permanentf(r string, cause error, format string, args ...any) *Error {
	return &Error{Retriever: r, Class: dag.ClassPermanent, Message: fmt.Sprintf(format, args...), cause: cause}
}

// ErrUnknownRetriever is the sentinel every retriever-registry miss
// unwraps to. Test with errors.Is; errors.As an *UnknownRetrieverError for
// the offending name. The executor maps it permanent — no retry conjures a
// retriever the deployment did not register.
var ErrUnknownRetriever = errors.New("unknown retriever")

// UnknownRetrieverError reports a lookup for a retriever name with no
// registered retriever.
type UnknownRetrieverError struct {
	// Name is the retriever name that missed.
	Name string
}

func (e *UnknownRetrieverError) Error() string {
	return fmt.Sprintf("no retriever registered under name %q", e.Name)
}

func (e *UnknownRetrieverError) Unwrap() error { return ErrUnknownRetriever }
