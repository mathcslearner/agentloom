package queuetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
)

func delivery(step string) queue.Delivery {
	return queue.Delivery{ID: "1-1", Envelope: Envelope(step), DeliveryCount: 1}
}

// TestScriptSequenceConsumesThenSticks pins the sequence contract: one
// action per invocation, and the last action repeats once the sequence is
// exhausted — so a single Fail fails forever and a trailing Succeed keeps
// succeeding.
func TestScriptSequenceConsumesThenSticks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	wantErr := errors.New("scripted")
	s := NewScript().OnStep("flaky", Fail(wantErr), Fail(wantErr), Succeed())
	for i, wantFail := range []bool{true, true, false, false} {
		err := s.Handle(ctx, delivery("flaky"))
		if wantFail && !errors.Is(err, wantErr) {
			t.Errorf("invocation %d: err = %v, want %v", i, err, wantErr)
		}
		if !wantFail && err != nil {
			t.Errorf("invocation %d: err = %v, want nil (last action sticks)", i, err)
		}
	}

	forever := NewScript().OnStep("dead", Fail(nil))
	for i := range 3 {
		if err := forever.Handle(ctx, delivery("dead")); err == nil {
			t.Errorf("invocation %d: err = nil, want the single Fail to repeat forever", i)
		}
	}
}

// TestScriptDefault covers unscripted steps: Succeed out of the box,
// overridable via Default.
func TestScriptDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if err := NewScript().Handle(ctx, delivery("anything")); err != nil {
		t.Errorf("default action: err = %v, want nil (Succeed)", err)
	}
	wantErr := errors.New("default failure")
	s := NewScript().Default(Fail(wantErr))
	if err := s.Handle(ctx, delivery("anything")); !errors.Is(err, wantErr) {
		t.Errorf("overridden default: err = %v, want %v", err, wantErr)
	}
}

// TestScriptPanicAction pins that PanicWith actually panics — the
// consumer's containment (safeHandle) is the behavior chaos tests target.
func TestScriptPanicAction(t *testing.T) {
	t.Parallel()

	s := NewScript().OnStep("boom", PanicWith("scripted panic"))
	defer func() {
		if r := recover(); r != "scripted panic" {
			t.Errorf("recover() = %v, want the scripted panic message", r)
		}
	}()
	s.Handle(context.Background(), delivery("boom")) //nolint:errcheck // panics before returning
	t.Error("Handle returned; want panic")
}

// TestScriptHangReleaseAndCancel covers both ways out of a Hang: Release
// (success — including a Release issued before the Hang starts) and
// handler-context cancellation (ctx.Err, a failure, which is how a killed
// consumer's in-flight hang resolves).
func TestScriptHangReleaseAndCancel(t *testing.T) {
	t.Parallel()

	s := NewScript().OnStep("slow", Hang())
	s.Release("slow") // pre-release: the Hang must pass straight through
	done := make(chan error, 1)
	go func() { done <- s.Handle(context.Background(), delivery("slow")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("pre-released hang: err = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pre-released hang never returned")
	}

	s2 := NewScript().OnStep("stuck", Hang())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- s2.Handle(ctx, delivery("stuck")) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled hang: err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled hang never returned")
	}
}

// TestJournalWrapRecords pins the journaling wrapper: success and failure
// results land with their delivery metadata, and a panic is recorded then
// re-raised.
func TestJournalWrapRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	j := &Journal{}
	wantErr := errors.New("handler error")
	h := j.wrap("worker-1", func(_ context.Context, d queue.Delivery) error {
		if d.Envelope.StepID == "bad" {
			return wantErr
		}
		return nil
	})
	if err := h(ctx, queue.Delivery{ID: "1-1", Envelope: Envelope("good"), DeliveryCount: 1}); err != nil {
		t.Fatalf("wrapped handler: %v", err)
	}
	if err := h(ctx, queue.Delivery{ID: "2-1", Envelope: Envelope("bad"), DeliveryCount: 3}); !errors.Is(err, wantErr) {
		t.Fatalf("wrapped handler error = %v, want %v", err, wantErr)
	}

	invs := j.Invocations()
	if len(invs) != 2 {
		t.Fatalf("journal has %d invocations, want 2", len(invs))
	}
	first, second := invs[0], invs[1]
	if first.Consumer != "worker-1" || first.EntryID != "1-1" || first.Step != "good" || first.DeliveryCount != 1 || first.Err != nil || first.End.IsZero() {
		t.Errorf("first invocation = %+v, want worker-1/1-1/good/1/success/finished", first)
	}
	if second.EntryID != "2-1" || second.DeliveryCount != 3 || !errors.Is(second.Err, wantErr) {
		t.Errorf("second invocation = %+v, want entry 2-1 delivery 3 with the handler error", second)
	}

	panicky := j.wrap("worker-1", func(context.Context, queue.Delivery) error { panic("kaboom") })
	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Errorf("recover() = %v, want the re-raised panic", r)
			}
		}()
		panicky(ctx, queue.Delivery{ID: "3-1", Envelope: Envelope("boom"), DeliveryCount: 1}) //nolint:errcheck // panics
	}()
	invs = j.Invocations()
	if len(invs) != 3 {
		t.Fatalf("journal has %d invocations after panic, want 3", len(invs))
	}
	if !invs[2].Panicked || invs[2].Err == nil || invs[2].End.IsZero() {
		t.Errorf("panic invocation = %+v, want Panicked with an error and a finish time", invs[2])
	}
}

// TestJournalDuplicateClaims pins the exactly-once-per-claim detector:
// two invocations sharing (entry, delivery count) are a violation; the
// same entry at different counts — at-least-once across claims — is not.
func TestJournalDuplicateClaims(t *testing.T) {
	t.Parallel()

	j := &Journal{}
	j.invs = []*Invocation{
		{Consumer: "a", EntryID: "1-1", DeliveryCount: 1},
		{Consumer: "b", EntryID: "1-1", DeliveryCount: 2}, // reclaim: fine
		{Consumer: "a", EntryID: "2-1", DeliveryCount: 1},
	}
	if dups := j.duplicateClaims(); len(dups) != 0 {
		t.Errorf("duplicateClaims = %v, want none for distinct claims", dups)
	}

	j.invs = append(j.invs, &Invocation{Consumer: "c", EntryID: "1-1", DeliveryCount: 2})
	dups := j.duplicateClaims()
	if len(dups) != 1 {
		t.Fatalf("duplicateClaims = %v, want exactly one violation", dups)
	}
}
