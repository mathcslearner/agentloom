package config_test

import (
	"log/slog"
	"strings"
	"testing"

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
