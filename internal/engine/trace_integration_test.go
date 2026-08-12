//go:build integration

package engine_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/mathcslearner/agentloom/internal/engine"
	obstrace "github.com/mathcslearner/agentloom/internal/obs/trace"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 7.3's integration suite: distributed tracing across the queue,
// asserted hermetically against an in-memory span recorder injected
// through the ConsumerConfig.TracerProvider and engine.WithTracerProvider
// seams — no collector, no export pipeline. The live two-process
// acceptance (one Jaeger trace spanning two worker containers) is
// make smoke-trace; these tests pin the span topology itself: one trace
// from submission through retries, links (never parent-child) for
// re-executions, and the claim/executor/completion/ack child spans.

// newSpanRecorder builds a TracerProvider whose spans land synchronously
// in the returned exporter.
func newSpanRecorder() (*tracetest.InMemoryExporter, *sdktrace.TracerProvider) {
	exporter := tracetest.NewInMemoryExporter()
	return exporter, sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
}

// submitTraced instantiates a run inside a root span the way the API does
// (ticket 7.3: the POST /v1/runs server span is the run's root), returning
// the run id and the root span's context.
func submitTraced(t *testing.T, s *store.Store, tp *sdktrace.TracerProvider, defJSON string) (runID uuid.UUID, root oteltrace.SpanContext) {
	t.Helper()
	def := mustDecode(t, defJSON)
	rootCtx, rootSpan := tp.Tracer("test").Start(t.Context(), "POST /v1/runs")
	traceparent, tracestate := obstrace.Inject(rootCtx)
	res, err := s.CreateRun(rootCtx, store.CreateRunArgs{
		Definition: def, Now: testNow,
		Trace: store.TraceContext{Parent: traceparent, State: tracestate},
	})
	rootSpan.End()
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return res.Run.ID, rootSpan.SpanContext()
}

// spanAttr returns the named attribute's stringified value ("" if absent).
func spanAttr(stub tracetest.SpanStub, key string) string {
	for _, kv := range stub.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// findAttempt locates the step.attempt span for (stepID, attempt).
func findAttempt(t *testing.T, spans tracetest.SpanStubs, stepID string, attempt string) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans {
		if s.Name == "step.attempt" && spanAttr(s, "step_id") == stepID && spanAttr(s, "attempt") == attempt {
			return s
		}
	}
	t.Fatalf("no step.attempt span for step %q attempt %s among %d spans", stepID, attempt, len(spans))
	return tracetest.SpanStub{}
}

// findChild locates a span named name whose parent is parentSpanID.
func findChild(t *testing.T, spans tracetest.SpanStubs, name string, parentSpanID oteltrace.SpanID) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans {
		if s.Name == name && s.Parent.SpanID() == parentSpanID {
			return s
		}
	}
	t.Fatalf("no %q span with parent %s", name, parentSpanID)
	return tracetest.SpanStub{}
}

// hasLinkTo reports whether the span carries a link to target.
func hasLinkTo(stub tracetest.SpanStub, target oteltrace.SpanID) bool {
	for _, l := range stub.Links {
		if l.SpanContext.SpanID() == target {
			return true
		}
	}
	return false
}

// waitSpans polls the exporter until cond holds — the attempt span ends a
// hair after the ACK the quiescence probes see, so assertions poll.
func waitSpans(t *testing.T, exporter *tracetest.InMemoryExporter, cond func(tracetest.SpanStubs) bool) tracetest.SpanStubs {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var spans tracetest.SpanStubs
	for time.Now().Before(deadline) {
		spans = exporter.GetSpans()
		if cond(spans) {
			return spans
		}
		time.Sleep(10 * time.Millisecond)
	}
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name+"("+spanAttr(s, "step_id")+"#"+spanAttr(s, "attempt")+")")
	}
	t.Fatalf("span condition never held; have %d spans: %s", len(spans), strings.Join(names, ", "))
	return nil
}

// countAttempts counts step.attempt spans in the recording.
func countAttempts(spans tracetest.SpanStubs) int {
	n := 0
	for _, s := range spans {
		if s.Name == "step.attempt" {
			n++
		}
	}
	return n
}

// syncBuffer is a goroutine-safe log sink for the trace_id assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// traceRetryDef fails once then succeeds under a deterministic two-attempt
// policy, with a successor proving cross-step parentage.
const traceRetryDef = `{
	"schema_version": 1,
	"name": "trace-retry",
	"steps": [
		{"id": "flaky", "type": "fail_n_times", "config": {"n": 1},
		 "retry": {"max_attempts": 2, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}},
		{"id": "after", "type": "noop"}
	],
	"edges": [{"from": "flaky", "to": "after"}]
}`

// TestTraceSingleRunWithRetryLink is 7.3's headline shape on one process:
// every span of a run — submission root, both attempts of a flaky step,
// the successor — shares one trace; the retry attempt is linked (never
// parented) to the failed attempt; the successor's attempt span descends
// from the completion transaction that readied it; and the attempt span
// carries the claim/executor/completion/ack children. trace_id appears in
// the structured logs for correlation.
func TestTraceSingleRunWithRetryLink(t *testing.T) {
	// Not Parallel: the test swaps slog's default logger to capture the
	// spawned consumer's log lines (its Run context carries no logger).
	ctx := t.Context()
	logs := &syncBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	exporter, tp := newSpanRecorder()
	runID, root := submitTraced(t, s, tp, traceRetryDef)
	h.EnsureGroup(ctx)

	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()),
		engine.WithTracerProvider(tp))
	cfg := retryWorkerConfig()
	cfg.TracerProvider = tp
	h.Spawn("worker-a", eng.Handle, cfg)

	// Attempt 1 fails; fire the scheduled retry; attempt 2 succeeds.
	due := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "flaky", due)
	clk.Set(due)
	if res := h.PromoteDue(ctx, due, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries, want 1", res.Promoted)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	// Three attempt spans: flaky #1, flaky #2, after #1.
	spans := waitSpans(t, exporter, func(sp tracetest.SpanStubs) bool { return countAttempts(sp) >= 3 })

	// One run, one trace — every recorded span shares the root's trace id.
	for _, sp := range spans {
		if sp.SpanContext.TraceID() != root.TraceID() {
			t.Errorf("span %s (%s) trace = %s, want the root trace %s",
				sp.Name, spanAttr(sp, "step_id"), sp.SpanContext.TraceID(), root.TraceID())
		}
	}

	attempt1 := findAttempt(t, spans, "flaky", "1")
	attempt2 := findAttempt(t, spans, "flaky", "2")
	afterAttempt := findAttempt(t, spans, "after", "1")

	// The entry step's attempt descends from the submission root span
	// (instantiation stamped the root context onto its outbox row).
	if attempt1.Parent.SpanID() != root.SpanID() {
		t.Errorf("attempt 1 parent = %s, want the submission root %s", attempt1.Parent.SpanID(), root.SpanID())
	}
	// The retry attempt parents to the run root (the delayed envelope's
	// durable context) and links — never parents — to the failed attempt.
	if attempt2.Parent.SpanID() != root.SpanID() {
		t.Errorf("retry attempt parent = %s, want the run root %s", attempt2.Parent.SpanID(), root.SpanID())
	}
	if attempt2.Parent.SpanID() == attempt1.SpanContext.SpanID() {
		t.Error("retry attempt is parented to the failed attempt; ADR-008 requires a link")
	}
	if !hasLinkTo(attempt2, attempt1.SpanContext.SpanID()) {
		t.Errorf("retry attempt carries no link to the failed attempt %s (links: %v)",
			attempt1.SpanContext.SpanID(), attempt2.Links)
	}

	// The attempt span carries the pipeline children (ADR-008 topology).
	findChild(t, spans, "step.claim", attempt2.SpanContext.SpanID())
	findChild(t, spans, "step.executor", attempt2.SpanContext.SpanID())
	completion := findChild(t, spans, "step.completion", attempt2.SpanContext.SpanID())
	findChild(t, spans, "queue.ack", attempt2.SpanContext.SpanID())

	// The successor descends from the completion transaction that readied
	// it — the enqueuing span, carried through the outbox row.
	if afterAttempt.Parent.SpanID() != completion.SpanContext.SpanID() {
		t.Errorf("successor attempt parent = %s, want the readying completion span %s",
			afterAttempt.Parent.SpanID(), completion.SpanContext.SpanID())
	}

	// trace_id joins the structured logs to the trace (ADR-008).
	if !strings.Contains(logs.String(), `"trace_id":"`+root.TraceID().String()+`"`) {
		t.Error("no log line carries the run trace's trace_id")
	}
	if !strings.Contains(logs.String(), `"span_id":"`) {
		t.Error("no log line carries a span_id")
	}
}

// traceJoinDef fans two entry steps into an all-mode join.
const traceJoinDef = `{
	"schema_version": 1,
	"name": "trace-join",
	"steps": [
		{"id": "a", "type": "noop"},
		{"id": "b", "type": "noop"},
		{"id": "merge", "type": "join", "config": {"mode": "all"}}
	],
	"edges": [
		{"from": "a", "to": "merge"},
		{"from": "b", "to": "merge"}
	]
}`

// TestTraceJoinFanInLinks: a fan-in join's attempt span carries links to
// every firing parent's attempt span (ADR-008) — the parent chain alone
// can only show the last-firing parent.
func TestTraceJoinFanInLinks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	exporter, tp := newSpanRecorder()
	runID, root := submitTraced(t, s, tp, traceJoinDef)
	h.EnsureGroup(ctx)

	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a", engine.WithDispatchNudge(d.Nudge), engine.WithTracerProvider(tp))
	cfg := workerConfig()
	cfg.TracerProvider = tp
	h.Spawn("worker-a", eng.Handle, cfg)

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	spans := waitSpans(t, exporter, func(sp tracetest.SpanStubs) bool { return countAttempts(sp) >= 3 })

	for _, sp := range spans {
		if sp.SpanContext.TraceID() != root.TraceID() {
			t.Errorf("span %s trace = %s, want %s", sp.Name, sp.SpanContext.TraceID(), root.TraceID())
		}
	}
	aAttempt := findAttempt(t, spans, "a", "1")
	bAttempt := findAttempt(t, spans, "b", "1")
	merge := findAttempt(t, spans, "merge", "1")
	if !hasLinkTo(merge, aAttempt.SpanContext.SpanID()) {
		t.Errorf("join attempt carries no link to parent a's attempt (links: %v)", merge.Links)
	}
	if !hasLinkTo(merge, bAttempt.SpanContext.SpanID()) {
		t.Errorf("join attempt carries no link to parent b's attempt (links: %v)", merge.Links)
	}
}

// TestTraceTakeoverLink: a reclaimed delivery of a running step whose
// holder went silent is taken over, and the new attempt span links to the
// lost attempt's span — restored from run_steps.trace_span, since the dead
// holder can hand nothing over.
func TestTraceTakeoverLink(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	exporter, tp := newSpanRecorder()
	runID, root := submitTraced(t, s, tp, singleNoop)
	h.EnsureGroup(ctx)

	// The doomed holder's attempt span context: same trace, a span id the
	// recorder never sees (the "worker" dies without exporting).
	deadSC := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    root.TraceID(),
		SpanID:     oteltrace.SpanID{0xde, 0xad, 0xde, 0xad, 0xde, 0xad, 0xde, 0xad},
		TraceFlags: oteltrace.FlagsSampled,
	})

	// Deliver the entry to a consumer that will never act (its PEL entry
	// goes idle), and claim the step in Postgres as that dead holder —
	// trace_span stamped, exactly what a real claim writes before a crash.
	h.Enqueue(ctx, queue.Envelope{
		RunID: runID, StepID: "only", Reason: queue.ReasonStepReady,
		TraceParent: obstrace.SpanContext(root),
	})
	h.ReadOne(ctx, "dead-worker")
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{
			RunID: runID, StepID: "only", Now: testNow,
			TraceSpan: obstrace.SpanContext(deadSC),
		})
		return err
	})
	if err != nil {
		t.Fatalf("claiming as the dead holder: %v", err)
	}

	// A live worker with a short lease reclaims the idle entry, takes the
	// step over, and completes it.
	eng := newWorker(t, s, "worker-b", engine.WithTracerProvider(tp))
	cfg := queue.ConsumerConfig{
		Block: 200 * time.Millisecond, Batch: 1,
		LeaseTTL: 300 * time.Millisecond, PromoterTick: time.Hour,
		TracerProvider: tp,
	}
	h.Spawn("worker-b", eng.Handle, cfg)

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	requireAttemptOutcomes(t, s, runID, "only", []string{
		store.AttemptOutcomeLost, store.StepStatusSucceeded,
	})

	spans := waitSpans(t, exporter, func(sp tracetest.SpanStubs) bool { return countAttempts(sp) >= 1 })
	takeover := findAttempt(t, spans, "only", "2")
	if takeover.SpanContext.TraceID() != root.TraceID() {
		t.Errorf("takeover attempt trace = %s, want %s", takeover.SpanContext.TraceID(), root.TraceID())
	}
	if !hasLinkTo(takeover, deadSC.SpanID()) {
		t.Errorf("takeover attempt carries no link to the lost attempt %s (links: %v)",
			deadSC.SpanID(), takeover.Links)
	}
	if takeover.Parent.SpanID() == deadSC.SpanID() {
		t.Error("takeover attempt is parented to the lost attempt; ADR-008 requires a link")
	}
}
