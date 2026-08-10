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
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
)

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
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	want := minimalEnvelope()
	h.Enqueue(ctx, want)

	// A stalls mid-task until "killed" (ctx cancel), then propagates the
	// cancellation as an error: entry delivered, leased, never acked.
	started := make(chan struct{})
	handlerA := func(hctx context.Context, _ queue.Delivery) error {
		close(started)
		<-hctx.Done()
		return hctx.Err()
	}
	a := h.Spawn("consumer-a", handlerA, queuetest.LeaseConfig(ttl))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("consumer A never received the entry")
	}
	if err := a.Kill(); err != nil {
		t.Fatalf("killing consumer A: Run returned %v, want nil", err)
	}
	killedAt := time.Now()

	got := make(chan queue.Delivery, 1)
	handlerB := func(_ context.Context, d queue.Delivery) error {
		got <- d
		return nil
	}
	h.Spawn("consumer-b", handlerB, queuetest.LeaseConfig(ttl))

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
	h.WaitStats(ctx, func(s queue.StreamStats) bool { return s.Pending == 0 })
	h.RequireHandledOncePerClaim()
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
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.Enqueue(ctx, minimalEnvelope())

	aStarted := make(chan struct{})
	aDone := make(chan struct{})
	handlerA := func(context.Context, queue.Delivery) error {
		close(aStarted)
		defer close(aDone)
		time.Sleep(taskDuration)
		return nil
	}
	h.Spawn("consumer-a", handlerA, queuetest.LeaseConfig(ttl))
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
	h.Spawn("consumer-b", handlerB, queuetest.LeaseConfig(ttl))

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
			for _, p := range h.PELSnapshot(ctx) {
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
	h.WaitStats(ctx, func(s queue.StreamStats) bool { return s.Pending == 0 })
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
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	want := minimalEnvelope()
	id := h.Enqueue(ctx, want)

	var handled atomic.Int64
	handler := func(context.Context, queue.Delivery) error {
		handled.Add(1)
		return errors.New("persistent failure")
	}
	poisoned := make(chan queue.PoisonMessage, 1)
	cfg := queuetest.LeaseConfig(ttl)
	cfg.PoisonThreshold = threshold
	cfg.PoisonHandler = func(_ context.Context, p queue.PoisonMessage) error {
		poisoned <- p
		return nil
	}
	h.Spawn("", handler, cfg)

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
	h.WaitStats(ctx, func(s queue.StreamStats) bool { return s.Pending == 0 })
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
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.EnqueueRaw(ctx, map[string]any{"v": "99", "junk": "payload"})

	handled := make(chan queue.Delivery, 1)
	handler := func(_ context.Context, d queue.Delivery) error {
		handled <- d
		return nil
	}
	poisoned := make(chan queue.PoisonMessage, 1)
	cfg := queuetest.LeaseConfig(ttl)
	cfg.PoisonThreshold = 1
	cfg.PoisonHandler = func(_ context.Context, p queue.PoisonMessage) error {
		poisoned <- p
		return nil
	}
	h.Spawn("", handler, cfg)

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
	h.WaitStats(ctx, func(s queue.StreamStats) bool { return s.Pending == 0 })
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
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.EnqueueRaw(ctx, map[string]any{"garbage": "yes"})
	h.Enqueue(ctx, envelopeForStep("valid"))

	handled := make(chan queue.Delivery, 1)
	handler := func(_ context.Context, d queue.Delivery) error {
		handled <- d
		return nil
	}
	cfg := queuetest.LeaseConfig(ttl)
	cfg.PoisonThreshold = 1 // PoisonHandler deliberately nil
	h.Spawn("", handler, cfg)

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
	if stats := h.Stats(ctx); stats.Pending != 1 {
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
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	for i := range n {
		h.Enqueue(ctx, envelopeForStep(fmt.Sprintf("step-%d", i)))
	}
	// A doomed consumer takes delivery of everything and dies without
	// acking: n leases, all destined to expire.
	if err := h.Client().XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: h.Queue().Group(), Consumer: "doomed", Streams: []string{h.Queue().Stream(), ">"},
		Count: n, Block: time.Second,
	}).Err(); err != nil {
		t.Fatalf("XREADGROUP as doomed consumer: %v", err)
	}

	deliveries := make(chan queue.Delivery, n)
	handler := func(_ context.Context, d queue.Delivery) error {
		deliveries <- d
		return nil
	}
	cfg := queuetest.LeaseConfig(ttl)
	cfg.Batch = batch
	h.Spawn("", handler, cfg)

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
	h.WaitStats(ctx, func(s queue.StreamStats) bool { return s.Pending == 0 })
	h.RequireHandledOncePerClaim()
}

// TestJanitorDeletesOrphanConsumers pins the janitor contract: a dead
// consumer with an empty PEL past the idle threshold is deleted; a dead
// consumer still holding pending entries is not (deleting it would drop
// PEL state); the janitor's own consumer survives.
func TestJanitorDeletesOrphanConsumers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	for _, step := range []string{"for-ghost", "for-holder"} {
		h.Enqueue(ctx, envelopeForStep(step))
	}
	// "ghost" reads one entry and acks it: zero pending, then goes silent.
	msg := h.ReadOne(ctx, "ghost")
	if err := h.Client().XAck(ctx, h.Queue().Stream(), h.Queue().Group(), msg.ID).Err(); err != nil {
		t.Fatalf("XACK as ghost: %v", err)
	}
	// "holder" reads one entry and goes silent without acking: one
	// pending entry, undeletable.
	h.ReadOne(ctx, "holder")

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
	h.Spawn("janitor-self", handler, cfg)

	deadline := time.Now().Add(opTimeout)
	for {
		consumers, err := h.Client().XInfoConsumers(ctx, h.Queue().Stream(), h.Queue().Group()).Result()
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

// TestHeartbeatDoesNotStealBackReclaimedLease pins the ownership guard
// (ADR-005, post-M3 audit): a consumer whose lease was legitimately taken
// over — here simulated with a direct XCLAIM to another consumer, as a
// reclaimer would after a stall — must not claim the entry back on its
// next heartbeat. The displaced handler still completes and acks (XACK
// needs no ownership); correctness rests on the claim_id fence.
func TestHeartbeatDoesNotStealBackReclaimedLease(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.Enqueue(ctx, minimalEnvelope())

	started := make(chan struct{})
	release := make(chan struct{})
	handlerA := func(hctx context.Context, _ queue.Delivery) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-hctx.Done():
			return hctx.Err()
		}
	}
	// Long TTL so nothing here can expire or reclaim; fast beats so A gets
	// many chances to (wrongly) steal the entry back while displaced.
	cfg := queue.ConsumerConfig{
		Block:             100 * time.Millisecond,
		LeaseTTL:          time.Minute,
		HeartbeatInterval: 50 * time.Millisecond,
	}
	h.Spawn("consumer-a", handlerA, cfg)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("consumer A never received the entry")
	}

	pel := h.PELSnapshot(ctx)
	if len(pel) != 1 {
		t.Fatalf("PEL = %+v, want exactly one entry", pel)
	}
	if err := h.Client().XClaimJustID(ctx, &redis.XClaimArgs{
		Stream:   h.Queue().Stream(),
		Group:    h.Queue().Group(),
		Consumer: "thief",
		MinIdle:  0,
		Messages: []string{pel[0].ID},
	}).Err(); err != nil {
		t.Fatalf("stealing the lease: %v", err)
	}

	// Across ~10 of A's heartbeat intervals, ownership must stay with the
	// thief: A's guarded beats detect the displacement and stop instead
	// of claiming the entry back.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, p := range h.PELSnapshot(ctx) {
			if p.Consumer != "thief" {
				t.Fatalf("PEL owner = %s, want thief (heartbeat stole the lease back)", p.Consumer)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	close(release)
	h.WaitStats(ctx, func(s queue.StreamStats) bool { return s.Pending == 0 })
	h.RequireHandledOncePerClaim()
}
