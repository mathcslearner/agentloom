package config

import (
	"fmt"
	"math"
	"strconv"
)

// Environment variables read by ObsConfig (ticket 7.1, ADR-008).
const (
	EnvObsMetricsAddr     = "AGENTLOOM_OBS_METRICS_ADDR"
	EnvObsPprofEnabled    = "AGENTLOOM_OBS_PPROF_ENABLED"
	EnvObsOTelEnabled     = "AGENTLOOM_OBS_OTEL_ENABLED"
	EnvObsOTelEndpoint    = "AGENTLOOM_OBS_OTEL_ENDPOINT"
	EnvObsOTelInsecure    = "AGENTLOOM_OBS_OTEL_INSECURE"
	EnvObsOTelSampleRatio = "AGENTLOOM_OBS_OTEL_SAMPLE_RATIO"
)

// Telemetry defaults (ADR-008): everything off. Unit tests, the
// storetest/queuetest harnesses, and the crash suite's subprocess workers
// must run with zero extra ports bound and zero collector dial attempts;
// compose and the telemetry integration tests opt in explicitly.
const (
	// DefaultObsOTelEndpoint is the conventional OTLP/gRPC port, pointing
	// at localhost because the endpoint only matters once OTelEnabled is
	// set — compose overrides it to the in-network Jaeger address.
	DefaultObsOTelEndpoint    = "localhost:4317"
	DefaultObsOTelSampleRatio = 1.0
)

// ObsConfig configures telemetry for both deployables (ticket 7.1,
// ADR-008): the Prometheus admin listener and the OTel trace pipeline.
// One shared config, not per-deployable — the two binaries read the same
// knobs and run in separate processes, so the admin addr never collides.
type ObsConfig struct {
	// MetricsAddr is the admin listener address (net.Listen "host:port"
	// form) serving GET /metrics and GET /healthz. Empty (the default)
	// starts no listener at all — telemetry's off switch for tests.
	MetricsAddr string
	// OTelEnabled turns the OTel trace pipeline on. False (the default)
	// installs the no-op TracerProvider: no exporter, no dials.
	OTelEnabled bool
	// OTelEndpoint is the OTLP/gRPC collector endpoint ("host:port", no
	// scheme). Only consulted when OTelEnabled is set.
	OTelEndpoint string
	// OTelInsecure dials the collector without TLS. Default true: dev
	// collectors (compose Jaeger) are plaintext and in-network; production
	// sets false.
	OTelInsecure bool
	// OTelSampleRatio is the ParentBased(TraceIDRatioBased) sampler
	// argument in (0, 1]. Default 1.0 — sample everything; dev traffic is
	// small, and production tuning is an env knob, not a redeploy.
	OTelSampleRatio float64
	// PprofEnabled mounts the net/http/pprof handlers on the admin
	// listener (GET /debug/pprof/...). False (the default) leaves them
	// off. Enabled for load-test investigation (ticket 19.x) so worker/API
	// CPU and heap profiles can be captured over the in-network admin
	// port; that port is never published to the host, so the profiles are
	// not reachable from outside the compose network.
	PprofEnabled bool
}

func defaultObsConfig() ObsConfig {
	return ObsConfig{
		MetricsAddr:     "",
		OTelEnabled:     false,
		OTelEndpoint:    DefaultObsOTelEndpoint,
		OTelInsecure:    true,
		OTelSampleRatio: DefaultObsOTelSampleRatio,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable. MetricsAddr and OTelEndpoint pass through opaquely —
// a malformed address fails at listen/dial time with a clearer error than
// any pre-check here.
func (c *ObsConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	applyString(fn, EnvObsMetricsAddr, &c.MetricsAddr)
	errs = applyBool(errs, fn, EnvObsPprofEnabled, &c.PprofEnabled)
	errs = applyBool(errs, fn, EnvObsOTelEnabled, &c.OTelEnabled)
	applyString(fn, EnvObsOTelEndpoint, &c.OTelEndpoint)
	errs = applyBool(errs, fn, EnvObsOTelInsecure, &c.OTelInsecure)
	if raw, ok := lookup(fn, EnvObsOTelSampleRatio); ok {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(f) || f <= 0 || f > 1 {
			errs = append(errs, fmt.Errorf("%s: invalid value %q (want a ratio in (0, 1])", EnvObsOTelSampleRatio, raw))
		} else {
			c.OTelSampleRatio = f
		}
	}
	return errs
}
