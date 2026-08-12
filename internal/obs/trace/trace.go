// Package trace sets up the OpenTelemetry trace pipeline for agentloom's
// deployables (ticket 7.1, ADR-008).
//
// Setup installs the global TracerProvider and the W3C tracecontext +
// baggage propagators. Disabled (the default — ADR-008's off switch for
// tests), it installs the no-op provider: no exporter, no dials, no
// goroutines. Enabled, spans export via OTLP/gRPC through a batch
// processor, with service.name/service.version/service.instance.id
// resources. Ticket 7.3 builds the actual spans (submission root span,
// queue-envelope propagation, attempt spans with retry links) on top of
// this pipeline.
package trace

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// The semconv version must match resource.Default()'s schema URL for
	// this otel SDK release, or resource.Merge rejects the pair.
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	nooptrace "go.opentelemetry.io/otel/trace/noop"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/version"
)

// Setup installs the global TracerProvider and propagators per cfg and
// returns the shutdown function that flushes and stops the pipeline
// (bound it with a context deadline at deployable shutdown). service is
// the OTel service.name (metrics.ServiceAPI / metrics.ServiceWorker);
// instanceID distinguishes replicas (the worker's consumer name, the
// API's hostname).
//
// With cfg.OTelEnabled false this is a pure no-op: the noop provider is
// installed and shutdown returns nil immediately. SDK-internal errors
// (an unreachable collector, a dropped batch) are routed to logger at
// warn — telemetry must never take the service down (ADR-008).
func Setup(ctx context.Context, cfg config.ObsConfig, service, instanceID string, logger *slog.Logger) (shutdown func(context.Context) error, err error) {
	// The propagator is global wiring both modes share: extracting from /
	// injecting into carriers is harmless without a live provider.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	if !cfg.OTelEnabled {
		otel.SetTracerProvider(nooptrace.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("obs: otel pipeline error", slog.Any("error", err))
	}))

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTelEndpoint)}
	if cfg.OTelInsecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	// The exporter dials lazily and retries in the background, so an
	// absent collector (obs profile not running) costs warn logs, never
	// boot failure.
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("obs: building OTLP exporter for %s: %w", cfg.OTelEndpoint, err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(service),
		semconv.ServiceVersion(version.Version),
		semconv.ServiceInstanceID(instanceID),
	))
	if err != nil {
		return nil, fmt.Errorf("obs: building resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.OTelSampleRatio))),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}
