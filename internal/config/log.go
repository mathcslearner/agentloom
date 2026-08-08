package config

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// Environment variables read by LogConfig.
const (
	EnvLogLevel  = "AGENTLOOM_LOG_LEVEL"
	EnvLogFormat = "AGENTLOOM_LOG_FORMAT"
	EnvLogSource = "AGENTLOOM_LOG_SOURCE"
)

// LogFormat selects the log output encoding.
type LogFormat string

// Supported log formats. JSON is the default and the production format;
// text exists for local development only.
const (
	LogFormatJSON LogFormat = "json"
	LogFormatText LogFormat = "text"
)

// LogConfig configures the structured logger (see internal/obs/log).
type LogConfig struct {
	// Level is the minimum record level that is emitted.
	Level slog.Level
	// Format is the output encoding, one of LogFormatJSON or LogFormatText.
	Format LogFormat
	// AddSource stamps each record with the file:line of the log call.
	AddSource bool
}

func defaultLogConfig() LogConfig {
	return LogConfig{
		Level:     slog.LevelInfo,
		Format:    LogFormatJSON,
		AddSource: false,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable. Valid overrides are applied even when others fail; the
// caller discards the whole Config on any error.
func (c *LogConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	if raw, ok := lookup(fn, EnvLogLevel); ok {
		if lvl, err := parseLogLevel(raw); err != nil {
			errs = append(errs, err)
		} else {
			c.Level = lvl
		}
	}
	if raw, ok := lookup(fn, EnvLogFormat); ok {
		if f, err := parseLogFormat(raw); err != nil {
			errs = append(errs, err)
		} else {
			c.Format = f
		}
	}
	if raw, ok := lookup(fn, EnvLogSource); ok {
		if b, err := strconv.ParseBool(raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid value %q (want a boolean: true or false)", EnvLogSource, raw))
		} else {
			c.AddSource = b
		}
	}
	return errs
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%s: invalid value %q (want one of debug, info, warn, error)", EnvLogLevel, raw)
	}
}

func parseLogFormat(raw string) (LogFormat, error) {
	switch LogFormat(strings.ToLower(raw)) {
	case LogFormatJSON:
		return LogFormatJSON, nil
	case LogFormatText:
		return LogFormatText, nil
	default:
		return "", fmt.Errorf("%s: invalid value %q (want one of json, text)", EnvLogFormat, raw)
	}
}
