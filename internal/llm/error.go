package llm

import (
	"fmt"
	"strconv"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// Error is a classified provider failure: the ADR-006 error class the
// M5 retry engine consumes, plus the provider-reported detail worth
// keeping (status, provider error code, request id, and the provider's
// suggested retry-after for 429/529 — M9's backpressure input).
//
// Secret hygiene is structural: the type has no field that can hold
// request headers or the outgoing payload, and constructors only ever
// ingest the response body's error type/message and the request-id /
// retry-after response headers — key material cannot reach an Error
// because it never enters the error path.
//
// Context cancellation and deadline expiry are deliberately NOT an
// Error: providers pass ctx errors through wrapped (errors.Is-able
// against context.Canceled / context.DeadlineExceeded) so the engine
// can judge timeout vs. cancelled from context state (ADR-006).
type Error struct {
	// Provider names the provider that failed ("anthropic").
	Provider string
	// Class is the ADR-006 class: transient or permanent. Providers
	// never classify timeout/cancelled (the engine's) nor
	// validation_failed (reserved until M11).
	Class dag.ErrorClass
	// Status is the HTTP status of the failed call; 0 for
	// transport-level failures that produced no response.
	Status int
	// Code is the provider's error type when the error body decoded
	// (e.g. "rate_limit_error", "invalid_request_error"); empty
	// otherwise.
	Code string
	// RetryAfter is the provider's suggested wait before retrying,
	// parsed from the Retry-After response header; zero when the
	// provider suggested none.
	RetryAfter time.Duration
	// RequestID is the provider's request id header, for support
	// correlation; empty when absent.
	RequestID string
	// Message is the provider's error message (or a client-side
	// description for validation/transport failures).
	Message string
	// cause is the wrapped underlying error, when one exists
	// (transport errors, body decode failures).
	cause error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Provider, e.Class)
	if e.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	if e.RequestID != "" {
		s += fmt.Sprintf(" (request-id %s)", e.RequestID)
	}
	return s
}

func (e *Error) Unwrap() error { return e.cause }

// classifyStatus maps an HTTP status onto the ADR-006 class per the
// 8.3 taxonomy: 4xx means the request itself is wrong and will fail
// identically on every retry (permanent), except 408 (server-side
// timeout) and 429 (rate limit), which heal on their own; everything
// else — 5xx including Anthropic's 529 overloaded, and unknown shapes
// — is the provider struggling (transient, ADR-006's default).
func classifyStatus(status int) dag.ErrorClass {
	switch {
	case status == 408 || status == 429:
		return dag.ClassTransient
	case status >= 400 && status < 500:
		return dag.ClassPermanent
	default:
		return dag.ClassTransient
	}
}

// parseRetryAfter parses a Retry-After header's delta-seconds form.
// The HTTP-date form is deliberately unsupported: parsing it needs a
// wall clock (violating the injectable-time invariant for a header no
// LLM provider is known to send) and a zero RetryAfter simply means
// "no suggestion" — the retry engine's own backoff applies.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	secs, err := strconv.ParseInt(header, 10, 64)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
