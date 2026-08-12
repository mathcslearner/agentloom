package trace

// Propagation helpers (ticket 7.3): the string-typed bridge between OTel
// span contexts and the places trace context lives durably — queue
// envelopes, runs.trace_parent/trace_state, task_outbox rows, and
// run_steps.trace_span. Everything speaks W3C traceparent/tracestate, so
// one format serves wire and storage alike.
//
// The helpers use an explicit tracecontext+baggage propagator instance,
// not otel.GetTextMapPropagator(): the global default is a no-op until
// Setup runs, and these functions must round-trip deterministically in
// tests that never install the pipeline.

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// propagator is the fixed W3C composite Setup also installs globally.
var propagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{})

// Carrier keys, per the W3C Trace Context spec — also the queue envelope
// field names (ADR-005 reserved them; 7.3 populates them).
const (
	keyTraceParent = "traceparent"
	keyTraceState  = "tracestate"
)

// Inject renders ctx's span context as traceparent/tracestate strings.
// Both come back empty when ctx carries no valid span context (tracing
// off, or no span started) — absent context stays absent, never an
// empty-string sentinel.
func Inject(ctx context.Context) (traceparent, tracestate string) {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier.Get(keyTraceParent), carrier.Get(keyTraceState)
}

// Extract returns ctx extended with the remote span context parsed from
// traceparent/tracestate. Empty or malformed input returns ctx unchanged —
// a span started from it becomes a root, which is exactly what "no
// context" means.
func Extract(ctx context.Context, traceparent, tracestate string) context.Context {
	if traceparent == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{keyTraceParent: traceparent}
	if tracestate != "" {
		carrier.Set(keyTraceState, tracestate)
	}
	return propagator.Extract(ctx, carrier)
}

// SpanContext formats a span context as a traceparent string — the
// run_steps.trace_span storage form. Empty when sc is invalid.
func SpanContext(sc oteltrace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	ctx := oteltrace.ContextWithSpanContext(context.Background(), sc)
	traceparent, _ := Inject(ctx)
	return traceparent
}

// WithLogContext stamps the active span's trace_id/span_id onto ctx's
// carried logger (ADR-008 log field dictionary: how log lines join to
// Jaeger). No valid span context — tracing off — leaves ctx unchanged.
func WithLogContext(ctx context.Context) context.Context {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ctx
	}
	return log.With(ctx, log.TraceID(sc.TraceID().String()), log.SpanID(sc.SpanID().String()))
}

// LinkFromTraceparent parses a stored traceparent string into a span link
// (ticket 7.3: retries, takeovers, and fan-in joins are links, never
// parent-child). ok is false when s is empty or malformed — callers skip
// the link; a broken stored context must never fail an execution path.
func LinkFromTraceparent(s string) (oteltrace.Link, bool) {
	if s == "" {
		return oteltrace.Link{}, false
	}
	sc := oteltrace.SpanContextFromContext(Extract(context.Background(), s, ""))
	if !sc.IsValid() {
		return oteltrace.Link{}, false
	}
	return oteltrace.Link{SpanContext: sc}, true
}
