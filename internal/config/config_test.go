package config_test

import (
	"log/slog"
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
		ReconcileLimit:        config.DefaultWorkerReconcileLimit,
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
	}
	if cfg.API != wantAPI {
		t.Errorf("default API config = %+v, want %+v", cfg.API, wantAPI)
	}
}

func TestLoadAPIOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(lookupFrom(map[string]string{
		config.EnvAPIAddr:            "127.0.0.1:9090",
		config.EnvAPIReadTimeout:     "5s",
		config.EnvAPIWriteTimeout:    "10s",
		config.EnvAPIIdleTimeout:     "1m",
		config.EnvAPIShutdownTimeout: "3s",
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
	}
	if cfg.API != want {
		t.Errorf("API config = %+v, want %+v", cfg.API, want)
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
		config.EnvWorkerReconcileLimit:        "50",
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
		ReconcileLimit:        50,
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
	}))
	if err == nil {
		t.Fatal("Load with invalid worker values: want error, got nil")
	}
	for _, env := range []string{
		config.EnvWorkerDispatchBatch,
		config.EnvWorkerReconcileLimit,
		config.EnvWorkerDispatchInterval,
	} {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("error %q does not mention %s", err, env)
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
