package trace_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/obs/trace"
)

// Setup mutates OTel's process-global provider/propagator, so these tests
// are deliberately not parallel with each other.

// TestSetupDisabledIsNoop pins the ADR-008 off switch: disabled installs
// a provider whose spans record nothing, and shutdown is free.
func TestSetupDisabledIsNoop(t *testing.T) {
	shutdown, err := trace.Setup(context.Background(), config.ObsConfig{OTelEnabled: false},
		metrics.ServiceWorker, "test-instance", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Setup disabled: %v", err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if span.IsRecording() {
		t.Error("disabled telemetry produced a recording span; want noop")
	}
	if span.SpanContext().IsValid() {
		t.Error("disabled telemetry produced a valid span context; want noop")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown: %v", err)
	}
}

// TestSetupEnabledRecordsAndShutsDown proves the enabled path builds a
// real SDK provider (recording spans, valid contexts) without any live
// collector — the exporter dials lazily and must never block boot — and
// that shutdown respects its context deadline against the dead endpoint.
func TestSetupEnabledRecordsAndShutsDown(t *testing.T) {
	cfg := config.ObsConfig{
		OTelEnabled:     true,
		OTelEndpoint:    "127.0.0.1:1", // deliberately nothing listening
		OTelInsecure:    true,
		OTelSampleRatio: 1.0,
	}
	start := time.Now()
	shutdown, err := trace.Setup(context.Background(), cfg,
		metrics.ServiceAPI, "test-instance", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Setup enabled: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Setup took %v; must not block on the collector", elapsed)
	}

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	if !span.IsRecording() {
		t.Error("enabled telemetry span not recording; want SDK provider")
	}
	if !span.SpanContext().IsValid() {
		t.Error("enabled telemetry span context invalid; want real trace/span IDs")
	}
	span.End()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err == nil {
		// The batch flush against a dead endpoint may or may not surface
		// an error depending on retry timing; either way it must return.
		t.Log("shutdown returned nil against dead collector (flush window empty)")
	}
	// Restore the noop provider so later tests in the package (or other
	// packages in this process) never export anywhere.
	if _, err := trace.Setup(context.Background(), config.ObsConfig{}, metrics.ServiceAPI, "test-instance",
		slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("restoring noop provider: %v", err)
	}
}
