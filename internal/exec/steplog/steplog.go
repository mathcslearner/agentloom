// Package steplog is per-step log capture (ticket 7.4, ADR-008): the tee
// that copies executor log lines — everything emitted through
// exec.StepContext.Logger — into the durable step_logs store feeding
// GET /v1/runs/{id}/steps/{sid}/logs and M18's per-step log view.
//
// The design is one Sink per worker process holding a bounded in-memory
// queue, drained to Postgres by an async flusher. The capture path is a
// non-blocking O(1) enqueue with drop-oldest on overflow, so a flooding
// executor can never stall execution — the ticket's hard requirement. Two
// mechanisms cap storage, both dropping oldest: the queue (backpressure)
// and the per-attempt ring cap enforced at flush time (StepLogRepo.Trim).
// Every captured line consumes a per-attempt sequence number whether or
// not it survives, so the logs API derives the truncation marker as
// max(seq) − stored rows, with no marker row to maintain.
//
// Seq allocation needs no coordination: retries, reclaims, and takeovers
// all mint a new attempt number at claim, so an attempt's ring has
// exactly one writer. A zombie's late flush lands under its old attempt
// number — harmless, preserved diagnostics.
package steplog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Defaults for Config's zero values.
const (
	DefaultLevel         = slog.LevelInfo
	DefaultCap           = 1000
	DefaultBuffer        = 8192
	DefaultMaxLineBytes  = 8192
	DefaultFlushInterval = 500 * time.Millisecond
	DefaultFlushBatch    = 512

	// closeFlushTimeout bounds the final drain when Run's context ends —
	// the worker is shutting down and must not hang on a wedged Postgres.
	closeFlushTimeout = 5 * time.Second
)

// Metrics is the sink's observability seam (ADR-008), satisfied
// structurally by obs/metrics.WorkerMetrics. The default is a no-op.
type Metrics interface {
	// StepLogCaptured records n lines accepted into the queue.
	StepLogCaptured(n int)
	// StepLogDropped records n lines dropped: queue overflow, or a failed
	// flush abandoning its batch.
	StepLogDropped(n int)
	// StepLogFlushFailure records one failed flush transaction.
	StepLogFlushFailure()
}

type nopMetrics struct{}

func (nopMetrics) StepLogCaptured(int)  {}
func (nopMetrics) StepLogDropped(int)   {}
func (nopMetrics) StepLogFlushFailure() {}

// Config tunes a Sink. The zero value means every default above.
type Config struct {
	// Level is the minimum captured level. Records below it are filtered —
	// they consume no seq and never count as dropped (unlike overflow).
	Level slog.Level
	// Cap is the per-attempt ring size: after each flush the attempt keeps
	// at most this many newest lines in the store.
	Cap int
	// Buffer is the in-memory queue capacity (lines) shared by every
	// in-flight step on this worker. Overflow drops the oldest queued line.
	Buffer int
	// MaxLineBytes caps one line's message and its marshaled fields, each
	// truncated independently with an explicit marker.
	MaxLineBytes int
	// FlushInterval is the flusher's cadence.
	FlushInterval time.Duration
	// FlushBatch is the maximum lines per flush transaction.
	FlushBatch int
	// Metrics receives capture/drop/flush-failure counts; nil means no-op.
	Metrics Metrics
	// Now stamps lines whose slog record carries a zero time (hand-built
	// records in tests); nil means time.Now. Ordinary records keep their
	// call-site timestamp — diagnostics, not engine state.
	Now func() time.Time
}

// withDefaults resolves zero fields. Level's zero value is LevelInfo
// (slog's convention), which is also our default — nothing to resolve.
func (c Config) withDefaults() Config {
	if c.Cap <= 0 {
		c.Cap = DefaultCap
	}
	if c.Buffer <= 0 {
		c.Buffer = DefaultBuffer
	}
	if c.MaxLineBytes <= 0 {
		c.MaxLineBytes = DefaultMaxLineBytes
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.FlushBatch <= 0 {
		c.FlushBatch = DefaultFlushBatch
	}
	if c.Metrics == nil {
		c.Metrics = nopMetrics{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// line is one captured log line, queued for the flusher.
type line struct {
	runID    uuid.UUID
	stepID   string
	attempt  int32
	seq      int64
	level    string
	message  string
	fields   []byte // marshaled JSON object; nil when the line had no attrs
	traceID  string
	loggedAt time.Time
}

// Sink is the worker-wide capture buffer and flusher. Build one with New,
// run its flusher with Run, and hand per-attempt tee loggers to the
// engine via LoggerFor. All methods are safe for concurrent use.
type Sink struct {
	st     *store.Store
	cfg    Config
	logger *slog.Logger

	mu   sync.Mutex
	buf  []line // fixed-capacity ring
	head int
	n    int
}

// New builds a Sink over st. logger receives the sink's own diagnostics
// (flush failures); nil means slog.Default().
func New(st *store.Store, cfg Config, logger *slog.Logger) *Sink {
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.withDefaults()
	return &Sink{st: st, cfg: cfg, logger: logger, buf: make([]line, cfg.Buffer)}
}

// LoggerFor returns base teed into this sink for one step attempt: lines
// keep flowing to base's handler exactly as before, and those at or above
// the capture level are also queued for durable storage under (runID,
// stepID, attempt), stamped with traceID (empty when tracing is off).
// Safe on a nil Sink — base comes back untouched, the engine's
// tee-disabled path.
func (s *Sink) LoggerFor(base *slog.Logger, runID uuid.UUID, stepID string, attempt int, traceID string) *slog.Logger {
	if s == nil {
		return base
	}
	c := &captureHandler{
		sink: s, runID: runID, stepID: stepID,
		attempt: int32(attempt), //nolint:gosec // attempt_count is an INT column
		traceID: traceID,
		seq:     new(seqCounter),
	}
	return slog.New(fanoutHandler{base.Handler(), c})
}

// enqueue queues one captured line, dropping the oldest queued line when
// the buffer is full. Never blocks beyond the mutex — this is the
// capture hot path.
func (s *Sink) enqueue(l line) {
	s.mu.Lock()
	dropped := false
	if s.n == len(s.buf) {
		// Drop-oldest: advance past the head slot, freeing it for l.
		s.head = (s.head + 1) % len(s.buf)
		s.n--
		dropped = true
	}
	s.buf[(s.head+s.n)%len(s.buf)] = l
	s.n++
	s.mu.Unlock()
	s.cfg.Metrics.StepLogCaptured(1)
	if dropped {
		s.cfg.Metrics.StepLogDropped(1)
	}
}

// popBatch removes and returns up to limit queued lines, oldest first.
func (s *Sink) popBatch(limit int) []line {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == 0 {
		return nil
	}
	k := min(s.n, limit)
	out := make([]line, k)
	for i := range k {
		out[i] = s.buf[(s.head+i)%len(s.buf)]
		s.buf[(s.head+i)%len(s.buf)] = line{} // release referenced memory
	}
	s.head = (s.head + k) % len(s.buf)
	s.n -= k
	return out
}

// Run drains the queue on FlushInterval until ctx ends, then performs one
// final bounded flush so a graceful shutdown loses nothing it still
// holds. cmd/worker runs this on the loop context that outlives SIGTERM —
// lines from steps finishing during the consumer's drain still land.
func (s *Sink) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeFlushTimeout)
			s.Flush(flushCtx)
			cancel()
			return
		case <-t.C:
			s.Flush(ctx)
		}
	}
}

// Flush drains everything queued at entry, in per-attempt transactions of
// at most FlushBatch lines each: the batch insert plus the ring-cap trim.
// Failures are logged, counted, and the failed group dropped — the sink
// trades completeness for guaranteed forward progress (these are
// diagnostics; execution must never wait on them).
func (s *Sink) Flush(ctx context.Context) {
	for {
		batch := s.popBatch(s.cfg.FlushBatch)
		if len(batch) == 0 {
			return
		}
		for _, g := range groupByAttempt(batch) {
			s.flushGroup(ctx, g)
		}
	}
}

// group is one attempt's slice of a batch, in queue (seq) order.
type group struct {
	runID   uuid.UUID
	stepID  string
	attempt int32
	lines   []line
}

// groupByAttempt splits a batch by attempt, preserving order within each
// group (the queue is oldest-first, and seq is monotonic per attempt).
func groupByAttempt(batch []line) []group {
	type key struct {
		runID   uuid.UUID
		stepID  string
		attempt int32
	}
	index := make(map[key]int)
	var groups []group
	for _, l := range batch {
		k := key{l.runID, l.stepID, l.attempt}
		i, ok := index[k]
		if !ok {
			i = len(groups)
			index[k] = i
			groups = append(groups, group{runID: l.runID, stepID: l.stepID, attempt: l.attempt})
		}
		groups[i].lines = append(groups[i].lines, l)
	}
	return groups
}

// flushGroup writes one attempt's lines and enforces its ring cap in one
// transaction.
func (s *Sink) flushGroup(ctx context.Context, g group) {
	rows := make([]gen.CreateStepLogsParams, len(g.lines))
	maxSeq := int64(0)
	for i, l := range g.lines {
		rows[i] = gen.CreateStepLogsParams{
			RunID: l.runID, StepID: l.stepID, Attempt: l.attempt, Seq: l.seq,
			Level: l.level, Message: l.message, Fields: l.fields,
			LoggedAt: l.loggedAt,
		}
		if l.traceID != "" {
			rows[i].TraceID = &g.lines[i].traceID
		}
		if l.seq > maxSeq {
			maxSeq = l.seq
		}
	}
	err := s.st.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if _, err := q.StepLogs().CreateBatch(ctx, rows); err != nil {
			return err
		}
		if drop := maxSeq - int64(s.cfg.Cap); drop > 0 {
			if _, err := q.StepLogs().Trim(ctx, g.runID, g.stepID, g.attempt, drop); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Log-and-drop, never retry: a poisoned group (deleted run, seq
		// conflict from a protocol bug) would otherwise wedge the flusher.
		s.cfg.Metrics.StepLogFlushFailure()
		s.cfg.Metrics.StepLogDropped(len(g.lines))
		s.logger.WarnContext(ctx, "step log flush failed; batch dropped",
			slog.String("run_id", g.runID.String()),
			slog.String("step_id", g.stepID),
			slog.Int("attempt", int(g.attempt)),
			slog.Int("lines", len(g.lines)),
			slog.Any("error", err))
	}
}
