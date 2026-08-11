//go:build integration

package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 4.4's dispatcher suite: SKIP LOCKED drain correctness under
// concurrency and ADR-005's producer crash cells — P1 (XADD landed, the
// drain transaction did not) absorbed at claim, P2 (XADD lost after the
// drain committed) healed by the reconciler (reconcile_integration_test).

// lostEnqueuer reports success without touching Redis — the lost-XADD
// shape behind ADR-005 P2/R1(a): the drain transaction commits (row
// deleted) but no message exists anywhere.
type lostEnqueuer struct{}

func (lostEnqueuer) Enqueue(context.Context, queue.Envelope) (string, error) {
	return "0-0", nil
}

// failAfterEnqueuer performs the real XADD, then reports failure for the
// first n calls — the P1 crash window (entry in stream, outbox row kept).
type failAfterEnqueuer struct {
	inner engine.Enqueuer
	n     atomic.Int64
}

func (f *failAfterEnqueuer) Enqueue(ctx context.Context, env queue.Envelope) (string, error) {
	id, err := f.inner.Enqueue(ctx, env)
	if err != nil {
		return id, err
	}
	if f.n.Add(-1) >= 0 {
		return "", errors.New("injected post-XADD drain failure")
	}
	return id, nil
}

// drainOnce runs one drain transaction through d, failing the test on an
// unexpected error.
func drainOnce(t *testing.T, d *engine.Dispatcher) int {
	t.Helper()
	n, err := d.DrainOnce(t.Context())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	return n
}

// wideDef builds a definition of n independent noop entry steps — n
// outbox rows at instantiation, no execution ordering.
func wideDef(n int) string {
	steps := make([]string, n)
	for i := range n {
		steps[i] = fmt.Sprintf(`{"id": "s%02d", "type": "noop"}`, i)
	}
	return fmt.Sprintf(`{
		"schema_version": 1,
		"name": "wide-fanout",
		"steps": [%s],
		"edges": []
	}`, strings.Join(steps, ","))
}

// TestConcurrentDrainersDispatchExactlyOnce: 4 dispatchers over one
// outbox, small batches, no consumers — every step's envelope reaches the
// stream exactly once, because SKIP LOCKED partitions the drainers onto
// disjoint rows (ticket 4.4 acceptance: no duplicate dispatch).
func TestConcurrentDrainersDispatchExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const steps = 12
	s, h, _ := setup(t, wideDef(steps))

	ctxDrain, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i := range 4 {
		d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{
			Interval: 5 * time.Millisecond, Batch: 3,
		})
		if err != nil {
			t.Fatalf("NewDispatcher %d: %v", i, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Run(ctxDrain)
		}()
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		tasks, err := s.Outbox().List(ctx, steps)
		if err != nil {
			t.Fatalf("listing outbox: %v", err)
		}
		if len(tasks) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox never drained: %d rows remain", len(tasks))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	// Exactly one stream entry per step — no duplicates from any drainer.
	perStep := map[string]int{}
	for _, env := range h.StreamEnvelopes(ctx) {
		perStep[env.StepID]++
	}
	if len(perStep) != steps {
		t.Errorf("stream has envelopes for %d steps, want %d", len(perStep), steps)
	}
	for stepID, n := range perStep {
		if n != 1 {
			t.Errorf("step %q dispatched %d times, want exactly once", stepID, n)
		}
	}
}

// cancelAfterEnqueuer performs the real XADD, then cancels the drain's
// context — everything after the XADD (the delete, the commit) fails, so
// the whole transaction rolls back. This is the P1 *rollback* branch
// (post-M4 audit): distinct from failAfterEnqueuer, whose partial-batch
// pass still commits.
type cancelAfterEnqueuer struct {
	inner  engine.Enqueuer
	cancel context.CancelFunc
	n      atomic.Int64
}

func (c *cancelAfterEnqueuer) Enqueue(ctx context.Context, env queue.Envelope) (string, error) {
	id, err := c.inner.Enqueue(ctx, env)
	if err == nil && c.n.Add(-1) >= 0 {
		c.cancel()
	}
	return id, err
}

// TestDrainRollbackKeepsRowAndEntry pins DrainOnce's rollback branch: a
// drain transaction that dies after the XADD leaves the entry in the
// stream AND the row in the outbox — the P1 shape via rollback rather
// than partial commit. The re-drain then produces the duplicate pair,
// absorbed at claim.
func TestDrainRollbackKeepsRowAndEntry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, singleNoop)

	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	crashing := &cancelAfterEnqueuer{inner: h.Queue(), cancel: ccancel}
	crashing.n.Store(1)
	d, err := engine.NewDispatcher(s, crashing, engine.DispatcherConfig{
		Interval: time.Hour, Batch: 16, // manual DrainOnce only
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	// The pass rolls back: XADD landed, nothing committed.
	if n, err := d.DrainOnce(cctx); err == nil || n != 0 {
		t.Fatalf("DrainOnce with mid-tx crash = (%d, %v), want (0, error)", n, err)
	}
	tasks, err := s.Outbox().List(ctx, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("outbox after rolled-back drain = %+v (err %v), want the row kept", tasks, err)
	}
	if envs := h.StreamEnvelopes(ctx); len(envs) != 1 {
		t.Fatalf("stream entries after rolled-back drain = %d, want the orphaned XADD", len(envs))
	}

	// A healthy dispatcher re-drains into the duplicate pair; exactly one
	// execution survives the claim CAS.
	d2, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{
		Interval: time.Hour, Batch: 16,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if n := drainOnce(t, d2); n != 1 {
		t.Fatalf("re-drain dispatched %d rows, want 1", n)
	}
	if envs := h.StreamEnvelopes(ctx); len(envs) != 2 {
		t.Fatalf("stream entries = %d, want the P1 duplicate pair", len(envs))
	}

	counting := &countingNoop{}
	cfg := queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1}
	h.Spawn("worker-a", newEngine(t, s, counting, "worker-a").Handle, cfg)
	h.Spawn("worker-b", newEngine(t, s, counting, "worker-b").Handle, cfg)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	if got := counting.calls.Load(); got != 1 {
		t.Errorf("executor ran %d times, want exactly 1 (duplicate must ACK-drop)", got)
	}
	requireSingleAttempts(t, s, runID, []string{"only"})
	waitRun(t, s, runID, store.RunStatusSucceeded)
	requireOutboxEmpty(t, s)
}

// TestDrainFailureAfterXADDAbsorbedAtClaim is ADR-005 P1: the XADD lands
// but the drain does not commit the delete, so the row re-drains and the
// step gets a duplicate stream entry — which the claim CAS ACK-drops.
// This is the ticket's "dupes that do occur are ACK-dropped at claim".
func TestDrainFailureAfterXADDAbsorbedAtClaim(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, singleNoop)

	flaky := &failAfterEnqueuer{inner: h.Queue()}
	flaky.n.Store(1) // first XADD lands, then the drain "crashes"
	d, err := engine.NewDispatcher(s, flaky, engine.DispatcherConfig{
		Interval: time.Hour, Batch: 16, // manual DrainOnce only
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	// First pass: entry hits the stream, row survives the failure.
	if n, err := d.DrainOnce(ctx); err == nil || n != 0 {
		t.Fatalf("DrainOnce with injected failure = (%d, %v), want (0, the injected error)", n, err)
	}
	tasks, err := s.Outbox().List(ctx, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("outbox after failed drain = %+v (err %v), want the row kept", tasks, err)
	}

	// Second pass re-dispatches: two live entries for one step.
	if n := drainOnce(t, d); n != 1 {
		t.Fatalf("re-drain dispatched %d rows, want 1", n)
	}
	envs := h.StreamEnvelopes(ctx)
	if len(envs) != 2 {
		t.Fatalf("stream entries = %d, want the P1 duplicate pair", len(envs))
	}
	for _, env := range envs {
		if env.RunID != runID || env.StepID != "only" {
			t.Errorf("stream envelope = %+v, want run %s step only", env, runID)
		}
	}

	// Both deliver; exactly one executes, the duplicate ACK-drops.
	counting := &countingNoop{}
	cfg := queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1}
	h.Spawn("worker-a", newEngine(t, s, counting, "worker-a").Handle, cfg)
	h.Spawn("worker-b", newEngine(t, s, counting, "worker-b").Handle, cfg)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	if got := counting.calls.Load(); got != 1 {
		t.Errorf("executor ran %d times, want exactly 1 (duplicate must ACK-drop)", got)
	}
	requireSingleAttempts(t, s, runID, []string{"only"})
	waitRun(t, s, runID, store.RunStatusSucceeded)
	requireOutboxEmpty(t, s)
}
