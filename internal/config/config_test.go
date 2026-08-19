package config_test

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/config"
)

// lookupFrom adapts a plain map to a config.LookupFunc.
func lookupFrom(env map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load with empty env: unexpected error: %v", err)
	}
	want := config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatJSON, AddSource: false}
	if cfg.Log != want {
		t.Errorf("default Log config = %+v, want %+v", cfg.Log, want)
	}
	if cfg.Postgres.DSN != config.DefaultPostgresDSN {
		t.Errorf("default Postgres DSN = %q, want %q", cfg.Postgres.DSN, config.DefaultPostgresDSN)
	}
	if cfg.Redis.Addr != config.DefaultRedisAddr {
		t.Errorf("default Redis addr = %q, want %q", cfg.Redis.Addr, config.DefaultRedisAddr)
	}
	wantQueue := config.QueueConfig{
		ConsumerBatch:        config.DefaultQueueConsumerBatch,
		ConsumerBlock:        config.DefaultQueueConsumerBlock,
		LeaseTTL:             config.DefaultQueueLeaseTTL,
		PoisonThreshold:      config.DefaultQueuePoisonThreshold,
		JanitorInterval:      config.DefaultQueueJanitorInterval,
		JanitorIdleThreshold: config.DefaultQueueJanitorIdleThreshold,
		PromoterTick:         config.DefaultQueuePromoterTick,
		TrimInterval:         config.DefaultQueueTrimInterval,
		// HeartbeatInterval and ReclaimInterval default to zero: derived
		// from LeaseTTL (TTL/3 and TTL/2) by internal/queue.
	}
	if cfg.Queue != wantQueue {
		t.Errorf("default Queue config = %+v, want %+v", cfg.Queue, wantQueue)
	}
	wantWorker := config.WorkerConfig{
		HealthInterval:        config.DefaultWorkerHealthInterval,
		DispatchInterval:      config.DefaultWorkerDispatchInterval,
		DispatchBatch:         config.DefaultWorkerDispatchBatch,
		ReconcileInterval:     config.DefaultWorkerReconcileInterval,
		ReconcileReadyStale:   config.DefaultWorkerReconcileReadyStale,
		ReconcileRunningStale: config.DefaultWorkerReconcileRunningStale,
		ReconcileRetryStale:   config.DefaultWorkerReconcileRetryStale,
		ReconcileLimit:        config.DefaultWorkerReconcileLimit,
		CancelPollInterval:    config.DefaultWorkerCancelPollInterval,
		DrainTimeout:          config.DefaultWorkerDrainTimeout,
		MetricsSampleInterval: config.DefaultWorkerMetricsSampleInterval,
		EffectsStrict:         true,
		StepLogEnabled:        true,
		StepLogLevel:          slog.LevelInfo,
		StepLogCap:            config.DefaultWorkerStepLogCap,
		StepLogBuffer:         config.DefaultWorkerStepLogBuffer,
		StepLogMaxLineBytes:   config.DefaultWorkerStepLogMaxLineBytes,
		StepLogFlushInterval:  config.DefaultWorkerStepLogFlushInterval,
		StepLogFlushBatch:     config.DefaultWorkerStepLogFlushBatch,
	}
	if cfg.Worker != wantWorker {
		t.Errorf("default Worker config = %+v, want %+v", cfg.Worker, wantWorker)
	}
	wantAPI := config.APIConfig{
		Addr:            config.DefaultAPIAddr,
		ReadTimeout:     config.DefaultAPIReadTimeout,
		WriteTimeout:    config.DefaultAPIWriteTimeout,
		IdleTimeout:     config.DefaultAPIIdleTimeout,
		ShutdownTimeout: config.DefaultAPIShutdownTimeout,
		WSTicketTTL:     config.DefaultAPIWSTicketTTL,
		RateLimit:       defaultRateLimit(),
	}
	if !reflect.DeepEqual(cfg.API, wantAPI) {
		t.Errorf("default API config = %+v, want %+v", cfg.API, wantAPI)
	}
	wantObs := config.ObsConfig{
		MetricsAddr:     "", // no listener — telemetry off by default (ADR-008)
		OTelEnabled:     false,
		OTelEndpoint:    config.DefaultObsOTelEndpoint,
		OTelInsecure:    true,
		OTelSampleRatio: config.DefaultObsOTelSampleRatio,
	}
	if cfg.Obs != wantObs {
		t.Errorf("default Obs config = %+v, want %+v", cfg.Obs, wantObs)
	}
}

// defaultRateLimit is the expected default rate-limit config (ticket 6.4),
// spelled out so a default drift fails a test.
func defaultRateLimit() config.APIRateLimitConfig {
	return config.APIRateLimitConfig{
		Enabled:   true,
		KeyPrefix: config.DefaultAPIRateLimitKeyPrefix,
		Submit:    config.APIRateLimitClass{Capacity: config.DefaultAPIRateLimitSubmitCapacity, RefillPerSec: config.DefaultAPIRateLimitSubmitRefill},
		Read:      config.APIRateLimitClass{Capacity: config.DefaultAPIRateLimitReadCapacity, RefillPerSec: config.DefaultAPIRateLimitReadRefill},
		Admin:     config.APIRateLimitClass{Capacity: config.DefaultAPIRateLimitAdminCapacity, RefillPerSec: config.DefaultAPIRateLimitAdminRefill},
		Global:    config.APIRateLimitClass{Capacity: config.DefaultAPIRateLimitGlobalCapacity, RefillPerSec: config.DefaultAPIRateLimitGlobalRefill},
	}
}

func TestLoadAPIOverrides(t *testing.T) {
	t.Parallel()

	// Constructed, not a literal: no sk_-shaped string may be committed
	// verbatim (the CI secret grep would flag it, by design).
	rootKey := "sk_" + strings.Repeat("a", 43)
	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvAPIAddr:            "127.0.0.1:9090",
		config.EnvAPIReadTimeout:     "5s",
		config.EnvAPIWriteTimeout:    "10s",
		config.EnvAPIIdleTimeout:     "1m",
		config.EnvAPIShutdownTimeout: "3s",
		config.EnvAPIRootKey:         rootKey,
		config.EnvAPIWSTicketSecret:  "shhh",
		config.EnvAPIWSTicketTTL:     "2m",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	want := config.APIConfig{
		Addr:            "127.0.0.1:9090",
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    10 * time.Second,
		IdleTimeout:     time.Minute,
		ShutdownTimeout: 3 * time.Second,
		RootKey:         rootKey,
		WSTicketSecret:  "shhh",
		WSTicketTTL:     2 * time.Minute,
		RateLimit:       defaultRateLimit(),
	}
	if !reflect.DeepEqual(cfg.API, want) {
		t.Errorf("API config = %+v, want %+v", cfg.API, want)
	}
}

func TestLoadAPIWSOrigins(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvAPIWSOrigins: "localhost:*, dash.example.com ,",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	// Comma-separated, whitespace-trimmed, empty entries dropped (ticket 18.1).
	want := []string{"localhost:*", "dash.example.com"}
	if !reflect.DeepEqual(cfg.API.WSOrigins, want) {
		t.Errorf("WSOrigins = %#v, want %#v", cfg.API.WSOrigins, want)
	}
	// Unset ⇒ nil (same-host-only, the 16.3/16.4 default).
	cfg, err = config.Load(lookupFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.API.WSOrigins != nil {
		t.Errorf("default WSOrigins = %#v, want nil", cfg.API.WSOrigins)
	}
}

func TestLoadAPIRateLimitOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvAPIRateLimitEnabled:        "false",
		config.EnvAPIRateLimitKeyPrefix:      "test:rl",
		config.EnvAPIRateLimitSubmitCapacity: "5",
		config.EnvAPIRateLimitSubmitRefill:   "2.5",
		config.EnvAPIRateLimitReadCapacity:   "7",
		config.EnvAPIRateLimitReadRefill:     "3",
		config.EnvAPIRateLimitAdminCapacity:  "2",
		config.EnvAPIRateLimitAdminRefill:    "0.5",
		config.EnvAPIRateLimitGlobalCapacity: "50",
		config.EnvAPIRateLimitGlobalRefill:   "25",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	want := config.APIRateLimitConfig{
		Enabled:   false,
		KeyPrefix: "test:rl",
		Submit:    config.APIRateLimitClass{Capacity: 5, RefillPerSec: 2.5},
		Read:      config.APIRateLimitClass{Capacity: 7, RefillPerSec: 3},
		Admin:     config.APIRateLimitClass{Capacity: 2, RefillPerSec: 0.5},
		Global:    config.APIRateLimitClass{Capacity: 50, RefillPerSec: 25},
	}
	if cfg.API.RateLimit != want {
		t.Errorf("RateLimit config = %+v, want %+v", cfg.API.RateLimit, want)
	}
}

func TestLoadAPIRateLimitInvalidValues(t *testing.T) {
	t.Parallel()

	bad := map[string]string{
		config.EnvAPIRateLimitEnabled:        "definitely",
		config.EnvAPIRateLimitSubmitCapacity: "0",
		config.EnvAPIRateLimitSubmitRefill:   "-1",
		config.EnvAPIRateLimitReadCapacity:   "many",
		config.EnvAPIRateLimitReadRefill:     "NaN",
		config.EnvAPIRateLimitAdminCapacity:  "-3",
		config.EnvAPIRateLimitAdminRefill:    "+Inf",
		config.EnvAPIRateLimitGlobalCapacity: "1.5",
		config.EnvAPIRateLimitGlobalRefill:   "0",
	}
	_, err := config.Load(lookupFrom(bad))
	if err == nil {
		t.Fatal("Load with invalid rate-limit values: want error, got nil")
	}
	for env := range bad {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not mention %s", err, env)
		}
	}
}

func TestLoadAPIInvalidValues(t *testing.T) {
	t.Parallel()

	_, err := config.Load(lookupFrom(map[string]string{
		config.EnvAPIReadTimeout:     "-1s",
		config.EnvAPIShutdownTimeout: "later",
	}))
	if err == nil {
		t.Fatal("Load with invalid API values: want error, got nil")
	}
	for _, env := range []string{config.EnvAPIReadTimeout, config.EnvAPIShutdownTimeout} {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not mention %s", err, env)
		}
	}
}

func TestLoadWorkerOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvWorkerHealthInterval:        "15s",
		config.EnvWorkerDispatchInterval:      "200ms",
		config.EnvWorkerDispatchBatch:         "16",
		config.EnvWorkerReconcileInterval:     "10s",
		config.EnvWorkerReconcileReadyStale:   "20s",
		config.EnvWorkerReconcileRunningStale: "2m",
		config.EnvWorkerReconcileRetryStale:   "30s",
		config.EnvWorkerReconcileLimit:        "50",
		config.EnvWorkerCancelPollInterval:    "3s",
		config.EnvWorkerDrainTimeout:          "40s",
		config.EnvWorkerMetricsSampleInterval: "7s",
		config.EnvWorkerEffectsStrict:         "false",
		config.EnvWorkerTestExecutors:         "true",
		config.EnvWorkerStepLogEnabled:        "false",
		config.EnvWorkerStepLogLevel:          "debug",
		config.EnvWorkerStepLogCap:            "200",
		config.EnvWorkerStepLogBuffer:         "1024",
		config.EnvWorkerStepLogMaxLineBytes:   "4096",
		config.EnvWorkerStepLogFlushInterval:  "250ms",
		config.EnvWorkerStepLogFlushBatch:     "64",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	want := config.WorkerConfig{
		HealthInterval:        15 * time.Second,
		DispatchInterval:      200 * time.Millisecond,
		DispatchBatch:         16,
		ReconcileInterval:     10 * time.Second,
		ReconcileReadyStale:   20 * time.Second,
		ReconcileRunningStale: 2 * time.Minute,
		ReconcileRetryStale:   30 * time.Second,
		ReconcileLimit:        50,
		CancelPollInterval:    3 * time.Second,
		DrainTimeout:          40 * time.Second,
		MetricsSampleInterval: 7 * time.Second,
		EffectsStrict:         false,
		TestExecutors:         true,
		StepLogEnabled:        false,
		StepLogLevel:          slog.LevelDebug,
		StepLogCap:            200,
		StepLogBuffer:         1024,
		StepLogMaxLineBytes:   4096,
		StepLogFlushInterval:  250 * time.Millisecond,
		StepLogFlushBatch:     64,
	}
	if cfg.Worker != want {
		t.Errorf("Worker config = %+v, want %+v", cfg.Worker, want)
	}
}

func TestLoadWorkerInvalidValues(t *testing.T) {
	t.Parallel()

	_, err := config.Load(lookupFrom(map[string]string{
		config.EnvWorkerDispatchBatch:    "0",
		config.EnvWorkerReconcileLimit:   "-3",
		config.EnvWorkerDispatchInterval: "soon",
		config.EnvWorkerDrainTimeout:     "0s",
		config.EnvWorkerEffectsStrict:    "loudly",
		config.EnvWorkerTestExecutors:    "maybe",
		config.EnvWorkerStepLogLevel:     "verbose",
		config.EnvWorkerStepLogCap:       "0",
	}))
	if err == nil {
		t.Fatal("Load with invalid worker values: want error, got nil")
	}
	for _, env := range []string{
		config.EnvWorkerDispatchBatch,
		config.EnvWorkerReconcileLimit,
		config.EnvWorkerDispatchInterval,
		config.EnvWorkerDrainTimeout,
		config.EnvWorkerEffectsStrict,
		config.EnvWorkerTestExecutors,
		config.EnvWorkerStepLogLevel,
		config.EnvWorkerStepLogCap,
	} {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not mention %s", err, env)
		}
	}
}

func TestLoadObsOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvObsMetricsAddr:     "127.0.0.1:9090",
		config.EnvObsPprofEnabled:    "true",
		config.EnvObsOTelEnabled:     "true",
		config.EnvObsOTelEndpoint:    "jaeger:4317",
		config.EnvObsOTelInsecure:    "false",
		config.EnvObsOTelSampleRatio: "0.25",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	want := config.ObsConfig{
		MetricsAddr:     "127.0.0.1:9090",
		PprofEnabled:    true,
		OTelEnabled:     true,
		OTelEndpoint:    "jaeger:4317",
		OTelInsecure:    false,
		OTelSampleRatio: 0.25,
	}
	if cfg.Obs != want {
		t.Errorf("Obs config = %+v, want %+v", cfg.Obs, want)
	}
}

func TestLoadObsInvalidValues(t *testing.T) {
	t.Parallel()

	for _, ratio := range []string{"0", "-0.5", "1.5", "NaN", "lots"} {
		bad := map[string]string{
			config.EnvObsOTelEnabled:     "definitely",
			config.EnvObsOTelInsecure:    "maybe",
			config.EnvObsOTelSampleRatio: ratio,
		}
		_, err := config.Load(lookupFrom(bad))
		if err == nil {
			t.Fatalf("Load with invalid obs values (ratio %q): want error, got nil", ratio)
		}
		for env := range bad {
			if !strings.Contains(err.Error(), env) {
				t.Errorf("error %q does not mention %s", err, env)
			}
		}
	}
}

func TestLoadToolsOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvToolsHTTPAllowlist:        " api.example.com , news.example.com:8443 ,",
		config.EnvToolsHTTPTimeout:          "10s",
		config.EnvToolsHTTPMaxResponseBytes: "2048",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	// Allowlist splits on commas, trims whitespace, and drops blanks.
	wantHosts := []string{"api.example.com", "news.example.com:8443"}
	if len(cfg.Tools.HTTPAllowlist) != len(wantHosts) {
		t.Fatalf("allowlist = %v, want %v", cfg.Tools.HTTPAllowlist, wantHosts)
	}
	for i, h := range wantHosts {
		if cfg.Tools.HTTPAllowlist[i] != h {
			t.Errorf("allowlist[%d] = %q, want %q", i, cfg.Tools.HTTPAllowlist[i], h)
		}
	}
	if cfg.Tools.HTTPTimeout != 10*time.Second {
		t.Errorf("timeout = %s, want 10s", cfg.Tools.HTTPTimeout)
	}
	if cfg.Tools.HTTPMaxResponseBytes != 2048 {
		t.Errorf("max response bytes = %d, want 2048", cfg.Tools.HTTPMaxResponseBytes)
	}
}

func TestLoadToolsDefaultsDenyAll(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.Tools.HTTPAllowlist != nil {
		t.Errorf("default allowlist = %v, want nil (deny all)", cfg.Tools.HTTPAllowlist)
	}
	if cfg.Tools.HTTPTimeout != 30*time.Second {
		t.Errorf("default timeout = %s, want 30s", cfg.Tools.HTTPTimeout)
	}
}

func TestLoadToolsInvalidValues(t *testing.T) {
	t.Parallel()

	bad := map[string]string{
		config.EnvToolsHTTPTimeout:          "nope",
		config.EnvToolsHTTPMaxResponseBytes: "-5",
	}
	_, err := config.Load(lookupFrom(bad))
	if err == nil {
		t.Fatal("Load with invalid tools values: want error, got nil")
	}
	for env := range bad {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not mention %s", err, env)
		}
	}
}

func TestLoadResourcesOverrides(t *testing.T) {
	t.Parallel()

	// Default: neither source set — the worker treats every resource as
	// unlimited (ADR-010, ticket 9.1); the throttle knobs and key prefix
	// carry their 9.2 defaults.
	cfg, err := config.Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load with empty env: %v", err)
	}
	if cfg.Resources.Inline != "" || cfg.Resources.File != "" {
		t.Errorf("default Resources sources = %+v, want both empty", cfg.Resources)
	}
	if cfg.Resources.KeyPrefix != config.DefaultResourcesKeyPrefix ||
		cfg.Resources.ThrottleFloor != config.DefaultResourcesThrottleFloor ||
		cfg.Resources.ThrottleCap != config.DefaultResourcesThrottleCap ||
		cfg.Resources.ThrottleJitterFrac != config.DefaultResourcesThrottleJitterFrac {
		t.Errorf("default Resources knobs = %+v, want the 9.2 defaults", cfg.Resources)
	}

	// Inline source.
	inline := `{"resources": [{"name": "anthropic:claude-sonnet-5", "requests": {"per_minute": 60}}]}`
	cfg, err = config.Load(lookupFrom(map[string]string{config.EnvResources: inline}))
	if err != nil {
		t.Fatalf("Load inline resources: %v", err)
	}
	if cfg.Resources.Inline != inline || cfg.Resources.File != "" {
		t.Errorf("Resources = %+v, want inline set only", cfg.Resources)
	}

	// File source.
	cfg, err = config.Load(lookupFrom(map[string]string{config.EnvResourcesFile: "/etc/agentloom/resources.json"}))
	if err != nil {
		t.Fatalf("Load file resources: %v", err)
	}
	if cfg.Resources.File != "/etc/agentloom/resources.json" || cfg.Resources.Inline != "" {
		t.Errorf("Resources = %+v, want file set only", cfg.Resources)
	}

	// Throttle knobs + key prefix.
	cfg, err = config.Load(lookupFrom(map[string]string{
		config.EnvResourcesKeyPrefix: "rl:test",
		config.EnvResourcesThrFloor:  "250ms",
		config.EnvResourcesThrCap:    "2m",
		config.EnvResourcesThrJitter: "0.5",
	}))
	if err != nil {
		t.Fatalf("Load throttle knobs: %v", err)
	}
	if cfg.Resources.KeyPrefix != "rl:test" ||
		cfg.Resources.ThrottleFloor != 250*time.Millisecond ||
		cfg.Resources.ThrottleCap != 2*time.Minute ||
		cfg.Resources.ThrottleJitterFrac != 0.5 {
		t.Errorf("Resources knobs = %+v, want the overrides", cfg.Resources)
	}
}

func TestLoadResourcesInvalidValues(t *testing.T) {
	t.Parallel()

	for name, env := range map[string]map[string]string{
		"jitter >= 1":     {config.EnvResourcesThrJitter: "1"},
		"jitter negative": {config.EnvResourcesThrJitter: "-0.1"},
		"jitter unparse":  {config.EnvResourcesThrJitter: "half"},
		"floor above cap": {config.EnvResourcesThrFloor: "10m", config.EnvResourcesThrCap: "1m"},
		"bad floor":       {config.EnvResourcesThrFloor: "nope"},
	} {
		if _, err := config.Load(lookupFrom(env)); err == nil {
			t.Errorf("%s: Load want error, got nil", name)
		}
	}
}

func TestLoadResourcesMutualExclusion(t *testing.T) {
	t.Parallel()

	_, err := config.Load(lookupFrom(map[string]string{
		config.EnvResources:     `{"resources": []}`,
		config.EnvResourcesFile: "/etc/agentloom/resources.json",
	}))
	if err == nil {
		t.Fatal("Load with both resource sources: want error, got nil")
	}
	for _, env := range []string{config.EnvResources, config.EnvResourcesFile} {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not mention %s", err, env)
		}
	}
}

func TestLoadCostDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	// Defaults: no override source, estimate policy.
	cfg, err := config.Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Cost.Inline != "" || cfg.Cost.File != "" {
		t.Errorf("default Cost sources = %+v, want both empty", cfg.Cost)
	}
	if cfg.Cost.UnknownModelPolicy != config.DefaultUnknownModelPolicy {
		t.Errorf("default Cost policy = %q, want %q", cfg.Cost.UnknownModelPolicy, config.DefaultUnknownModelPolicy)
	}

	// Overrides.
	cfg, err = config.Load(lookupFrom(map[string]string{
		config.EnvPricingFile:         "/etc/agentloom/pricing.json",
		config.EnvCostUnknownModelPol: config.UnknownModelPolicyFail,
	}))
	if err != nil {
		t.Fatalf("Load overrides: %v", err)
	}
	if cfg.Cost.File != "/etc/agentloom/pricing.json" || cfg.Cost.UnknownModelPolicy != config.UnknownModelPolicyFail {
		t.Errorf("Cost = %+v, want file + fail policy", cfg.Cost)
	}
}

func TestLoadCostInvalid(t *testing.T) {
	t.Parallel()

	// Bad policy value.
	if _, err := config.Load(lookupFrom(map[string]string{config.EnvCostUnknownModelPol: "guess"})); err == nil {
		t.Error("Load with bad policy: want error, got nil")
	}

	// Both override sources set.
	_, err := config.Load(lookupFrom(map[string]string{
		config.EnvPricing:     `{"schema_version": 1}`,
		config.EnvPricingFile: "/etc/agentloom/pricing.json",
	}))
	if err == nil {
		t.Fatal("Load with both pricing sources: want error, got nil")
	}
	for _, env := range []string{config.EnvPricing, config.EnvPricingFile} {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not mention %s", err, env)
		}
	}
}

func TestLoadEventsDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	// Defaults.
	cfg, err := config.Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	wantEvents := config.EventsConfig{
		PubSubEnabled:  true,
		ChannelPrefix:  config.DefaultEventsChannelPrefix,
		PublishBuffer:  config.DefaultEventsPublishBuffer,
		PublishTimeout: config.DefaultEventsPublishTimeout,
	}
	if cfg.Events != wantEvents {
		t.Errorf("default Events config = %+v, want %+v", cfg.Events, wantEvents)
	}

	// Overrides.
	cfg, err = config.Load(lookupFrom(map[string]string{
		config.EnvEventsPubSubEnabled:  "false",
		config.EnvEventsChannelPrefix:  "custom",
		config.EnvEventsPublishBuffer:  "64",
		config.EnvEventsPublishTimeout: "5s",
	}))
	if err != nil {
		t.Fatalf("Load overrides: %v", err)
	}
	if cfg.Events.PubSubEnabled || cfg.Events.ChannelPrefix != "custom" ||
		cfg.Events.PublishBuffer != 64 || cfg.Events.PublishTimeout != 5*time.Second {
		t.Errorf("Events overrides = %+v", cfg.Events)
	}
}

func TestLoadEventsInvalid(t *testing.T) {
	t.Parallel()
	for _, env := range []string{config.EnvEventsPublishBuffer, config.EnvEventsPublishTimeout} {
		if _, err := config.Load(lookupFrom(map[string]string{env: "-1"})); err == nil {
			t.Errorf("Load with %s=-1: want error, got nil", env)
		}
	}
}

func TestLoadRedisAddrOverride(t *testing.T) {
	t.Parallel()

	const addr = "redis.internal:6380"
	cfg, err := config.Load(lookupFrom(map[string]string{config.EnvRedisAddr: addr}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.Redis.Addr != addr {
		t.Errorf("Redis addr = %q, want %q", cfg.Redis.Addr, addr)
	}
}

func TestLoadLLMOverrides(t *testing.T) {
	t.Parallel()

	// Empty env: the provider is unconfigured, not a load error (a
	// worker running no llm steps boots keyless).
	cfg, err := config.Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load with empty env: unexpected error: %v", err)
	}
	if cfg.LLM.AnthropicAPIKey != "" {
		t.Errorf("default Anthropic key = %q, want empty", cfg.LLM.AnthropicAPIKey)
	}
	if cfg.LLM.OpenAIAPIKey != "" {
		t.Errorf("default OpenAI key = %q, want empty", cfg.LLM.OpenAIAPIKey)
	}

	// Each provider key overrides independently (8.4: either provider may
	// be configured without the other).
	anthKey := "sk-ant-" + strings.Repeat("x", 8) // constructed, never a literal (6.1 secret grep)
	oaiKey := "sk-" + strings.Repeat("y", 8)      // constructed, never a literal (6.1 secret grep)
	cfg, err = config.Load(lookupFrom(map[string]string{
		config.EnvAnthropicAPIKey: anthKey,
		config.EnvOpenAIAPIKey:    oaiKey,
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.LLM.AnthropicAPIKey != anthKey {
		t.Errorf("Anthropic key = %q, want %q", cfg.LLM.AnthropicAPIKey, anthKey)
	}
	if cfg.LLM.OpenAIAPIKey != oaiKey {
		t.Errorf("OpenAI key = %q, want %q", cfg.LLM.OpenAIAPIKey, oaiKey)
	}

	// OpenAI alone, Anthropic absent.
	cfg, err = config.Load(lookupFrom(map[string]string{config.EnvOpenAIAPIKey: oaiKey}))
	if err != nil {
		t.Fatalf("Load OpenAI-only: unexpected error: %v", err)
	}
	if cfg.LLM.AnthropicAPIKey != "" || cfg.LLM.OpenAIAPIKey != oaiKey {
		t.Errorf("OpenAI-only load = {anthropic %q, openai %q}", cfg.LLM.AnthropicAPIKey, cfg.LLM.OpenAIAPIKey)
	}

	// Mock toggle (ticket 8.5): off by default, on with a boolean env, and
	// a non-boolean value is a load error.
	cfg, err = config.Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.MockEnabled {
		t.Error("mock enabled by default, want off")
	}
	cfg, err = config.Load(lookupFrom(map[string]string{config.EnvLLMMockEnabled: "true"}))
	if err != nil {
		t.Fatalf("Load with mock enabled: %v", err)
	}
	if !cfg.LLM.MockEnabled {
		t.Error("AGENTLOOM_LLM_MOCK_ENABLED=true did not enable the mock")
	}
	if _, err := config.Load(lookupFrom(map[string]string{config.EnvLLMMockEnabled: "maybe"})); err == nil {
		t.Error("non-boolean mock toggle: want a load error, got nil")
	}

	// Mock script (ticket 14.5): inline and file sources carried opaquely,
	// mutually exclusive.
	cfg, err = config.Load(lookupFrom(map[string]string{config.EnvLLMMockScript: `{"rules":[]}`}))
	if err != nil {
		t.Fatalf("Load with mock script: %v", err)
	}
	if cfg.LLM.MockScript != `{"rules":[]}` {
		t.Errorf("mock script = %q, want the inline JSON", cfg.LLM.MockScript)
	}
	cfg, err = config.Load(lookupFrom(map[string]string{config.EnvLLMMockScriptFile: "/etc/mock.json"}))
	if err != nil {
		t.Fatalf("Load with mock script file: %v", err)
	}
	if cfg.LLM.MockScriptFile != "/etc/mock.json" {
		t.Errorf("mock script file = %q, want the path", cfg.LLM.MockScriptFile)
	}
	if _, err := config.Load(lookupFrom(map[string]string{
		config.EnvLLMMockScript:     `{"rules":[]}`,
		config.EnvLLMMockScriptFile: "/etc/mock.json",
	})); err == nil {
		t.Error("both mock script sources set: want a load error, got nil")
	}
}

func TestLoadPostgresDSNOverride(t *testing.T) {
	t.Parallel()

	const dsn = "postgres://other:other@dbhost:5433/otherdb" //nolint:gosec // G101: made-up test value
	cfg, err := config.Load(lookupFrom(map[string]string{config.EnvPostgresDSN: dsn}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.Postgres.DSN != dsn {
		t.Errorf("Postgres DSN = %q, want %q", cfg.Postgres.DSN, dsn)
	}
}

func TestLoadQueueOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvQueueStream:               "steps:ready:test",
		config.EnvQueueGroup:                "workers:test",
		config.EnvQueueDelayedKey:           "sched:delayed:test",
		config.EnvQueueConsumerBatch:        "32",
		config.EnvQueueConsumerBlock:        "250ms",
		config.EnvQueueLeaseTTL:             "10s",
		config.EnvQueueHeartbeatInterval:    "2s",
		config.EnvQueueReclaimInterval:      "4s",
		config.EnvQueuePoisonThreshold:      "3",
		config.EnvQueueJanitorInterval:      "1m",
		config.EnvQueueJanitorIdleThreshold: "30m",
		config.EnvQueuePromoterTick:         "250ms",
		config.EnvQueueTrimInterval:         "45s",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	want := config.QueueConfig{
		Stream:               "steps:ready:test",
		Group:                "workers:test",
		DelayedKey:           "sched:delayed:test",
		ConsumerBatch:        32,
		ConsumerBlock:        250 * time.Millisecond,
		LeaseTTL:             10 * time.Second,
		HeartbeatInterval:    2 * time.Second,
		ReclaimInterval:      4 * time.Second,
		PoisonThreshold:      3,
		JanitorInterval:      time.Minute,
		JanitorIdleThreshold: 30 * time.Minute,
		PromoterTick:         250 * time.Millisecond,
		TrimInterval:         45 * time.Second,
	}
	if cfg.Queue != want {
		t.Errorf("Queue config = %+v, want %+v", cfg.Queue, want)
	}
}

func TestLoadEnvOverridesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvLogLevel:  "debug",
		config.EnvLogFormat: "text",
		config.EnvLogSource: "true",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	want := config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatText, AddSource: true}
	if cfg.Log != want {
		t.Errorf("Log config = %+v, want %+v", cfg.Log, want)
	}
}

func TestLoadLevelValues(t *testing.T) {
	t.Parallel()

	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"WARN":  slog.LevelWarn, // case-insensitive
	}
	for raw, want := range cases {
		cfg, err := config.Load(lookupFrom(map[string]string{config.EnvLogLevel: raw}))
		if err != nil {
			t.Errorf("Load with %s=%q: unexpected error: %v", config.EnvLogLevel, raw, err)
			continue
		}
		if cfg.Log.Level != want {
			t.Errorf("Load with %s=%q: Level = %v, want %v", config.EnvLogLevel, raw, cfg.Log.Level, want)
		}
	}
}

func TestLoadEmptyValueKeepsDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{config.EnvLogLevel: ""}))
	if err != nil {
		t.Fatalf("Load with empty %s: unexpected error: %v", config.EnvLogLevel, err)
	}
	if cfg.Log.Level != slog.LevelInfo {
		t.Errorf("Level = %v, want default %v", cfg.Log.Level, slog.LevelInfo)
	}
}

func TestLoadInvalidValuesAreActionable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key, value string
		wantInErr  []string
	}{
		{config.EnvLogLevel, "verbose", []string{config.EnvLogLevel, `"verbose"`, "debug, info, warn, error"}},
		{config.EnvLogFormat, "yaml", []string{config.EnvLogFormat, `"yaml"`, "json, text"}},
		{config.EnvLogSource, "yep", []string{config.EnvLogSource, `"yep"`, "true or false"}},
		{config.EnvQueueConsumerBatch, "lots", []string{config.EnvQueueConsumerBatch, `"lots"`, "positive integer"}},
		{config.EnvQueueConsumerBatch, "0", []string{config.EnvQueueConsumerBatch, `"0"`, "positive integer"}},
		{config.EnvQueueConsumerBlock, "soon", []string{config.EnvQueueConsumerBlock, `"soon"`, "positive Go duration"}},
		{config.EnvQueueConsumerBlock, "-5s", []string{config.EnvQueueConsumerBlock, `"-5s"`, "positive Go duration"}},
		{config.EnvQueueLeaseTTL, "0", []string{config.EnvQueueLeaseTTL, `"0"`, "positive Go duration"}},
		{config.EnvQueueLeaseTTL, "forever", []string{config.EnvQueueLeaseTTL, `"forever"`, "positive Go duration"}},
		{config.EnvQueueHeartbeatInterval, "-1s", []string{config.EnvQueueHeartbeatInterval, `"-1s"`, "positive Go duration"}},
		{config.EnvQueueReclaimInterval, "later", []string{config.EnvQueueReclaimInterval, `"later"`, "positive Go duration"}},
		{config.EnvQueuePoisonThreshold, "0", []string{config.EnvQueuePoisonThreshold, `"0"`, "positive integer"}},
		{config.EnvQueuePoisonThreshold, "many", []string{config.EnvQueuePoisonThreshold, `"many"`, "positive integer"}},
		{config.EnvQueueJanitorInterval, "0s", []string{config.EnvQueueJanitorInterval, `"0s"`, "positive Go duration"}},
		{config.EnvQueueJanitorIdleThreshold, "never", []string{config.EnvQueueJanitorIdleThreshold, `"never"`, "positive Go duration"}},
		{config.EnvQueuePromoterTick, "0s", []string{config.EnvQueuePromoterTick, `"0s"`, "positive Go duration"}},
		{config.EnvQueuePromoterTick, "often", []string{config.EnvQueuePromoterTick, `"often"`, "positive Go duration"}},
		{config.EnvQueueTrimInterval, "0s", []string{config.EnvQueueTrimInterval, `"0s"`, "positive Go duration"}},
		{config.EnvQueueTrimInterval, "rarely", []string{config.EnvQueueTrimInterval, `"rarely"`, "positive Go duration"}},
		{config.EnvWorkerHealthInterval, "0s", []string{config.EnvWorkerHealthInterval, `"0s"`, "positive Go duration"}},
		{config.EnvWorkerHealthInterval, "hourly", []string{config.EnvWorkerHealthInterval, `"hourly"`, "positive Go duration"}},
	}
	for _, tc := range cases {
		_, err := config.Load(lookupFrom(map[string]string{tc.key: tc.value}))
		if err == nil {
			t.Errorf("Load with %s=%q: want error, got nil", tc.key, tc.value)
			continue
		}
		for _, want := range tc.wantInErr {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Load with %s=%q: error %q does not mention %q", tc.key, tc.value, err, want)
			}
		}
	}
}

func TestLoadReportsAllErrorsAtOnce(t *testing.T) {
	t.Parallel()

	_, err := config.Load(lookupFrom(map[string]string{
		config.EnvLogLevel:  "verbose",
		config.EnvLogFormat: "yaml",
		config.EnvLogSource: "yep",
	}))
	if err == nil {
		t.Fatal("Load with three invalid vars: want error, got nil")
	}
	for _, key := range []string{config.EnvLogLevel, config.EnvLogFormat, config.EnvLogSource} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("joined error %q does not mention %s", err, key)
		}
	}
}

func TestLoadCacheDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if !cfg.Cache.Enabled {
		t.Error("cache should be enabled by default")
	}
	if cfg.Cache.KeyPrefix != config.DefaultCacheKeyPrefix {
		t.Errorf("key prefix = %q, want %q", cfg.Cache.KeyPrefix, config.DefaultCacheKeyPrefix)
	}
	if cfg.Cache.DefaultTTL != config.DefaultCacheDefaultTTL {
		t.Errorf("default ttl = %s, want %s", cfg.Cache.DefaultTTL, config.DefaultCacheDefaultTTL)
	}
	if cfg.Cache.MaxValueBytes != config.DefaultCacheMaxValueBytes {
		t.Errorf("max value bytes = %d, want %d", cfg.Cache.MaxValueBytes, config.DefaultCacheMaxValueBytes)
	}
}

func TestLoadCacheOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvCacheEnabled:       "false",
		config.EnvCacheKeyPrefix:     "cache:test",
		config.EnvCacheDefaultTTL:    "6h",
		config.EnvCacheMaxValueBytes: "4096",
	}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.Cache.Enabled {
		t.Error("cache should be disabled")
	}
	if cfg.Cache.KeyPrefix != "cache:test" {
		t.Errorf("key prefix = %q, want cache:test", cfg.Cache.KeyPrefix)
	}
	if cfg.Cache.DefaultTTL != 6*time.Hour {
		t.Errorf("default ttl = %s, want 6h", cfg.Cache.DefaultTTL)
	}
	if cfg.Cache.MaxValueBytes != 4096 {
		t.Errorf("max value bytes = %d, want 4096", cfg.Cache.MaxValueBytes)
	}
}

func TestLoadCacheInvalidValues(t *testing.T) {
	t.Parallel()

	// A non-positive TTL, a bad byte count, and a TTL over the ceiling are
	// each reported.
	bad := map[string]string{
		config.EnvCacheDefaultTTL:    "0s",
		config.EnvCacheMaxValueBytes: "-1",
	}
	_, err := config.Load(lookupFrom(bad))
	if err == nil {
		t.Fatal("Load with invalid cache values: want error, got nil")
	}
	for env := range bad {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not mention %s", err, env)
		}
	}

	_, err = config.Load(lookupFrom(map[string]string{config.EnvCacheDefaultTTL: "9000h"}))
	if err == nil || !strings.Contains(err.Error(), config.EnvCacheDefaultTTL) {
		t.Errorf("Load with over-ceiling TTL = %v, want an error mentioning %s", err, config.EnvCacheDefaultTTL)
	}
}
