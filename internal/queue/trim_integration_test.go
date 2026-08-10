//go:build integration

package queue_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
)

// TestTrimAckedKeepsPendingAndUndelivered walks one queue through every
// retention state by hand and asserts TrimAcked removes exactly the
// delivered-and-acked entries: pending entries pin the threshold, and
// undelivered entries survive even with an empty PEL.
func TestTrimAckedKeepsPendingAndUndelivered(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	q := h.Queue()

	h.EnqueueStep(ctx, "s1")
	id2 := h.EnqueueStep(ctx, "s2")
	id3 := h.EnqueueStep(ctx, "s3")
	m1 := h.ReadOne(ctx, "c1")
	m2 := h.ReadOne(ctx, "c1")

	// Nothing acked below the lowest pending entry: nothing to trim.
	requireTrim(ctx, t, q, 0)
	if got := h.Stats(ctx).Length; got != 3 {
		t.Fatalf("XLEN after no-op trim = %d, want 3", got)
	}

	// Acking the oldest pending entry frees exactly it; s2 (pending) and
	// s3 (undelivered) both survive.
	ack(ctx, t, h, m1.ID)
	requireTrim(ctx, t, q, 1)
	if got := h.StreamEnvelopes(ctx); len(got) != 2 || got[0].StepID != "s2" || got[1].StepID != "s3" {
		t.Fatalf("stream after first trim = %+v, want [s2 s3]", got)
	}
	if pel := h.PELSnapshot(ctx); len(pel) != 1 || pel[0].ID != id2 {
		t.Fatalf("PEL after first trim = %+v, want exactly %s", pel, id2)
	}

	// With the PEL empty, everything delivered goes — but the undelivered
	// s3 must survive: the threshold is last-delivered's successor, and
	// s3 is beyond it.
	ack(ctx, t, h, m2.ID)
	requireTrim(ctx, t, q, 1)
	if got := h.StreamEnvelopes(ctx); len(got) != 1 || got[0].StepID != "s3" {
		t.Fatalf("stream after second trim = %+v, want [s3]", got)
	}

	// Deliver s3: pending again pins the threshold.
	m3 := h.ReadOne(ctx, "c1")
	if m3.ID != id3 {
		t.Fatalf("delivered %s, want %s", m3.ID, id3)
	}
	requireTrim(ctx, t, q, 0)
	ack(ctx, t, h, m3.ID)
	requireTrim(ctx, t, q, 1)
	if got := h.Stats(ctx).Length; got != 0 {
		t.Fatalf("XLEN after final trim = %d, want 0", got)
	}

	// The quiescence probe must survive trimming: group lag stays
	// computable because deletions never exceed last-delivered-id, so a
	// fresh undelivered entry is still detected as not-drained.
	if ok, detail := h.IsQuiescent(ctx); !ok {
		t.Fatalf("queue not quiescent after full drain + trim: %s", detail)
	}
	h.EnqueueStep(ctx, "s4")
	if ok, _ := h.IsQuiescent(ctx); ok {
		t.Fatal("IsQuiescent = true with an undelivered entry; lag lost after trimming")
	}
	m4 := h.ReadOne(ctx, "c1")
	ack(ctx, t, h, m4.ID)
	if ok, detail := h.IsQuiescent(ctx); !ok {
		t.Fatalf("queue not quiescent after draining s4: %s", detail)
	}
}

// TestTrimAckedWithoutGroup pins the typed error: retention needs the
// group's delivery state, so a missing group (or stream) is ErrNoGroup,
// never a fabricated no-op.
func TestTrimAckedWithoutGroup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	if _, err := h.Queue().TrimAcked(ctx); !errors.Is(err, queue.ErrNoGroup) {
		t.Fatalf("TrimAcked without group: error = %v, want ErrNoGroup", err)
	}
}

// TestConsumerTrimsAckedEntries pins the duty wiring: a consumer with a
// short TrimInterval drains and then physically empties the stream on its
// own, with no manual TrimAcked call.
func TestConsumerTrimsAckedEntries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	cfg := queuetest.FastConfig()
	cfg.Block = 100 * time.Millisecond
	cfg.TrimInterval = 150 * time.Millisecond
	h.SpawnScript("", queuetest.NewScript(), cfg)

	for i := range 5 {
		h.EnqueueStep(ctx, fmt.Sprintf("s%d", i))
	}
	h.WaitStats(ctx, func(s queue.StreamStats) bool {
		return s.Length == 0 && s.Pending == 0
	})
	h.RequireHandledOncePerClaim()
}

// requireTrim runs one TrimAcked pass and asserts the evicted count.
func requireTrim(ctx context.Context, t *testing.T, q *queue.Queue, want int64) {
	t.Helper()
	got, err := q.TrimAcked(ctx)
	if err != nil {
		t.Fatalf("TrimAcked: %v", err)
	}
	if got != want {
		t.Fatalf("TrimAcked trimmed %d entries, want %d", got, want)
	}
}

// ack XACKs one entry, failing the test on error.
func ack(ctx context.Context, t *testing.T, h *queuetest.Harness, id string) {
	t.Helper()
	if err := h.Client().XAck(ctx, h.Queue().Stream(), h.Queue().Group(), id).Err(); err != nil {
		t.Fatalf("XACK %s: %v", id, err)
	}
}
