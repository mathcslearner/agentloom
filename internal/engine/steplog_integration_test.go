//go:build integration

package engine_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/steplog"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Ticket 7.4's engine suite: the StepContext.Logger tee end to end —
// lines land in step_logs with attempt and trace_id, retries get separate
// per-attempt rings, and the headline: a 10k-line flood neither stalls
// execution nor exceeds the ring cap, with the truncation marker derived.

// steplogMetrics counts the sink's Metrics calls (engine_test's own —
// the steplog package's counter is unexported).
type steplogMetrics struct {
	mu                sync.Mutex
	captured, dropped int
	flushFailures     int
}

func (m *steplogMetrics) StepLogCaptured(n int) { m.mu.Lock(); m.captured += n; m.mu.Unlock() }
func (m *steplogMetrics) StepLogDropped(n int)  { m.mu.Lock(); m.dropped += n; m.mu.Unlock() }
func (m *steplogMetrics) StepLogFlushFailure()  { m.mu.Lock(); m.flushFailures++; m.mu.Unlock() }

// chattyNoop is a noop-typed executor that logs through the step logger:
// one debug line (filtered at the default info capture level), then
// `lines` info lines, then one warn line with grouped attrs. It fails
// with a transient error while sc.Attempt <= failFirst.
type chattyNoop struct {
	lines     int
	failFirst int
}

func (*chattyNoop) Type() string { return string(dag.StepNoop) }

func (c *chattyNoop) Execute(_ context.Context, sc exec.StepContext) (exec.Output, error) {
	logger := sc.Logger
	logger.Debug("below the capture level")
	for i := 1; i <= c.lines; i++ {
		logger.Info(fmt.Sprintf("working %d", i), slog.Int("i", i))
	}
	logger.WithGroup("io").Warn("finishing", slog.String("phase", "close"), slog.Int("attempt", sc.Attempt))
	if sc.Attempt <= c.failFirst {
		return exec.Output{}, exec.Transientf("deliberate failure on attempt %d", sc.Attempt)
	}
	return exec.Output{}, nil
}

// newStepLogSink builds a sink over the test store with manual flushing
// (a huge interval; tests call Flush themselves for determinism).
func newStepLogSink(s *store.Store, cfg steplog.Config) *steplog.Sink {
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = time.Hour
	}
	return steplog.New(s, cfg, slog.New(slog.DiscardHandler))
}

// listAllStepLogs reads every stored line of one attempt, in seq order.
func listAllStepLogs(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, attempt int32) []gen.StepLog {
	t.Helper()
	rows, err := s.StepLogs().ListPage(t.Context(), gen.ListStepLogsPageParams{
		RunID: runID, StepID: stepID, Attempt: attempt,
		Levels: store.LogLevelsAtOrAbove(store.LogLevelDebug), Limit: 1 << 20,
	})
	if err != nil {
		t.Fatalf("listing step logs: %v", err)
	}
	return rows
}

// TestStepLogsCapturedWithTraceAndAttempt: the tee captures executor
// lines durably — level-filtered at capture, fields marshaled, trace_id
// stamped from the attempt span, attempt from the durable claim.
func TestStepLogsCapturedWithTraceAndAttempt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, singleNoop)
	_, tp := newSpanRecorder()
	sink := newStepLogSink(s, steplog.Config{})
	reg, err := exec.NewRegistry(&chattyNoop{lines: 2})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithStepLogs(sink), engine.WithTracerProvider(tp))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, queue.ConsumerConfig{
		Block: 500 * time.Millisecond, Batch: 1, TracerProvider: tp,
	})
	h.Enqueue(ctx, stepEnvelope(runID, "only"))

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	sink.Flush(ctx)

	rows := listAllStepLogs(t, s, runID, "only", 1)
	// The debug line is filtered at capture (default level info) and
	// consumes no seq: exactly 2 info + 1 warn survive, seqs 1..3.
	if len(rows) != 3 {
		t.Fatalf("stored %d lines, want 3 (debug filtered at capture):\n%+v", len(rows), rows)
	}
	for i, row := range rows {
		if row.Seq != int64(i+1) {
			t.Errorf("row %d seq = %d, want %d", i, row.Seq, i+1)
		}
		if row.Attempt != 1 {
			t.Errorf("row %d attempt = %d, want 1", i, row.Attempt)
		}
		if row.TraceID == nil || len(*row.TraceID) != 32 {
			t.Errorf("row %d trace_id = %v, want a 32-hex trace id from the attempt span", i, row.TraceID)
		}
	}
	if rows[0].Level != store.LogLevelInfo || rows[0].Message != "working 1" ||
		!jsonEqual(t, rows[0].Fields, []byte(`{"i": 1}`)) {
		t.Errorf("first line = %+v, want info 'working 1' with fields {\"i\":1}", rows[0])
	}
	last := rows[2]
	if last.Level != store.LogLevelWarn || last.Message != "finishing" ||
		!jsonEqual(t, last.Fields, []byte(`{"io": {"phase": "close", "attempt": 1}}`)) {
		t.Errorf("warn line = %+v (fields %s), want grouped fields under io", last, last.Fields)
	}
}

// TestStepLogsPerAttemptRings: a retrying step's attempts are separate
// log streams — the retry (a fresh attempt via the 5.2 machinery on the
// fake clock) starts its own ring at seq 1, nothing appended to the
// failed attempt's.
func TestStepLogsPerAttemptRings(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const retryNoop = `{
		"schema_version": 1,
		"name": "steplog-retry",
		"steps": [
			{"id": "only", "type": "noop",
			 "retry": {"max_attempts": 2, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}}
		],
		"edges": []
	}`
	s, h, runID := setup(t, retryNoop)
	clk := newFakeClock(testNow)
	sink := newStepLogSink(s, steplog.Config{})
	reg, err := exec.NewRegistry(&chattyNoop{lines: 1, failFirst: 1})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()),
		engine.WithStepLogs(sink))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	due := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "only", due)
	clk.Set(due)
	if res := h.PromoteDue(ctx, due, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries, want 1", res.Promoted)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	sink.Flush(ctx)

	requireAttemptOutcomes(t, s, runID, "only", []string{
		store.AttemptOutcomeTransient, store.StepStatusSucceeded,
	})
	for attempt := int32(1); attempt <= 2; attempt++ {
		rows := listAllStepLogs(t, s, runID, "only", attempt)
		if len(rows) != 2 || rows[0].Seq != 1 {
			t.Fatalf("attempt %d ring = %d rows starting at seq %d, want its own 2-row ring from seq 1:\n%+v",
				attempt, len(rows), rows[0].Seq, rows)
		}
		if !jsonEqual(t, rows[1].Fields, fmt.Appendf(nil, `{"io": {"phase": "close", "attempt": %d}}`, attempt)) {
			t.Errorf("attempt %d warn fields = %s, want its own attempt number", attempt, rows[1].Fields)
		}
	}
}

// floodNoop logs `lines` info lines as fast as possible — the executor
// log flood of the ticket's acceptance criterion.
type floodNoop struct {
	lines int
}

func (*floodNoop) Type() string { return string(dag.StepNoop) }

func (f *floodNoop) Execute(_ context.Context, sc exec.StepContext) (exec.Output, error) {
	for i := 1; i <= f.lines; i++ {
		sc.Logger.Info(fmt.Sprintf("flood %d", i))
	}
	return exec.Output{}, nil
}

// TestStepLogFloodCappedWithoutStallingExecution is the acceptance
// criterion: 10k lines against a small buffer and ring cap. The step
// completes promptly (the capture path is a non-blocking enqueue with
// drop-oldest — nothing ever waits on Postgres), storage stays at or
// under the cap holding the newest lines, and the loss is visible as the
// derived truncation marker.
func TestStepLogFloodCappedWithoutStallingExecution(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const written, ringCap, buffer = 10_000, 100, 512
	s, h, runID := setup(t, singleNoop)
	m := &steplogMetrics{}
	sink := newStepLogSink(s, steplog.Config{Cap: ringCap, Buffer: buffer, Metrics: m})
	reg, err := exec.NewRegistry(&floodNoop{lines: written})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a", engine.WithStepLogs(sink))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, workerConfig())

	start := time.Now()
	h.Enqueue(ctx, stepEnvelope(runID, "only"))
	waitRun(t, s, runID, store.RunStatusSucceeded)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("flooded run took %v — the capture path must not stall execution", elapsed)
	}
	h.WaitQuiescent(ctx)
	sink.Flush(ctx)

	stats, err := s.StepLogs().Stats(ctx, runID, "only", 1)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Stored > ringCap {
		t.Errorf("stored %d lines, want at most the ring cap %d", stats.Stored, ringCap)
	}
	if stats.MaxSeq != written {
		t.Errorf("max seq = %d, want %d (every line consumed a seq)", stats.MaxSeq, written)
	}
	// The derived truncation marker: dropped = max seq − stored.
	if dropped := stats.MaxSeq - stats.Stored; dropped < written-ringCap {
		t.Errorf("derived dropped = %d, want at least %d", dropped, written-ringCap)
	}
	// The retained window is the newest lines.
	rows := listAllStepLogs(t, s, runID, "only", 1)
	if len(rows) == 0 || rows[len(rows)-1].Seq != written {
		t.Fatalf("retained window ends at seq %d, want the newest line %d", rows[len(rows)-1].Seq, written)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.captured != written || m.dropped == 0 || m.flushFailures != 0 {
		t.Errorf("metrics = %d captured, %d dropped, %d flush failures; want %d captured, some buffer drops, none failed",
			m.captured, m.dropped, m.flushFailures, written)
	}
}
