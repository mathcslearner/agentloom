//go:build integration

package queuetest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
)

const opTimeout = 30 * time.Second

const ttl = 400 * time.Millisecond

// TestKillAtPreHandle is crash cell W2 through the harness: the consumer
// dies after claiming but before the handler runs — the handler is never
// invoked, the lease survives in the PEL, and a second consumer reclaims
// and completes.
func TestKillAtPreHandle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	a := h.SpawnScript("consumer-a", queuetest.NewScript(), queuetest.LeaseConfig(ttl))
	a.KillAt(queue.PhasePreHandle, "victim")
	id := h.EnqueueStep(ctx, "victim")

	select {
	case <-a.Killed():
	case <-ctx.Done():
		t.Fatal("the armed pre-handle kill never fired")
	}
	if err := a.Kill(); err != nil {
		t.Fatalf("collecting killed consumer A: Run returned %v, want nil", err)
	}
	if invs := h.Journal().Invocations(); len(invs) != 0 {
		t.Fatalf("journal = %+v, want empty (a pre-handle crash must precede the handler)", invs)
	}
	pel := h.PELSnapshot(ctx)
	if len(pel) != 1 || pel[0].ID != id || pel[0].Consumer != "consumer-a" || pel[0].RetryCount != 1 {
		t.Fatalf("PEL after kill = %+v, want entry %s leased to consumer-a at delivery count 1", pel, id)
	}

	h.SpawnScript("consumer-b", queuetest.NewScript(), queuetest.LeaseConfig(ttl))
	invs := h.WaitJournal(ctx, func(invs []queuetest.Invocation) bool {
		return len(invs) == 1 && !invs[0].End.IsZero()
	})
	if invs[0].Consumer != "consumer-b" || invs[0].DeliveryCount != 2 || invs[0].Err != nil {
		t.Errorf("recovery invocation = %+v, want consumer-b succeeding at delivery count 2", invs[0])
	}
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
}

// TestKillAtPreAck is crash cell W4 — the cell that motivated the
// PhaseHook, because it cannot be provoked from outside the queue
// package: the handler completes its work, the consumer dies before XACK,
// and the entry is legitimately redelivered. The handler therefore runs
// twice — at-least-once across claims is the designed behavior (the claim
// CAS dedupes downstream) — but never twice within one claim.
func TestKillAtPreAck(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	a := h.SpawnScript("consumer-a", queuetest.NewScript(), queuetest.LeaseConfig(ttl))
	a.KillAt(queue.PhasePreAck, "victim")
	id := h.EnqueueStep(ctx, "victim")

	select {
	case <-a.Killed():
	case <-ctx.Done():
		t.Fatal("the armed pre-ack kill never fired")
	}
	if err := a.Kill(); err != nil {
		t.Fatalf("collecting killed consumer A: Run returned %v, want nil", err)
	}
	invs := h.Journal().Invocations()
	if len(invs) != 1 || invs[0].Err != nil || invs[0].End.IsZero() {
		t.Fatalf("journal after kill = %+v, want exactly one successful invocation (work done, ack suppressed)", invs)
	}
	pel := h.PELSnapshot(ctx)
	if len(pel) != 1 || pel[0].ID != id || pel[0].RetryCount != 1 {
		t.Fatalf("PEL after kill = %+v, want entry %s still pending at delivery count 1", pel, id)
	}

	h.SpawnScript("consumer-b", queuetest.NewScript(), queuetest.LeaseConfig(ttl))
	invs = h.WaitJournal(ctx, func(invs []queuetest.Invocation) bool {
		return len(invs) == 2 && !invs[1].End.IsZero()
	})
	if invs[1].Consumer != "consumer-b" || invs[1].DeliveryCount != 2 || invs[1].Err != nil {
		t.Errorf("redelivery invocation = %+v, want consumer-b succeeding at delivery count 2", invs[1])
	}
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
}

// TestKillMidHandle composes the mid-handle crash from harness
// primitives: the handler hangs, the kill switch cancels the consumer
// mid-flight (the hang resolves to ctx.Err — a nack), and the entry is
// reclaimed. The second delivery consumes the next scripted action and
// succeeds.
func TestKillMidHandle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	script := queuetest.NewScript().OnStep("victim", queuetest.Hang(), queuetest.Succeed())
	a := h.SpawnScript("consumer-a", script, queuetest.LeaseConfig(ttl))
	h.EnqueueStep(ctx, "victim")

	// The journal records the invocation at start (End zero while the
	// handler runs) — that is the "handler is mid-flight" signal.
	h.WaitJournal(ctx, func(invs []queuetest.Invocation) bool {
		return len(invs) == 1 && invs[0].End.IsZero()
	})
	if err := a.Kill(); err != nil {
		t.Fatalf("killing consumer A mid-handle: Run returned %v, want nil", err)
	}
	invs := h.Journal().Invocations()
	if len(invs) != 1 || !errors.Is(invs[0].Err, context.Canceled) {
		t.Fatalf("journal after mid-handle kill = %+v, want one invocation failed with context.Canceled", invs)
	}

	h.SpawnScript("consumer-b", script, queuetest.LeaseConfig(ttl))
	invs = h.WaitJournal(ctx, func(invs []queuetest.Invocation) bool {
		return len(invs) == 2 && !invs[1].End.IsZero()
	})
	if invs[1].Consumer != "consumer-b" || invs[1].DeliveryCount != 2 || invs[1].Err != nil {
		t.Errorf("recovery invocation = %+v, want consumer-b succeeding at delivery count 2", invs[1])
	}
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
}

// TestQuiescenceInvariants walks one entry through every non-quiescent
// state the probe distinguishes — delayed-set occupancy, undelivered
// stream backlog, delivered-but-unacked PEL entry — and asserts
// quiescence holds exactly at the start and after the final ack.
func TestQuiescenceInvariants(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	t0 := time.UnixMilli(1_754_000_000_000).UTC()

	if ok, detail := h.IsQuiescent(ctx); !ok {
		t.Fatalf("fresh harness not quiescent: %s", detail)
	}

	if err := h.Delayed().Schedule(ctx, queuetest.Envelope("later"), t0); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if ok, detail := h.IsQuiescent(ctx); ok || detail == "" {
		t.Errorf("quiescent with a delayed entry pending; want delayed-set violation, got ok=%t %q", ok, detail)
	}

	h.PromoteDue(ctx, t0, 16)
	if ok, detail := h.IsQuiescent(ctx); ok || detail == "" {
		t.Errorf("quiescent with an undelivered stream entry; want drain violation, got ok=%t %q", ok, detail)
	}

	msg := h.ReadOne(ctx, "probe")
	if ok, detail := h.IsQuiescent(ctx); ok || detail == "" {
		t.Errorf("quiescent with an unacked PEL entry; want PEL violation, got ok=%t %q", ok, detail)
	}

	if err := h.Client().XAck(ctx, h.Queue().Stream(), h.Queue().Group(), msg.ID).Err(); err != nil {
		t.Fatalf("XACK: %v", err)
	}
	h.WaitQuiescent(ctx)
}

// TestScriptedLadderUnderReclaim drives one entry through fail → panic →
// succeed across real reclaims: the sequence is consumed in order, the
// panic is contained and journaled, and the third claim completes. Also
// the end-to-end proof that scripted behaviors, the journal, and the
// quiescence assertions compose.
func TestScriptedLadderUnderReclaim(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	script := queuetest.NewScript().OnStep("flaky",
		queuetest.Fail(errors.New("first attempt")),
		queuetest.PanicWith("second attempt"),
		queuetest.Succeed())
	h.SpawnScript("", script, queuetest.LeaseConfig(ttl))
	h.EnqueueStep(ctx, "flaky")

	invs := h.WaitJournal(ctx, func(invs []queuetest.Invocation) bool {
		return len(invs) == 3 && !invs[2].End.IsZero()
	})
	for i, want := range []struct {
		count    int64
		failed   bool
		panicked bool
	}{{1, true, false}, {2, true, true}, {3, false, false}} {
		inv := invs[i]
		if inv.DeliveryCount != want.count || (inv.Err != nil) != want.failed || inv.Panicked != want.panicked {
			t.Errorf("invocation %d = %+v, want delivery %d failed=%t panicked=%t", i, inv, want.count, want.failed, want.panicked)
		}
	}
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
}
