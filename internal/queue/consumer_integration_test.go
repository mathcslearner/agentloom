//go:build integration

package queue_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
)

// envelopeForStep is minimalEnvelope with a distinguishing step ID.
func envelopeForStep(step string) queue.Envelope {
	env := minimalEnvelope()
	env.StepID = step
	return env
}

// TestConsumerProcessesBatches proves the happy path across multiple
// XREADGROUP batches: every enqueued envelope is handled exactly once with
// DeliveryCount 1, acked, and the loop shuts down cleanly.
func TestConsumerProcessesBatches(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	const n = 40 // several batches at Batch: 16
	for i := range n {
		h.Enqueue(ctx, envelopeForStep(fmt.Sprintf("step-%d", i)))
	}

	deliveries := make(chan queue.Delivery, n)
	handler := func(_ context.Context, d queue.Delivery) error {
		deliveries <- d
		return nil
	}
	c := h.Spawn("", handler, queuetest.FastConfig())

	seen := make(map[string]int)
	for range n {
		select {
		case d := <-deliveries:
			seen[d.Envelope.StepID]++
			if d.DeliveryCount != 1 {
				t.Errorf("step %s: DeliveryCount = %d, want 1 (fresh delivery)", d.Envelope.StepID, d.DeliveryCount)
			}
		case <-ctx.Done():
			t.Fatalf("timed out after %d/%d deliveries", len(seen), n)
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

	stats := h.WaitStats(ctx, func(s queue.StreamStats) bool { return s.Pending == 0 })
	if stats.Length != n {
		t.Errorf("stream length = %d, want %d (no trimming)", stats.Length, n)
	}
	if err := c.Kill(); err != nil {
		t.Errorf("Run returned %v on clean shutdown, want nil", err)
	}
}

// TestConsumerHandlerErrorLeavesPending pins the nack contract: a handler
// error means no ACK, so the entry stays in the PEL for reclaim (3.4).
func TestConsumerHandlerErrorLeavesPending(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.Enqueue(ctx, minimalEnvelope())

	handled := make(chan struct{}, 1)
	handler := func(context.Context, queue.Delivery) error {
		handled <- struct{}{}
		return errors.New("transient failure")
	}
	c := h.Spawn("", handler, queuetest.FastConfig())

	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("handler never invoked")
	}
	if err := c.Kill(); err != nil { // stop first: rules out an ACK racing the assertion
		t.Errorf("Run returned %v, want nil", err)
	}
	if stats := h.Stats(ctx); stats.Pending != 1 {
		t.Errorf("Pending = %d after handler error, want 1 (no ACK)", stats.Pending)
	}
}

// TestConsumerPanicContained proves one poisoned message cannot kill the
// loop: the panicking delivery stays pending, later deliveries in the same
// batch are still handled, and shutdown stays clean.
func TestConsumerPanicContained(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.Enqueue(ctx, envelopeForStep("boom"))
	h.Enqueue(ctx, envelopeForStep("fine"))

	handled := make(chan string, 2)
	handler := func(_ context.Context, d queue.Delivery) error {
		if d.Envelope.StepID == "boom" {
			panic("scripted handler panic")
		}
		handled <- d.Envelope.StepID
		return nil
	}
	c := h.Spawn("", handler, queuetest.FastConfig())

	select {
	case step := <-handled:
		if step != "fine" {
			t.Errorf("handled step %q, want %q", step, "fine")
		}
	case <-ctx.Done():
		t.Fatal("loop died after panic: the following delivery was never handled")
	}
	if err := c.Kill(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if stats := h.Stats(ctx); stats.Pending != 1 {
		t.Errorf("Pending = %d, want 1 (the panicked delivery, un-acked)", stats.Pending)
	}
}

// TestConsumerMalformedEntryLeftPending pins ADR-005's no-silent-drop rule:
// an undecodable entry is never acked (its delivery count will walk it into
// the poison path) and never reaches the handler; the loop moves on.
func TestConsumerMalformedEntryLeftPending(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.EnqueueRaw(ctx, map[string]any{"v": "99", "junk": "yes"})
	h.Enqueue(ctx, envelopeForStep("valid"))

	handled := make(chan queue.Delivery, 2)
	handler := func(_ context.Context, d queue.Delivery) error {
		handled <- d
		return nil
	}
	c := h.Spawn("", handler, queuetest.FastConfig())

	select {
	case d := <-handled:
		if d.Envelope.StepID != "valid" {
			t.Errorf("handler saw step %q, want %q", d.Envelope.StepID, "valid")
		}
	case <-ctx.Done():
		t.Fatal("valid entry behind the malformed one was never handled")
	}
	if err := c.Kill(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	select {
	case d := <-handled:
		t.Errorf("handler saw a second delivery %+v; the malformed entry must never reach it", d)
	default:
	}
	if stats := h.Stats(ctx); stats.Pending != 1 {
		t.Errorf("Pending = %d, want 1 (the malformed entry, un-acked)", stats.Pending)
	}
}

// TestConsumerShutdownDrainsInFlight pins the drain contract: cancellation
// mid-handler waits for the handler, and a success during shutdown still
// acks (the ACK runs on a detached context).
func TestConsumerShutdownDrainsInFlight(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.Enqueue(ctx, minimalEnvelope())

	started := make(chan struct{})
	finished := make(chan struct{})
	handler := func(context.Context, queue.Delivery) error {
		close(started)
		// Ignore cancellation deliberately: simulate a handler that decides
		// to finish its (quick) work during shutdown.
		time.Sleep(300 * time.Millisecond)
		close(finished)
		return nil
	}
	c := h.Spawn("", handler, queuetest.FastConfig())

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("handler never started")
	}
	if err := c.Kill(); err != nil { // cancels mid-handler, waits for Run
		t.Errorf("Run returned %v, want nil", err)
	}
	select {
	case <-finished:
	default:
		t.Error("Run returned before the in-flight handler finished")
	}
	if stats := h.Stats(ctx); stats.Pending != 0 {
		t.Errorf("Pending = %d, want 0 (success during shutdown must still ack)", stats.Pending)
	}
}

// TestConsumerShutdownDuringIdleBlock pins the shutdown-latency contract:
// go-redis does not interrupt a blocked XREADGROUP mid-read, so
// cancellation is observed between blocks and shutdown from idle is
// bounded by the configured Block — ADR-005's stated rationale for
// keeping the default at 5s. A 200ms Block must shut down well before the
// default would have.
func TestConsumerShutdownDuringIdleBlock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	handler := func(context.Context, queue.Delivery) error { return nil }
	c := h.Spawn("", handler, queue.ConsumerConfig{Block: 200 * time.Millisecond})

	time.Sleep(100 * time.Millisecond) // let the loop enter its idle block
	begin := time.Now()
	if err := c.Kill(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Errorf("shutdown from idle took %v, want bounded by the 200ms Block (plus slack)", elapsed)
	}
}

// TestKillPreAckRedelivery is the at-least-once acceptance test (crash
// matrix W4 for 3.3's slice): consumer A is killed after delivery but
// before ACK, the entry survives in the PEL, a reclaim hands it to
// consumer B with an incremented delivery count, and B's handler path
// completes and acks it. The XAUTOCLAIM here is issued by the test, and B
// replays the entry through the exported Process — deliberately kept
// manual even though the queuetest harness can now express this scenario
// end to end (TestKillAtPreAck there), because this test pins the
// export's contract: reclaimed entries enter the same per-message path.
func TestKillPreAckRedelivery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	q := h.Queue()
	client := h.Client()
	h.EnsureGroup(ctx)
	want := minimalEnvelope()
	id := h.Enqueue(ctx, want)

	// Consumer A stalls in the handler until "killed" (ctx cancel), then
	// propagates the cancellation as an error: delivered, never acked.
	started := make(chan struct{})
	handlerA := func(hctx context.Context, _ queue.Delivery) error {
		close(started)
		<-hctx.Done()
		return hctx.Err()
	}
	a := h.Spawn("consumer-a", handlerA, queuetest.FastConfig())
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("consumer A never received the entry")
	}
	if err := a.Kill(); err != nil {
		t.Fatalf("killing consumer A: Run returned %v, want nil", err)
	}

	pending := h.PELSnapshot(ctx)
	if len(pending) != 1 || pending[0].ID != id || pending[0].Consumer != "consumer-a" || pending[0].RetryCount != 1 {
		t.Fatalf("PEL after kill = %+v, want entry %s owned by consumer-a with delivery count 1", pending, id)
	}

	// Reclaim to B — what 3.4's reclaimer does on lease expiry (min-idle
	// 0 stands in for an expired TTL; the TTL threshold is 3.4's subject).
	msgs, _, err := client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: q.Stream(), Group: q.Group(), Consumer: "consumer-b",
		MinIdle: 0, Start: "0", Count: 10,
	}).Result()
	if err != nil {
		t.Fatalf("XAUTOCLAIM: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != id {
		t.Fatalf("XAUTOCLAIM returned %+v, want exactly entry %s", msgs, id)
	}
	pending = h.PELSnapshot(ctx)
	if len(pending) != 1 || pending[0].Consumer != "consumer-b" || pending[0].RetryCount != 2 {
		t.Fatalf("PEL after reclaim = %+v, want entry owned by consumer-b with delivery count 2", pending)
	}

	// B replays the reclaimed entry through the shared handler path.
	var got queue.Delivery
	handlerB := func(_ context.Context, d queue.Delivery) error {
		got = d
		return nil
	}
	b := q.NewConsumer("consumer-b", handlerB, queuetest.FastConfig())
	if acked := b.Process(ctx, msgs[0], pending[0].RetryCount); !acked {
		t.Fatal("Process on the redelivered entry reported no ACK")
	}
	if got.ID != id || got.Envelope != want || got.DeliveryCount != 2 {
		t.Errorf("redelivery seen by B = %+v, want ID %s, envelope %+v, DeliveryCount 2", got, id, want)
	}
	if stats := h.Stats(ctx); stats.Pending != 0 {
		t.Errorf("Pending = %d after B completed, want 0", stats.Pending)
	}
}
