package tools

import (
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// Error is a classified tool failure: the ADR-006 class the M5 retry
// engine consumes, plus a short message. The tool executor wraps it into
// an exec.ClassifiedError so the engine honors the declared class.
//
// Secret hygiene is structural (the internal/llm precedent): the type has
// no field that can hold request headers or a request/response body, so
// header values and payloads cannot reach an error even by accident.
//
// Context cancellation and deadline expiry are deliberately NOT an Error:
// tools pass ctx errors through unwrapped so the engine judges timeout vs.
// cancelled from context state (ADR-006 rows 3/8).
type Error struct {
	// Tool names the tool that failed ("http_request").
	Tool string
	// Class is the ADR-006 class: transient or permanent. Tools never
	// classify timeout/cancelled (the engine's) nor validation_failed
	// (reserved until M11).
	Class dag.ErrorClass
	// Message describes the failure without leaking argument or header
	// values (host/method/status only, for http_request).
	Message string
	// cause is the wrapped underlying error, when one exists.
	cause error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Tool, e.Class)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.cause }

// transientf builds a transient *Error for tool tool. The tool parameter
// mirrors permanentf's (which several tools call with distinct names); it
// stays for that symmetry and for future non-http tools that return
// transient errors, even though http_request is its only caller today.
//
//nolint:unparam // tool is part of the helper's contract; symmetric with permanentf
func transientf(tool, format string, args ...any) *Error {
	return &Error{Tool: tool, Class: dag.ClassTransient, Message: fmt.Sprintf(format, args...)}
}

// permanentf builds a permanent *Error for tool tool.
func permanentf(tool, format string, args ...any) *Error {
	return &Error{Tool: tool, Class: dag.ClassPermanent, Message: fmt.Sprintf(format, args...)}
}

// HostNotAllowedError reports that http_request was asked to reach a host
// not on the configured allowlist — the SSRF guard (ticket 8.7). It is a
// deterministic permanent failure: no retry can make a blocked host
// reachable, and the request is provably never sent. It is the ticket's
// required typed error, so it stays its own type (errors.As-reachable)
// rather than a plain *Error.
type HostNotAllowedError struct {
	// Host is the disallowed host (host or host:port) as parsed from the
	// request URL — never the full URL, which could carry query secrets.
	Host string
}

func (e *HostNotAllowedError) Error() string {
	return fmt.Sprintf("host %q is not on the http_request allowlist", e.Host)
}

// Class reports the ADR-006 class: a blocked host is permanent.
func (e *HostNotAllowedError) Class() dag.ErrorClass { return dag.ClassPermanent }

// ErrUnknownTool is the sentinel every tool-registry miss unwraps to.
// Test with errors.Is; errors.As an *UnknownToolError for the offending
// name. The executor maps it permanent — no retry conjures a tool the
// deployment did not register.
var ErrUnknownTool = errors.New("unknown tool")

// UnknownToolError reports a lookup for a tool name with no registered
// tool.
type UnknownToolError struct {
	// Name is the tool name that missed.
	Name string
}

func (e *UnknownToolError) Error() string {
	return fmt.Sprintf("no tool registered under name %q", e.Name)
}

func (e *UnknownToolError) Unwrap() error { return ErrUnknownTool }

// ErrInvalidArgs is the sentinel every args-validation failure unwraps
// to. Args were validated at submit time only for the `tool` step's outer
// shape (tool name present); the per-tool args schema is enforced here at
// dispatch, and a violation is a permanent failure — the same rendered
// args fail identically on every retry.
var ErrInvalidArgs = errors.New("invalid tool args")

// ArgsValidationError reports a step's rendered args failing a tool's
// args JSON Schema (ticket 8.7's pre-invocation gate). The executor maps
// it permanent and the tool's Invoke is never reached.
type ArgsValidationError struct {
	// Tool is the tool whose schema rejected the args.
	Tool string
	// Detail is the validator's human-readable violation list. It
	// describes structure (missing/typed/unknown fields), not secret
	// values, so it is safe to surface.
	Detail string
}

func (e *ArgsValidationError) Error() string {
	return fmt.Sprintf("args for tool %q failed schema validation: %s", e.Tool, e.Detail)
}

func (e *ArgsValidationError) Unwrap() error { return ErrInvalidArgs }
