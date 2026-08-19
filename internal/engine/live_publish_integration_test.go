//go:build integration

package engine_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/event/pubsub"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// uniqueEventPrefix isolates a test's pub/sub channels on the shared Redis.
func uniqueEventPrefix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "agentloom-test-events-" + hex.EncodeToString(b[:])
}

// setupWithSink builds one test's isolated world with an event pub/sub publisher
// wired as the store's after-commit sink, and returns the sink-wired store, the
// queue harness (whose Redis client the publisher/subscribers share), the run
// id, and the channel prefix.
func setupWithSink(t *testing.T, defJSON string, pub store.EventSink) (*store.Store, *queuetest.Harness, uuid.UUID) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t), store.WithEventSink(pub))
	h := queuetest.New(t)
	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("decoding definition: %v", err)
	}
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	h.EnsureGroup(t.Context())
	return s, h, res.Run.ID
}

// Since ticket 16.3 the store's event repo satisfies pubsub.Backfiller directly
// (store.EventRepo.EventsAfter), so these tests feed s.Events() to the Tailer
// with no local adapter — the same wiring the WS server uses.

// tsSink wraps an inner sink and stamps the commit time of each event by seq, so
// a test can measure commit-to-received latency end to end.
type tsSink struct {
	inner     store.EventSink
	mu        sync.Mutex
	committed map[int64]time.Time
}

func newTSSink(inner store.EventSink) *tsSink {
	return &tsSink{inner: inner, committed: map[int64]time.Time{}}
}

func (s *tsSink) EventsCommitted(ctx context.Context, envs []event.Envelope) {
	now := time.Now()
	s.mu.Lock()
	for _, e := range envs {
		s.committed[e.Seq] = now
	}
	s.mu.Unlock()
	s.inner.EventsCommitted(ctx, envs)
}

func (s *tsSink) commitTime(seq int64) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.committed[seq]
	return t, ok
}

// countingMetrics is a pubsub.Metrics that counts publish outcomes.
type countingMetrics struct {
	mu        sync.Mutex
	published int
	failed    int
	dropped   int
}

func (m *countingMetrics) EventPublished(string)        { m.mu.Lock(); m.published++; m.mu.Unlock() }
func (m *countingMetrics) PublishFailed()               { m.mu.Lock(); m.failed++; m.mu.Unlock() }
func (m *countingMetrics) PublishDropped(n int)         { m.mu.Lock(); m.dropped += n; m.mu.Unlock() }
func (m *countingMetrics) PublishLatency(time.Duration) {}
func (m *countingMetrics) failures() int                { m.mu.Lock(); defer m.mu.Unlock(); return m.failed }

// TestLivePublishReachesSubscriber is DoD-1 (ADR-018): a subscriber tailing a
// run's channel sees committed events promptly (the <100ms local budget), and
// the snapshot/backfill/tail assembly (the 16.3 protocol shape) delivers the
// complete feed with no gaps or dupes while two workers execute the run.
// Latency is measured on the live-delivered events; completeness is guaranteed
// by the Tailer's gap-detected backfill (the events published before the
// subscription existed — run_created and the entry step_ready — are healed by
// the first live message's gap, exactly as a real client resumes).
func TestLivePublishReachesSubscriber(t *testing.T) {
	t.Parallel()
	prefix := uniqueEventPrefix(t)
	h := queuetest.New(t)
	client := h.Client()

	pub := pubsub.NewPublisher(client, pubsub.Options{Prefix: prefix})
	ts := newTSSink(pub)
	// Build the world with the timestamping sink in front of the publisher.
	s := store.NewFromPool(storetest.NewDB(t), store.WithEventSink(ts))
	def, err := dag.Decode([]byte(fanoutDef))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	h.EnsureGroup(t.Context())
	defer func() { _ = pub.Close(context.Background()) }()

	sub, err := pubsub.SubscribeRun(t.Context(), client, prefix, runID, nil)
	if err != nil {
		t.Fatalf("SubscribeRun: %v", err)
	}
	defer func() { _ = sub.Close() }()

	var (
		mu        sync.Mutex
		delivered []int64                 // via the Tailer (complete, in order)
		liveRecv  = map[int64]time.Time{} // receive time of a live message, by seq
	)
	backfill := s.Events()
	tailer := pubsub.NewTailer(runID, 0, backfill, func(env event.Envelope) {
		mu.Lock()
		delivered = append(delivered, env.Seq)
		mu.Unlock()
	}, 100)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case env, ok := <-sub.Events():
				if !ok {
					return
				}
				mu.Lock()
				if _, seen := liveRecv[env.Seq]; !seen {
					liveRecv[env.Seq] = time.Now()
				}
				mu.Unlock()
				if err := tailer.Offer(context.Background(), env); err != nil {
					t.Errorf("Offer: %v", err)
				}
			}
		}
	}()

	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(t.Context())

	close(stop)
	<-done
	if err := tailer.Catchup(context.Background()); err != nil {
		t.Fatalf("final Catchup: %v", err)
	}

	rows, err := s.Events().List(t.Context(), runID, 0, 10000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Completeness: the assembled feed is exactly seqs 1..N, no gaps or dupes.
	if len(delivered) != len(rows) {
		t.Fatalf("assembled feed has %d events, want %d (DB head)", len(delivered), len(rows))
	}
	for i, seq := range delivered {
		if seq != int64(i+1) {
			t.Fatalf("delivered[%d] = %d, want %d (gap or dupe)", i, seq, i+1)
		}
	}
	// Latency: over the events that arrived live (excludes the pre-subscription
	// ones healed by backfill), commit→received must meet the local budget.
	var latencies []time.Duration
	for seq, at := range liveRecv {
		if ct, ok := ts.commitTime(seq); ok {
			latencies = append(latencies, at.Sub(ct))
		}
	}
	if len(latencies) == 0 {
		t.Fatal("no events arrived via the live path — the publish path is not working")
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	median := latencies[len(latencies)/2]
	maxLat := latencies[len(latencies)-1]
	t.Logf("live commit→received latency: median=%s max=%s (n=%d of %d events)", median, maxLat, len(latencies), len(rows))
	if median > 100*time.Millisecond {
		t.Errorf("median live commit→received latency %s exceeds the 100ms local budget", median)
	}
}

// TestPubSubLossRecoversViaBackfill is DoD-2 (ADR-018): a lossy subscriber (every
// third live message dropped) still delivers exactly seqs 1..N with no gaps or
// dupes, because the Tailer detects each gap and heals it from the DB.
func TestPubSubLossRecoversViaBackfill(t *testing.T) {
	t.Parallel()
	prefix := uniqueEventPrefix(t)
	h := queuetest.New(t)
	client := h.Client()
	pub := pubsub.NewPublisher(client, pubsub.Options{Prefix: prefix})
	s, _, runID := setupWithSinkOn(t, h, fanoutDef, pub)
	defer func() { _ = pub.Close(context.Background()) }()

	sub, err := pubsub.SubscribeRun(t.Context(), client, prefix, runID, nil)
	if err != nil {
		t.Fatalf("SubscribeRun: %v", err)
	}
	defer func() { _ = sub.Close() }()

	var (
		mu        sync.Mutex
		delivered []int64
	)
	backfill := &countingBackfiller{inner: s.Events()}
	tailer := pubsub.NewTailer(runID, 0, backfill, func(env event.Envelope) {
		mu.Lock()
		delivered = append(delivered, env.Seq)
		mu.Unlock()
	}, 100)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case env, ok := <-sub.Events():
				if !ok {
					return
				}
				if env.Seq%3 == 0 { // simulate pub/sub loss
					continue
				}
				if err := tailer.Offer(context.Background(), env); err != nil {
					t.Errorf("Offer: %v", err)
				}
			}
		}
	}()

	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(t.Context())

	close(stop)
	<-done
	// Final catchup heals any trailing dropped events after the live tail stops.
	if err := tailer.Catchup(context.Background()); err != nil {
		t.Fatalf("final Catchup: %v", err)
	}

	rows, err := s.Events().List(t.Context(), runID, 0, 10000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != len(rows) {
		t.Fatalf("delivered %d events, want %d (DB head)", len(delivered), len(rows))
	}
	for i, seq := range delivered {
		if seq != int64(i+1) {
			t.Fatalf("delivered[%d] = %d, want %d (gaps or dupes despite backfill)", i, seq, i+1)
		}
	}
	if backfill.calls() == 0 {
		t.Fatal("expected the dropped messages to trigger a backfill, but none happened")
	}
}

// TestPublishFailureNeverAffectsEngine is DoD-3 (ADR-018): with a publisher over
// an unreachable Redis (every publish fails), the run still completes normally
// and its DB feed is intact — a publish failure never touches the transaction.
func TestPublishFailureNeverAffectsEngine(t *testing.T) {
	t.Parallel()
	// A publisher over an unroutable Redis: every PUBLISH fails.
	badClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = badClient.Close() }()
	m := &countingMetrics{}
	pub := pubsub.NewPublisher(badClient, pubsub.Options{Prefix: uniqueEventPrefix(t), PublishTimeout: 200 * time.Millisecond, Metrics: m})
	s, h, runID := setupWithSink(t, fanoutDef, pub)
	defer func() { _ = pub.Close(context.Background()) }()

	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)
	// The run reaches succeeded despite the broken publisher.
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(t.Context())

	// The DB feed is intact and gap-free.
	rows, err := s.Events().List(t.Context(), runID, 0, 10000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for i, row := range rows {
		if row.Seq != int64(i+1) {
			t.Fatalf("feed not gap-free at index %d: seq %d", i, row.Seq)
		}
	}
	// The publisher recorded failures — proving it tried and failed harmlessly.
	deadline := time.Now().Add(2 * time.Second)
	for m.failures() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.failures() == 0 {
		t.Error("expected publish failures against the unreachable Redis, got none")
	}
}

// setupWithSinkOn is setupWithSink but reusing an already-built harness (so the
// publisher and subscriber share its Redis client).
func setupWithSinkOn(t *testing.T, h *queuetest.Harness, defJSON string, pub store.EventSink) (*store.Store, *queuetest.Harness, uuid.UUID) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t), store.WithEventSink(pub))
	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	h.EnsureGroup(t.Context())
	return s, h, res.Run.ID
}

// countingBackfiller counts EventsAfter calls.
type countingBackfiller struct {
	inner pubsub.Backfiller
	n     int
	mu    sync.Mutex
}

func (c *countingBackfiller) EventsAfter(ctx context.Context, runID uuid.UUID, afterSeq int64, limit int32) ([]event.Envelope, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.inner.EventsAfter(ctx, runID, afterSeq, limit)
}

func (c *countingBackfiller) calls() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }
