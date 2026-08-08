package log_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

func TestFromIsNilSafe(t *testing.T) {
	t.Parallel()

	var nilCtx context.Context
	if log.From(nilCtx) == nil {
		t.Error("From(nil) returned nil")
	}
	if log.From(context.Background()) == nil {
		t.Error("From(context.Background()) returned nil")
	}
}

func TestIntoFromRoundTrip(t *testing.T) {
	t.Parallel()

	logger := log.New(jsonLogConfig(slog.LevelInfo), &bytes.Buffer{})
	ctx := log.Into(context.Background(), logger)
	if got := log.From(ctx); got != logger {
		t.Errorf("From returned %p, want the stored logger %p", got, logger)
	}
}

func TestIntoNilContextAndNilLogger(t *testing.T) {
	t.Parallel()

	logger := log.New(jsonLogConfig(slog.LevelInfo), &bytes.Buffer{})

	var nilCtx context.Context
	ctx := log.Into(nilCtx, logger)
	if ctx == nil {
		t.Fatal("Into(nil, logger) returned nil context")
	}
	if got := log.From(ctx); got != logger {
		t.Error("Into(nil, logger) did not store the logger")
	}

	base := context.Background()
	if got := log.Into(base, nil); got != base {
		t.Error("Into(ctx, nil) did not return the context unchanged")
	}
}

func TestWithAccumulatesFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := log.Into(context.Background(), log.New(jsonLogConfig(slog.LevelInfo), &buf))
	ctx = log.With(ctx, log.RunID("run-1"))
	ctx = log.With(ctx, log.StepID("step-a"))

	log.From(ctx).Info("step event")

	records := decodeLines(t, buf.Bytes())
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if got := records[0][log.FieldRunID]; got != "run-1" {
		t.Errorf("record[%q] = %v, want %q", log.FieldRunID, got, "run-1")
	}
	if got := records[0][log.FieldStepID]; got != "step-a" {
		t.Errorf("record[%q] = %v, want %q", log.FieldStepID, got, "step-a")
	}
}
