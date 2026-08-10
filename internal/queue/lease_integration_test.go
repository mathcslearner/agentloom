//go:build integration

package queue_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/queue"
)

// leaseCfg tunes the lease machinery to test speeds: the given TTL with
// derived heartbeat (TTL/3) and reclaim (TTL/2) intervals, and a short
// block so duty ticks are observed promptly. ADR-005's convention: chaos
// tests shrink the TTL rather than faking the Redis server clock, which is
// the one clock the protocol consumes without injecting.
func leaseCfg(ttl time.Duration) queue.ConsumerConfig {
	return queue.ConsumerConfig{Block: 100 * time.Millisecond, LeaseTTL: ttl}
}

// pelSnapshot returns the full PEL for the queue's group.
func pelSnapshot(ctx context.Context, tb testing.TB, client *redis.Client, q *queue.Queue) []redis.XPendingExt {
	tb.Helper()
	pending, err := client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.Stream(), Group: q.Group(), Start: "-", End: "+", Count: 100,
	}).Result()
	if err != nil {
		tb.Fatalf("XPENDING: %v", err)
	}
	return pending
}

// TestReclaimCompletesKilledConsumersTask is the ticket's flagship
// acceptance test (crash matrix W2/W3, queue slice): consumer A dies
// mid-task — heartbeats stop with it — and consumer B's reclaimer picks
// the expired lease up via XAUTOCLAIM within TTL + ε and completes it
// through the shared process path. The reclaim increments the delivery
// count (XAUTOCLAIM without JUSTID), which B observes as DeliveryCount 2.
func TestReclaimCompletesKilledConsumersTask(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	const ttl = 500 * time.Millisecond
	q := newTestQueue(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	want := minimalEnvelope()
	if _, err := q.Enqueue(ctx, want); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A stalls mid-task until "killed" (ctx cancel), then propagates the
	// cancellation as an error: entry delivered, leased, never acked.
	started := make(chan struct{})
	handlerA := func(hctx context.Context, _ queue.Delivery) error {
		close(started)
		<-hctx.Done()
		return hctx.Err()
	}
	stopA := startConsumer(t, q.NewConsumer("consumer-a", handlerA, leaseCfg(ttl)))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("consumer A never received the entry")
	}
	if err := stopA(); err != nil {
		t.Fatalf("killing consumer A: Run returned %v, want nil", err)
	}
	killedAt := time.Now()

	got := make(chan queue.Delivery, 1)
	handlerB := func(_ context.Context, d queue.Delivery) error {
		got <- d
		return nil
	}
	startConsumer(t, q.NewConsumer("consumer-b", handlerB, leaseCfg(ttl)))

	select {
	case d := <-got:
		// TTL + ε: idle must exceed the TTL, plus one reclaim tick (TTL/2)
		// to notice it, plus scheduling slack.
		if elapsed := time.Since(killedAt); elapsed > ttl+ttl/2+2*time.Second {
			t.Errorf("reclaim took %v after the kill, want within TTL+ε (TTL %v)", elapsed, ttl)
		}
		if d.DeliveryCount != 2 {
			t.Errorf("DeliveryCount = %d, want 2 (reclaim is a redelivery and increments the counter)", d.DeliveryCount)
		}
		if d.Envelope != want {
			t.Errorf("reclaimed envelope = %+v, want %+v", d.Envelope, want)
		}
	case <-ctx.Done():
		t.Fatal("consumer B never completed the reclaimed entry")
	}
	stats := waitForStats(ctx, t, q, func(s queue.StreamStats) bool { return s.Pending == 0 })
	if stats.Pending != 0 {
		t.Errorf("Pending = %d after B completed, want 0", stats.Pending)
	}
}

// TestHeartbeatPreventsReclaim pins the other half of the lease contract:
// a long task whose heartbeater keeps XCLAIMing JUSTID to self is never
// reclaimed across more than 3× the lease TTL, even with a hungry
// reclaimer ticking next to it — and because heartbeats use JUSTID, the
// delivery count stays 1 the whole time.
func TestHeartbeatPreventsReclaim(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	const ttl = 400 * time.Millisecond
	const taskDuration = 3*ttl + ttl/2 // > 3× TTL, per the acceptance criterion
	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := q.Enqueue(ctx, minimalEnvelope()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	aStarted := make(chan struct{})
	aDone := make(chan struct{})
	handlerA := func(context.Context, queue.Delivery) error {
		close(aStarted)
		defer close(aDone)
		time.Sleep(taskDuration)
		return nil
	}
	startConsumer(t, q.NewConsumer("consumer-a", handlerA, leaseCfg(ttl)))
	// B joins only once A holds the entry — otherwise B could win the
	// fresh-delivery race and the test would measure nothing.
	select {
	case <-aStarted:
	case <-ctx.Done():
		t.Fatal("consumer A never received the entry")
	}

	stolen := make(chan queue.Delivery, 1)
	handlerB := func(_ context.Context, d queue.Delivery) error {
		stolen <- d
		return nil
	}
	startConsumer(t, q.NewConsumer("consumer-b", handlerB, leaseCfg(ttl)))

	// While A executes, the entry must stay leased to A with delivery
	// count 1 — JUSTID heartbeats reset idle without touching the counter.
	sampled := false
poll:
	for {
		select {
		case <-aDone:
			break poll
		case <-ctx.Done():
			t.Fatal("consumer A never finished its long task")
		case <-time.After(ttl / 4):
			for _, p := range pelSnapshot(ctx, t, client, q) {
				sampled = true
				if p.Consumer != "consumer-a" {
					t.Errorf("PEL owner mid-task = %s, want consumer-a (heartbeat must prevent reclaim)", p.Consumer)
				}
				if p.RetryCount != 1 {
					t.Errorf("delivery count mid-task = %d, want 1 (JUSTID heartbeats must not inflate it)", p.RetryCount)
				}
			}
		}
	}
	if !sampled {
		t.Error("never observed the entry in the PEL mid-task; heartbeat assertions did not run")
	}

	select {
	case d := <-stolen:
		t.Fatalf("consumer B received %+v; a heartbeated task must never be reclaimed", d)
	default:
	}
	stats := waitForStats(ctx, t, q, func(s queue.StreamStats) bool { return s.Pending == 0 })
	if stats.Pending != 0 {
		t.Errorf("Pending = %d after A completed, want 0", stats.Pending)
	}
}

// TestPoisonDivertsAfterThreshold drives one entry through the designed
// error → reclaim → error ladder until its delivery count crosses the
// threshold, and asserts the consumer then diverts it to the poison
// callback instead of the handler, acking on the callback's nil return.
func TestPoisonDivertsAfterThreshold(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	const ttl = 300 * time.Millisecond
	const threshold = 2
	q := newTestQueue(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	want := minimalEnvelope()
	id, err := q.Enqueue(ctx, want)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var handled atomic.Int64
	handler := func(context.Context, queue.Delivery) error {
		handled.Add(1)
		return errors.New("persistent failure")
	}
	poisoned := make(chan queue.PoisonMessage, 1)
	cfg := leaseCfg(ttl)
	cfg.PoisonThreshold = threshold
	cfg.PoisonHandler = func(_ context.Context, p queue.PoisonMessage) error {
		poisoned <- p
		return nil
	}
	startConsumer(t, q.NewConsumer("", handler, cfg))

	select {
	case p := <-poisoned:
		if p.ID != id {
			t.Errorf("poison entry ID = %s, want %s", p.ID, id)
		}
		// Fresh delivery (1) and one reclaim (2) go to the handler; the
		// second reclaim (3) crosses the threshold.
		if p.DeliveryCount != threshold+1 {
			t.Errorf("poison DeliveryCount = %d, want %d", p.DeliveryCount, threshold+1)
		}
		if p.Envelope == nil || *p.Envelope != want {
			t.Errorf("poison Envelope = %+v, want %+v", p.Envelope, want)
		}
	case <-ctx.Done():
		t.Fatal("poison callback never invoked")
	}
	if n := handled.Load(); n != threshold {
		t.Errorf("handler invoked %d times, want %d (deliveries at or below the threshold)", n, threshold)
	}
	stats := waitForStats(ctx, t, q, func(s queue.StreamStats) bool { return s.Pending == 0 })
	if stats.Pending != 0 {
		t.Errorf("Pending = %d after poison ack, want 0", stats.Pending)
	}
}

// TestPoisonMalformedEnvelopePreservesContents pins ADR-005's promise that
// an undecodable entry — which can never reach the handler — is eventually
// diverted to the poison path with its raw contents intact, instead of
// being silently dropped or spinning forever.
func TestPoisonMalformedEnvelopePreservesContents(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	const ttl = 300 * time.Millisecond
	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.Stream(),
		ID:     "*",
		Values: map[string]any{"v": "99", "junk": "payload"},
	}).Err(); err != nil {
		t.Fatalf("raw XADD: %v", err)
	}

	handled := make(chan queue.Delivery, 1)
	handler := func(_ context.Context, d queue.Delivery) error {
		handled <- d
		return nil
	}
	poisoned := make(chan queue.PoisonMessage, 1)
	cfg := leaseCfg(ttl)
	cfg.PoisonThreshold = 1
	cfg.PoisonHandler = func(_ context.Context, p queue.PoisonMessage) error {
		poisoned <- p
		return nil
	}
	startConsumer(t, q.NewConsumer("", handler, cfg))

	select {
	case p := <-poisoned:
		if p.Envelope != nil {
			t.Errorf("poison Envelope = %+v, want nil for an undecodable entry", p.Envelope)
		}
		if p.Values["junk"] != "payload" || p.Values["v"] != "99" {
			t.Errorf("poison Values = %+v, want the raw contents preserved", p.Values)
		}
		if p.DeliveryCount != 2 {
			t.Errorf("poison DeliveryCount = %d, want 2 (fresh delivery + one reclaim)", p.DeliveryCount)
		}
	case <-ctx.Done():
		t.Fatal("poison callback never invoked for the malformed entry")
	}
	select {
	case d := <-handled:
		t.Errorf("handler saw %+v; a malformed entry must never reach it", d)
	default:
	}
	stats := waitForStats(ctx, t, q, func(s queue.StreamStats) bool { return s.Pending == 0 })
	if stats.Pending != 0 {
		t.Errorf("Pending = %d after poison ack, want 0", stats.Pending)
	}
}

// TestPoisonWithoutHandlerLeavesPending pins the no-silent-drop rule when
// no poison callback is configured: the entry stays visibly pending —
// logged and re-surfaced every reclaim pass — while the loop keeps serving
// other entries.
func TestPoisonWithoutHandlerLeavesPending(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	const ttl = 300 * time.Millisecond
	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.Stream(),
		ID:     "*",
		Values: map[string]any{"garbage": "yes"},
	}).Err(); err != nil {
		t.Fatalf("raw XADD: %v", err)
	}
	if _, err := q.Enqueue(ctx, envelopeForStep("valid")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	handled := make(chan queue.Delivery, 1)
	handler := func(_ context.Context, d queue.Delivery) error {
		handled <- d
		return nil
	}
	cfg := leaseCfg(ttl)
	cfg.PoisonThreshold = 1 // PoisonHandler deliberately nil
	startConsumer(t, q.NewConsumer("", handler, cfg))

	select {
	case d := <-handled:
		if d.Envelope.StepID != "valid" {
			t.Errorf("handled step %q, want %q", d.Envelope.StepID, "valid")
		}
	case <-ctx.Done():
		t.Fatal("valid entry behind the poison one was never handled")
	}
	// Let several reclaim passes cross the threshold and hit the nil
	// callback; the entry must still be pending afterwards.
	time.Sleep(4 * ttl)
	stats, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 1 {
		t.Errorf("Pending = %d, want 1 (poison entry with no callback must stay pending, never dropped)", stats.Pending)
	}
}

// TestReclaimCursorSweepsFullPEL proves cursor handling: with more expired
// leases than one XAUTOCLAIM batch, successive reclaim ticks walk the
// cursor across the whole PEL and every entry is recovered.
func TestReclaimCursorSweepsFullPEL(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	const ttl = 300 * time.Millisecond
	const n = 10
	const batch = 4 // forces at least three reclaim ticks
	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for i := range n {
		if _, err := q.Enqueue(ctx, envelopeForStep(fmt.Sprintf("step-%d", i))); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	// A doomed consumer takes delivery of everything and dies without
	// acking: n leases, all destined to expire.
	if err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: q.Group(), Consumer: "doomed", Streams: []string{q.Stream(), ">"},
		Count: n, Block: time.Second,
	}).Err(); err != nil {
		t.Fatalf("XREADGROUP as doomed consumer: %v", err)
	}

	deliveries := make(chan queue.Delivery, n)
	handler := func(_ context.Context, d queue.Delivery) error {
		deliveries <- d
		return nil
	}
	cfg := leaseCfg(ttl)
	cfg.Batch = batch
	startConsumer(t, q.NewConsumer("", handler, cfg))

	seen := make(map[string]int)
	for range n {
		select {
		case d := <-deliveries:
			seen[d.Envelope.StepID]++
			if d.DeliveryCount != 2 {
				t.Errorf("step %s: DeliveryCount = %d, want 2 (one fresh delivery to doomed, one reclaim)", d.Envelope.StepID, d.DeliveryCount)
			}
		case <-ctx.Done():
			t.Fatalf("timed out after %d/%d reclaimed deliveries", len(seen), n)
		}
	}
	for step, count := range seen {
		if count != 1 {
			t.Errorf("step %s handled %d times, want 1", step, count)
		}
	}
	if len(seen) != n {
		t.Errorf("handled %d distinct steps, want %d", len(seen), n)
	}
	stats := waitForStats(ctx, t, q, func(s queue.StreamStats) bool { return s.Pending == 0 })
	if stats.Pending != 0 {
		t.Errorf("Pending = %d after full sweep, want 0", stats.Pending)
	}
}

// TestJanitorDeletesOrphanConsumers pins the janitor contract: a dead
// consumer with an empty PEL past the idle threshold is deleted; a dead
// consumer still holding pending entries is not (deleting it would drop
// PEL state); the janitor's own consumer survives.
func TestJanitorDeletesOrphanConsumers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for _, step := range []string{"for-ghost", "for-holder"} {
		if _, err := q.Enqueue(ctx, envelopeForStep(step)); err != nil {
			t.Fatalf("Enqueue %s: %v", step, err)
		}
	}
	// "ghost" reads one entry and acks it: zero pending, then goes silent.
	msg := readOne(ctx, t, client, q, "ghost")
	if err := client.XAck(ctx, q.Stream(), q.Group(), msg.ID).Err(); err != nil {
		t.Fatalf("XACK as ghost: %v", err)
	}
	// "holder" reads one entry and goes silent without acking: one
	// pending entry, undeletable.
	readOne(ctx, t, client, q, "holder")

	cfg := queue.ConsumerConfig{
		Block: 100 * time.Millisecond,
		// A long TTL keeps the reclaimer away from holder's entry, so the
		// zero-pending guard is what's under test — not reclaim emptying
		// holder's PEL first.
		LeaseTTL:             time.Minute,
		JanitorInterval:      200 * time.Millisecond,
		JanitorIdleThreshold: 400 * time.Millisecond,
	}
	handler := func(context.Context, queue.Delivery) error { return nil }
	c := q.NewConsumer("janitor-self", handler, cfg)
	startConsumer(t, c)

	deadline := time.Now().Add(opTimeout)
	for {
		consumers, err := client.XInfoConsumers(ctx, q.Stream(), q.Group()).Result()
		if err != nil {
			t.Fatalf("XINFO CONSUMERS: %v", err)
		}
		names := make(map[string]int64, len(consumers))
		for _, con := range consumers {
			names[con.Name] = con.Pending
		}
		if _, ok := names["ghost"]; !ok {
			if _, ok := names["holder"]; !ok {
				t.Error("holder was deleted; a consumer with pending entries must survive the janitor")
			}
			if _, ok := names["janitor-self"]; !ok {
				t.Error("the janitor deleted its own consumer record")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("janitor never deleted ghost; consumers = %v", names)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
