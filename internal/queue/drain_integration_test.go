//go:build integration

package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
)

// Graceful shutdown & drain (ticket 5.7). These tests pin the consumer's
// two-phase shutdown: canceling Run's context stops claiming but not
// working — the in-flight handler and every already-delivered entry
// finish under the work context, which survives the cancellation until
// the drain deadline. The discriminator against the pre-5.7 behavior is
// the hang: a Hang action resolves to ctx.Err() when its handler context
// is canceled, so a hang that stays blocked across the cancellation — and
// then succeeds on Release — proves the handler ran under a context the
// shutdown did not touch.

// drainConfig is FastConfig with a generous drain budget.
func drainConfig(timeout time.Duration) queue.ConsumerConfig {
	cfg := queuetest.FastConfig()
	cfg.DrainTimeout = timeout
	return cfg
}

// consumerRegistered reports whether the consumer-group member with the
// given name currently exists.
func consumerRegistered(t *testing.T, h *queuetest.Harness, name string) bool {
	t.Helper()
	consumers, err := h.Client().XInfoConsumers(context.Background(), h.Queue().Stream(), h.Queue().Group()).Result()
	if err != nil {
		t.Fatalf("XINFO CONSUMERS: %v", err)
	}
	for _, c := range consumers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// waitInFlight waits until the journal shows exactly n invocations with
// the last one still running (End unset) — the "handler started"
// synchronization point.
func waitInFlight(ctx context.Context, h *queuetest.Harness, n int) {
	h.WaitJournal(ctx, func(invs []queuetest.Invocation) bool {
		return len(invs) == n && invs[n-1].End.IsZero()
	})
}

// TestConsumerDrainFinishesInFlightHandler proves the core 5.7 contract:
// a consumer interrupted mid-handler does not cancel the handler — the
// hang stays blocked through the interrupt, completes once released, the
// entry acks, and Run returns nil. A fully clean drain also deregisters
// the consumer from the group, so a graceful restart strands no orphan
// for the janitor.
func TestConsumerDrainFinishesInFlightHandler(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	script := queuetest.NewScript().OnStep("slow", queuetest.Hang())
	c := h.SpawnScript("drainer", script, drainConfig(10*time.Second))
	h.EnqueueStep(ctx, "slow")
	waitInFlight(ctx, h, 1)

	c.Interrupt()
	// The interrupt must not resolve the hang: releasing it afterwards is
	// what lets the drain finish. If the pre-5.7 behavior regressed, the
	// hang would already have returned ctx.Err() and the invocation below
	// would carry an error and no ack.
	script.Release("slow")
	if err := c.Kill(); err != nil {
		t.Fatalf("Run returned %v after drain, want nil", err)
	}

	invs := h.Journal().Invocations()
	if len(invs) != 1 {
		t.Fatalf("journal has %d invocations, want 1: %+v", len(invs), invs)
	}
	if invs[0].Err != nil {
		t.Errorf("in-flight handler resolved with %v, want nil (drain must not cancel it)", invs[0].Err)
	}
	h.WaitQuiescent(ctx)
	if consumerRegistered(t, h, "drainer") {
		t.Error("consumer still registered after a clean drain; want self-deregistration")
	}
}

// TestConsumerDrainProcessesBatchRemainder proves a drain covers the
// whole PEL in hand, not just the running handler: with a batch of four
// delivered entries and the first hanging, an interrupt mid-first must
// still hand the remaining three to the handler — they are leases this
// consumer owns, and abandoning them would cost the fleet a reclaim cycle
// each (the "zero reclaim churn" half of the ticket's acceptance).
func TestConsumerDrainProcessesBatchRemainder(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	steps := []string{"s1", "s2", "s3", "s4"}
	for _, s := range steps {
		h.EnqueueStep(ctx, s)
	}

	script := queuetest.NewScript().OnStep("s1", queuetest.Hang())
	cfg := drainConfig(10 * time.Second)
	cfg.Batch = len(steps)
	c := h.SpawnScript("batch-drainer", script, cfg)

	// One read delivers all four (they were on the stream before the
	// first XREADGROUP): the whole batch is leased before s1 hangs.
	h.WaitStats(ctx, func(s queue.StreamStats) bool { return s.Pending == int64(len(steps)) })
	waitInFlight(ctx, h, 1)

	c.Interrupt()
	script.Release("s1")
	if err := c.Kill(); err != nil {
		t.Fatalf("Run returned %v after drain, want nil", err)
	}

	seen := make(map[string]int)
	for _, inv := range h.Journal().Invocations() {
		seen[inv.Step]++
		if inv.Err != nil {
			t.Errorf("step %s resolved with %v, want nil", inv.Step, inv.Err)
		}
	}
	for _, s := range steps {
		if seen[s] != 1 {
			t.Errorf("step %s handled %d times, want exactly once during drain", s, seen[s])
		}
	}
	h.RequireHandledOncePerClaim()
	h.WaitQuiescent(ctx)
	if consumerRegistered(t, h, "batch-drainer") {
		t.Error("consumer still registered after a clean drain; want self-deregistration")
	}
}

// TestConsumerDrainDeadlineAbandons proves the bounded half of the
// contract: a handler that never finishes is abandoned when the drain
// budget elapses — the work context is canceled (the hang resolves to
// context.Canceled), the entry stays pending un-acked so its lease
// expires naturally into reclaim, and the consumer stays registered
// because deregistering would drop that pending state.
func TestConsumerDrainDeadlineAbandons(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	const drainTimeout = 300 * time.Millisecond
	h := queuetest.New(t)
	script := queuetest.NewScript().OnStep("stuck", queuetest.Hang())
	c := h.SpawnScript("abandoner", script, drainConfig(drainTimeout))
	h.EnqueueStep(ctx, "stuck")
	waitInFlight(ctx, h, 1)

	start := time.Now()
	c.Interrupt()
	if err := c.Kill(); err != nil {
		t.Fatalf("Run returned %v after abandoning, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < drainTimeout {
		t.Errorf("Run returned after %v, before the %v drain deadline — the hang cannot have been abandoned", elapsed, drainTimeout)
	}

	invs := h.Journal().Invocations()
	if len(invs) != 1 {
		t.Fatalf("journal has %d invocations, want 1: %+v", len(invs), invs)
	}
	if !errors.Is(invs[0].Err, context.Canceled) {
		t.Errorf("abandoned handler resolved with %v, want context.Canceled from the drain deadline", invs[0].Err)
	}
	if pending := h.Stats(ctx).Pending; pending != 1 {
		t.Errorf("PEL has %d entries, want 1: an abandoned entry must stay pending for reclaim", pending)
	}
	if !consumerRegistered(t, h, "abandoner") {
		t.Error("consumer deregistered despite a pending entry; DELCONSUMER would drop its lease state")
	}
}
