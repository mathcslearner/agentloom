// Package log builds the canonical structured logger for agentloom and
// carries it through context.Context.
//
// All engine code logs through *slog.Logger instances produced by New (one
// JSON object per line in production) and attaches the canonical fields
// declared here — never ad-hoc key strings — so logs stay greppable and
// joinable across the API server and the worker fleet.
package log

import (
	"io"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/config"
)

// Canonical field names. Use these constants (or the typed attr helpers
// below) instead of string literals so field names never drift.
const (
	FieldRunID    = "run_id"
	FieldStepID   = "step_id"
	FieldAttempt  = "attempt"
	FieldWorkerID = "worker_id"
	FieldTraceID  = "trace_id"
	// FieldSpanID is the active span, hex (ticket 7.1, ADR-008) — stamped
	// alongside trace_id from span context once 7.3 wires tracing through
	// the queue.
	FieldSpanID = "span_id"
	// FieldKeyID identifies the API key behind an authenticated request
	// (ticket 6.1, ADR-007) — always the key's row id or the "root"
	// sentinel, never key material.
	FieldKeyID = "key_id"
	// FieldService names the emitting deployable (ticket 7.1, ADR-008):
	// one of internal/obs/metrics' Service* constants.
	FieldService = "service"
)

// RunID returns the canonical attr for a run identifier.
func RunID(id string) slog.Attr { return slog.String(FieldRunID, id) }

// StepID returns the canonical attr for a step identifier.
func StepID(id string) slog.Attr { return slog.String(FieldStepID, id) }

// Attempt returns the canonical attr for a step attempt number.
func Attempt(n int) slog.Attr { return slog.Int(FieldAttempt, n) }

// WorkerID returns the canonical attr for a worker identifier.
func WorkerID(id string) slog.Attr { return slog.String(FieldWorkerID, id) }

// TraceID returns the canonical attr for a trace identifier.
func TraceID(id string) slog.Attr { return slog.String(FieldTraceID, id) }

// SpanID returns the canonical attr for a span identifier.
func SpanID(id string) slog.Attr { return slog.String(FieldSpanID, id) }

// Service returns the canonical attr for the emitting deployable.
func Service(name string) slog.Attr { return slog.String(FieldService, name) }

// KeyID returns the canonical attr for an API key identifier.
func KeyID(id string) slog.Attr { return slog.String(FieldKeyID, id) }

// New builds a logger writing to w per cfg: JSON (one object per line) or
// text encoding, records below cfg.Level dropped, source locations stamped
// when cfg.AddSource is set.
func New(cfg config.LogConfig, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     cfg.Level,
		AddSource: cfg.AddSource,
	}
	var h slog.Handler
	if cfg.Format == config.LogFormatText {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}
