package steplog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
)

// White-box tests: the capture path (handler → queue) is exercised
// without a database — Flush is integration-tested with the engine.

// countingMetrics records the Metrics calls.
type countingMetrics struct {
	mu                               sync.Mutex
	captured, dropped, flushFailures int
}

func (m *countingMetrics) StepLogCaptured(n int) { m.mu.Lock(); m.captured += n; m.mu.Unlock() }
func (m *countingMetrics) StepLogDropped(n int)  { m.mu.Lock(); m.dropped += n; m.mu.Unlock() }
func (m *countingMetrics) StepLogFlushFailure()  { m.mu.Lock(); m.flushFailures++; m.mu.Unlock() }

// newTestSink builds a storeless sink (Flush must not be called).
func newTestSink(cfg Config) *Sink {
	return New(nil, cfg, slog.New(slog.DiscardHandler))
}

// drain pops everything queued.
func drain(s *Sink) []line {
	var out []line
	for {
		batch := s.popBatch(1 << 20)
		if len(batch) == 0 {
			return out
		}
		out = append(out, batch...)
	}
}

func TestCaptureLineIdentityAndSeq(t *testing.T) {
	t.Parallel()
	s := newTestSink(Config{})
	runID := uuid.New()
	logger := s.LoggerFor(slog.New(slog.DiscardHandler), runID, "a", 3, "cafe1234")

	logger.Info("first", slog.Int("n", 1))
	logger.Warn("second")

	lines := drain(s)
	if len(lines) != 2 {
		t.Fatalf("captured %d lines, want 2", len(lines))
	}
	first := lines[0]
	if first.runID != runID || first.stepID != "a" || first.attempt != 3 || first.traceID != "cafe1234" {
		t.Errorf("line identity = %+v, want run/step/attempt/trace stamped", first)
	}
	if first.seq != 1 || lines[1].seq != 2 {
		t.Errorf("seqs = %d, %d, want 1, 2", first.seq, lines[1].seq)
	}
	if first.level != store.LogLevelInfo || lines[1].level != store.LogLevelWarn {
		t.Errorf("levels = %q, %q, want info, warn", first.level, lines[1].level)
	}
	if first.message != "first" || first.loggedAt.IsZero() {
		t.Errorf("line = %+v, want message and a timestamp", first)
	}
	var fields map[string]any
	if err := json.Unmarshal(first.fields, &fields); err != nil || fields["n"] != float64(1) {
		t.Errorf("fields = %s (%v), want {\"n\": 1}", first.fields, err)
	}
	if lines[1].fields != nil {
		t.Errorf("attr-less line fields = %s, want nil", lines[1].fields)
	}
}

func TestCaptureLevelFilterConsumesNoSeq(t *testing.T) {
	t.Parallel()
	s := newTestSink(Config{Level: slog.LevelInfo})
	logger := s.LoggerFor(slog.New(slog.DiscardHandler), uuid.New(), "a", 1, "")

	logger.Debug("filtered")
	logger.Info("kept")

	lines := drain(s)
	if len(lines) != 1 || lines[0].seq != 1 || lines[0].message != "kept" {
		t.Fatalf("lines = %+v, want exactly the info line at seq 1 (filtered records consume no seq)", lines)
	}
}

func TestCaptureWithAttrsAndGroups(t *testing.T) {
	t.Parallel()
	s := newTestSink(Config{})
	base := s.LoggerFor(slog.New(slog.DiscardHandler), uuid.New(), "a", 1, "")

	derived := base.With(slog.String("tool", "curl")).WithGroup("http").With(slog.Int("status", 200))
	derived.Info("done", slog.String("phase", "close"), slog.Any("error", errors.New("boom")))
	// The derivative shares the attempt's seq counter with its parent.
	base.Info("parent")

	lines := drain(s)
	if len(lines) != 2 {
		t.Fatalf("captured %d lines, want 2", len(lines))
	}
	var fields map[string]any
	if err := json.Unmarshal(lines[0].fields, &fields); err != nil {
		t.Fatalf("fields %s: %v", lines[0].fields, err)
	}
	if fields["tool"] != "curl" {
		t.Errorf("fields = %v, want top-level tool=curl", fields)
	}
	http, _ := fields["http"].(map[string]any)
	if http == nil || http["status"] != float64(200) || http["phase"] != "close" || http["error"] != "boom" {
		t.Errorf("fields.http = %v, want status/phase under the open group and the error stringified", http)
	}
	if lines[0].seq != 1 || lines[1].seq != 2 {
		t.Errorf("seqs = %d, %d, want a shared counter (1, 2)", lines[0].seq, lines[1].seq)
	}
}

func TestCaptureTruncatesOversizedLines(t *testing.T) {
	t.Parallel()
	s := newTestSink(Config{MaxLineBytes: 64})
	logger := s.LoggerFor(slog.New(slog.DiscardHandler), uuid.New(), "a", 1, "")

	logger.Info(strings.Repeat("m", 500))
	logger.Info("big fields", slog.String("blob", strings.Repeat("f", 500)))

	lines := drain(s)
	if got := lines[0].message; len(got) != 64+len(truncationSuffix) || !strings.HasSuffix(got, truncationSuffix) {
		t.Errorf("message len %d = %q..., want 64 bytes + truncation suffix", len(got), got[:20])
	}
	var fields map[string]any
	if err := json.Unmarshal(lines[1].fields, &fields); err != nil {
		t.Fatalf("fields %s: %v", lines[1].fields, err)
	}
	if _, ok := fields["_fields_truncated_bytes"]; !ok || fields["blob"] != nil {
		t.Errorf("oversized fields = %v, want the truncation marker object", fields)
	}
}

func TestEnqueueDropsOldestOnOverflow(t *testing.T) {
	t.Parallel()
	m := &countingMetrics{}
	s := newTestSink(Config{Buffer: 4, Metrics: m})
	logger := s.LoggerFor(slog.New(slog.DiscardHandler), uuid.New(), "a", 1, "")

	for i := 1; i <= 10; i++ {
		logger.Info(fmt.Sprintf("line %d", i))
	}

	lines := drain(s)
	if len(lines) != 4 {
		t.Fatalf("queued %d lines, want buffer size 4", len(lines))
	}
	for i, l := range lines {
		if want := int64(7 + i); l.seq != want {
			t.Errorf("survivor %d seq = %d, want %d (newest kept, oldest dropped)", i, l.seq, want)
		}
	}
	if m.captured != 10 || m.dropped != 6 {
		t.Errorf("metrics = %d captured, %d dropped, want 10, 6", m.captured, m.dropped)
	}
}

func TestFanoutStillFeedsBaseHandler(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := newTestSink(Config{Level: slog.LevelWarn})
	logger := s.LoggerFor(base, uuid.New(), "a", 1, "")

	// Below the capture level but above the base handler's: the terminal
	// side keeps its own policy.
	logger.Info("terminal only", slog.String("k", "v"))

	if !strings.Contains(buf.String(), "terminal only") || !strings.Contains(buf.String(), `"k":"v"`) {
		t.Errorf("base handler output = %q, want the record passed through", buf.String())
	}
	if lines := drain(s); len(lines) != 0 {
		t.Errorf("captured %d lines below the capture level, want 0", len(lines))
	}
}

func TestNilSinkPassesBaseThrough(t *testing.T) {
	t.Parallel()
	base := slog.New(slog.DiscardHandler)
	var s *Sink
	if got := s.LoggerFor(base, uuid.New(), "a", 1, ""); got != base {
		t.Errorf("nil sink LoggerFor = %v, want base unchanged", got)
	}
}

func TestGroupByAttemptPreservesOrder(t *testing.T) {
	t.Parallel()
	run := uuid.New()
	batch := []line{
		{runID: run, stepID: "a", attempt: 1, seq: 1},
		{runID: run, stepID: "b", attempt: 1, seq: 1},
		{runID: run, stepID: "a", attempt: 1, seq: 2},
		{runID: run, stepID: "a", attempt: 2, seq: 1},
	}
	groups := groupByAttempt(batch)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	if groups[0].stepID != "a" || len(groups[0].lines) != 2 ||
		groups[0].lines[0].seq != 1 || groups[0].lines[1].seq != 2 {
		t.Errorf("group a/1 = %+v, want seqs 1,2 in order", groups[0])
	}
}

func TestLevelStringBands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		l    slog.Level
		want string
	}{
		{slog.LevelDebug - 4, store.LogLevelDebug},
		{slog.LevelDebug, store.LogLevelDebug},
		{slog.LevelInfo, store.LogLevelInfo},
		{slog.LevelInfo + 2, store.LogLevelInfo},
		{slog.LevelWarn, store.LogLevelWarn},
		{slog.LevelError, store.LogLevelError},
		{slog.LevelError + 8, store.LogLevelError},
	}
	for _, c := range cases {
		if got := levelString(c.l); got != c.want {
			t.Errorf("levelString(%v) = %q, want %q", c.l, got, c.want)
		}
	}
}

// TestCaptureRecordTimeKept pins that a record's call-site time survives
// (UTC) and the injected clock only backfills zero times.
func TestCaptureRecordTimeKept(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	s := newTestSink(Config{Now: func() time.Time { return fixed }})
	logger := s.LoggerFor(slog.New(slog.DiscardHandler), uuid.New(), "a", 1, "")

	before := time.Now().Add(-time.Second)
	logger.Info("stamped")
	lines := drain(s)
	if lines[0].loggedAt.Before(before) || lines[0].loggedAt.Equal(fixed) {
		t.Errorf("loggedAt = %v, want the record's own call-site time", lines[0].loggedAt)
	}
}
