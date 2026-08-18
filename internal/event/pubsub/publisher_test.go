package pubsub

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/event"
)

// fakeRedis captures Publish calls and can be scripted to fail. It satisfies
// redisPublisher, exercising the publisher's default publish path.
type fakeRedis struct {
	mu       sync.Mutex
	messages map[string][][]byte
	err      error
}

func newFakeRedis() *fakeRedis { return &fakeRedis{messages: map[string][][]byte{}} }

func (f *fakeRedis) Publish(ctx context.Context, channel string, message any) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		cmd.SetErr(f.err)
		return cmd
	}
	f.messages[channel] = append(f.messages[channel], message.([]byte))
	cmd.SetVal(1)
	return cmd
}

func (f *fakeRedis) got(channel string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.messages[channel]
}

// countMetrics counts every recorder call.
type countMetrics struct {
	mu        sync.Mutex
	published map[string]int
	failed    int
	dropped   int
	latencies []time.Duration
}

func newCountMetrics() *countMetrics { return &countMetrics{published: map[string]int{}} }

func (m *countMetrics) EventPublished(ch string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published[ch]++
}

func (m *countMetrics) PublishFailed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed++
}

func (m *countMetrics) PublishDropped(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped += n
}

func (m *countMetrics) PublishLatency(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latencies = append(m.latencies, d)
}

func (m *countMetrics) snapshot() (pubRun, pubFire, failed, dropped int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.published[ChannelRun], m.published[ChannelFirehose], m.failed, m.dropped
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestPublisherPublishesToRunAndFirehose(t *testing.T) {
	t.Parallel()
	fake := newFakeRedis()
	m := newCountMetrics()
	pub := NewPublisher(fake, Options{Prefix: "t", Metrics: m})
	defer func() { _ = pub.Close(context.Background()) }()

	runID := uuid.New()
	pub.EventsCommitted(context.Background(), []event.Envelope{env(runID, 1), env(runID, 2)})

	waitFor(t, func() bool {
		pr, pf, _, _ := m.snapshot()
		return pr == 2 && pf == 2
	})
	if got := fake.got(RunChannel("t", runID)); len(got) != 2 {
		t.Fatalf("run channel got %d messages, want 2", len(got))
	}
	if got := fake.got(FirehoseChannel("t")); len(got) != 2 {
		t.Fatalf("firehose got %d messages, want 2", len(got))
	}
	// The published bytes parse back to the same typed envelope.
	parsed, err := event.ParseEnvelope(fake.got(RunChannel("t", runID))[0])
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if parsed.Seq != 1 || parsed.RunID != runID {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestPublisherEmptyBatchNoop(t *testing.T) {
	t.Parallel()
	m := newCountMetrics()
	pub := NewPublisher(newFakeRedis(), Options{Metrics: m})
	defer func() { _ = pub.Close(context.Background()) }()
	pub.EventsCommitted(context.Background(), nil)
	// Nothing queued.
	if pub.QueueLen() != 0 {
		t.Fatalf("queue len = %d, want 0", pub.QueueLen())
	}
}

func TestPublisherFailureMetered(t *testing.T) {
	t.Parallel()
	m := newCountMetrics()
	pub := NewPublisher(newFakeRedis(), Options{Metrics: m})
	pub.SetPublishFn(func(context.Context, string, []byte) error { return errors.New("redis down") })
	defer func() { _ = pub.Close(context.Background()) }()

	pub.EventsCommitted(context.Background(), []event.Envelope{env(uuid.New(), 1)})
	// Two failures (run + firehose), no successes.
	waitFor(t, func() bool {
		_, _, failed, _ := m.snapshot()
		return failed == 2
	})
	pr, pf, _, _ := m.snapshot()
	if pr != 0 || pf != 0 {
		t.Fatalf("published run=%d fire=%d, want 0/0", pr, pf)
	}
}

// TestPublisherOverflowDrops fills the buffer while the drain goroutine is
// blocked, and asserts excess batches are dropped (metered) rather than blocking
// EventsCommitted.
func TestPublisherOverflowDrops(t *testing.T) {
	t.Parallel()
	m := newCountMetrics()
	release := make(chan struct{})
	pub := NewPublisher(newFakeRedis(), Options{Buffer: 1, Metrics: m})
	// The first publish blocks until released, so the drain goroutine is stuck
	// and the buffer (cap 1) fills.
	var once sync.Once
	pub.SetPublishFn(func(_ context.Context, _ string, _ []byte) error {
		once.Do(func() { <-release })
		return nil
	})

	runID := uuid.New()
	// 1st: taken by the drain goroutine (blocks). 2nd: fills the buffer.
	// 3rd..: dropped.
	for i := int64(1); i <= 5; i++ {
		pub.EventsCommitted(context.Background(), []event.Envelope{env(runID, i)})
	}
	waitFor(t, func() bool {
		_, _, _, dropped := m.snapshot()
		return dropped >= 1
	})
	close(release)
	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestPublisherCloseDrains(t *testing.T) {
	t.Parallel()
	fake := newFakeRedis()
	m := newCountMetrics()
	pub := NewPublisher(fake, Options{Prefix: "t", Metrics: m})

	runID := uuid.New()
	for i := int64(1); i <= 10; i++ {
		pub.EventsCommitted(context.Background(), []event.Envelope{env(runID, i)})
	}
	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After a clean Close, every queued batch was published.
	pr, pf, failed, dropped := m.snapshot()
	if pr != 10 || pf != 10 || failed != 0 || dropped != 0 {
		t.Fatalf("after drain: run=%d fire=%d failed=%d dropped=%d, want 10/10/0/0", pr, pf, failed, dropped)
	}
}

func TestPublisherCloseIdempotent(t *testing.T) {
	t.Parallel()
	pub := NewPublisher(newFakeRedis(), Options{})
	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
}

func TestPublisherLatencyRecorded(t *testing.T) {
	t.Parallel()
	m := newCountMetrics()
	// Injected clock: commit at t0, publish observed at t0+50ms.
	var mu sync.Mutex
	cur := time.Unix(1000, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	pub := NewPublisher(newFakeRedis(), Options{Metrics: m, Clock: clock})
	// The publish fn advances the clock by 50ms before returning.
	pub.SetPublishFn(func(context.Context, string, []byte) error {
		mu.Lock()
		cur = cur.Add(25 * time.Millisecond)
		mu.Unlock()
		return nil
	})
	pub.EventsCommitted(context.Background(), []event.Envelope{env(uuid.New(), 1)})
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.latencies) == 1
	})
	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// committed at 1000s; two publishes advanced 25ms each → latency 50ms.
	if m.latencies[0] != 50*time.Millisecond {
		t.Fatalf("latency = %s, want 50ms", m.latencies[0])
	}
}

// TestPublisherClosedClientErrors exercises the default publish path against a
// closed real client (no live Redis needed): every publish errors and is
// metered, and EventsCommitted never blocks.
func TestPublisherClosedClientErrors(t *testing.T) {
	t.Parallel()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // unroutable
	_ = client.Close()
	m := newCountMetrics()
	pub := NewPublisher(client, Options{Metrics: m, PublishTimeout: 200 * time.Millisecond})

	pub.EventsCommitted(context.Background(), []event.Envelope{env(uuid.New(), 1)})
	waitFor(t, func() bool {
		_, _, failed, _ := m.snapshot()
		return failed >= 1
	})
	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
