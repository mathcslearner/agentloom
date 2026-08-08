package log_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/obs/log"
)

func jsonLogConfig(level slog.Level) config.LogConfig {
	return config.LogConfig{Level: level, Format: config.LogFormatJSON}
}

// decodeLines asserts that out is exactly one JSON object per line and
// returns the decoded objects.
func decodeLines(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for i, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %d is not a JSON object: %v\nline: %s", i+1, err, line)
		}
		records = append(records, record)
	}
	return records
}

func TestNewEmitsOneJSONObjectPerLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(jsonLogConfig(slog.LevelInfo), &buf)

	logger.Info("step claimed", log.RunID("run-1"), log.StepID("step-a"), log.Attempt(1))
	logger.Warn("step retrying", log.RunID("run-1"), log.StepID("step-a"), log.Attempt(2), log.WorkerID("w-1"), log.TraceID("t-1"))

	records := decodeLines(t, buf.Bytes())
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}

	first := records[0]
	for key, want := range map[string]any{
		"msg":            "step claimed",
		log.FieldRunID:   "run-1",
		log.FieldStepID:  "step-a",
		log.FieldAttempt: float64(1), // JSON numbers decode as float64
	} {
		if got := first[key]; got != want {
			t.Errorf("record[0][%q] = %v, want %v", key, got, want)
		}
	}
	for _, key := range []string{"time", "level"} {
		if _, ok := first[key]; !ok {
			t.Errorf("record[0] missing standard field %q", key)
		}
	}

	second := records[1]
	if got := second[log.FieldWorkerID]; got != "w-1" {
		t.Errorf("record[1][%q] = %v, want %q", log.FieldWorkerID, got, "w-1")
	}
	if got := second[log.FieldTraceID]; got != "t-1" {
		t.Errorf("record[1][%q] = %v, want %q", log.FieldTraceID, got, "t-1")
	}
}

func TestNewFiltersBelowConfiguredLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(jsonLogConfig(slog.LevelInfo), &buf)

	logger.Debug("suppressed")
	if buf.Len() != 0 {
		t.Errorf("debug record emitted at info level: %s", buf.Bytes())
	}

	logger = log.New(jsonLogConfig(slog.LevelDebug), &buf)
	logger.Debug("emitted")
	if buf.Len() == 0 {
		t.Error("debug record not emitted at debug level")
	}
}

func TestNewTextFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatText}, &buf)

	logger.Info("hello", log.RunID("run-1"))
	out := buf.String()
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "run_id=run-1") {
		t.Errorf("text output missing expected key=value pairs: %s", out)
	}
	if json.Valid(buf.Bytes()) {
		t.Errorf("text output unexpectedly valid JSON: %s", out)
	}
}
