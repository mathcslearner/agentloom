package trace

import (
	"context"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// testSpanContext builds a valid remote span context to round-trip.
func testSpanContext(t *testing.T) oteltrace.SpanContext {
	t.Helper()
	traceID, err := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
	return oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: oteltrace.FlagsSampled, Remote: true,
	})
}

func TestInjectExtractRoundTrip(t *testing.T) {
	sc := testSpanContext(t)
	ctx := oteltrace.ContextWithSpanContext(context.Background(), sc)

	traceparent, tracestate := Inject(ctx)
	if traceparent == "" {
		t.Fatal("Inject returned empty traceparent for a valid span context")
	}
	if tracestate != "" {
		t.Errorf("Inject tracestate = %q, want empty (none set)", tracestate)
	}

	got := oteltrace.SpanContextFromContext(Extract(context.Background(), traceparent, tracestate))
	if got.TraceID() != sc.TraceID() || got.SpanID() != sc.SpanID() {
		t.Errorf("round-trip = %s/%s, want %s/%s", got.TraceID(), got.SpanID(), sc.TraceID(), sc.SpanID())
	}
	if !got.IsSampled() {
		t.Error("round-trip lost the sampled flag")
	}
	if !got.IsRemote() {
		t.Error("extracted span context should be remote")
	}
}

func TestInjectWithoutSpanContext(t *testing.T) {
	traceparent, tracestate := Inject(context.Background())
	if traceparent != "" || tracestate != "" {
		t.Errorf("Inject on empty ctx = (%q, %q), want empty pair", traceparent, tracestate)
	}
}

func TestExtractTolerance(t *testing.T) {
	for _, tc := range []struct {
		name        string
		traceparent string
	}{
		{"empty", ""},
		{"garbage", "not-a-traceparent"},
		{"truncated", "00-4bf92f3577b34da6"},
		{"zero trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := Extract(context.Background(), tc.traceparent, "")
			if sc := oteltrace.SpanContextFromContext(ctx); sc.IsValid() {
				t.Errorf("Extract(%q) produced a valid span context %v, want none", tc.traceparent, sc)
			}
		})
	}
}

func TestSpanContextFormat(t *testing.T) {
	sc := testSpanContext(t)
	got := SpanContext(sc)
	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if got != want {
		t.Errorf("SpanContext = %q, want %q", got, want)
	}
	if s := SpanContext(oteltrace.SpanContext{}); s != "" {
		t.Errorf("SpanContext(invalid) = %q, want empty", s)
	}
}

func TestLinkFromTraceparent(t *testing.T) {
	sc := testSpanContext(t)
	link, ok := LinkFromTraceparent(SpanContext(sc))
	if !ok {
		t.Fatal("LinkFromTraceparent rejected a valid traceparent")
	}
	if link.SpanContext.TraceID() != sc.TraceID() || link.SpanContext.SpanID() != sc.SpanID() {
		t.Errorf("link = %s/%s, want %s/%s",
			link.SpanContext.TraceID(), link.SpanContext.SpanID(), sc.TraceID(), sc.SpanID())
	}
	if _, ok := LinkFromTraceparent(""); ok {
		t.Error("LinkFromTraceparent accepted the empty string")
	}
	if _, ok := LinkFromTraceparent("junk"); ok {
		t.Error("LinkFromTraceparent accepted junk")
	}
}
