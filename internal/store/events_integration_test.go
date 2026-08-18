//go:build integration

package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/store"
)

// TestEventSeqMonotonicUnderConcurrentWriters is the 16.1 acceptance assertion
// (ADR-018): per-run event seq is monotonic and gap-free even when many writers
// append concurrently. N goroutines each append M guard_tripped events on one
// run (each in its own transaction, each taking the run lock and allocating a
// seq after the write), and the resulting seqs must be exactly 1..N*M —
// contiguous, unique, ascending — with every row decoding to a typed envelope.
func TestEventSeqMonotonicUnderConcurrentWriters(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)
	now := time.Unix(1_700_000_000, 0).UTC()

	const writers, perWriter = 8, 25
	total := writers * perWriter

	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				ev := store.GuardTrippedEvent{
					Guard:   "max_total_steps",
					Current: int64(w*1000 + i),
					Cap:     999999,
					Unit:    "steps",
					Action:  "fail",
				}
				err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
					return store.RecordGuardTripped(ctx, q, run.ID, ev, now)
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}

	// Read the whole feed and assert the seq invariant.
	rows, err := s.Events().List(ctx, run.ID, 0, int32(total)+10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(rows) != total {
		t.Fatalf("event count = %d, want %d", len(rows), total)
	}
	for i, row := range rows {
		wantSeq := int64(i + 1)
		if row.Seq != wantSeq {
			t.Fatalf("row %d seq = %d, want %d (not contiguous/ascending)", i, row.Seq, wantSeq)
		}
		if row.Type != store.EventGuardTripped {
			t.Fatalf("row %d type = %q, want %q", i, row.Type, store.EventGuardTripped)
		}
		// Every stored row decodes to a typed envelope (the read path).
		env, err := store.EventEnvelope(row)
		if err != nil {
			t.Fatalf("row %d EventEnvelope: %v", i, err)
		}
		if env.Seq != wantSeq || env.RunID != run.ID || env.SchemaVersion != event.SchemaVersion {
			t.Fatalf("row %d envelope mismatch: %+v", i, env)
		}
		if _, ok := env.Payload.(*event.GuardTripped); !ok {
			t.Fatalf("row %d payload type = %T, want *event.GuardTripped", i, env.Payload)
		}
	}
}

// TestEventBackfillFromLastSeq checks the WS backfill shape: List(afterSeq)
// returns exactly the events after the cursor, in seq order, each decoding to a
// typed envelope with its step id lifted.
func TestEventBackfillFromLastSeq(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)
	now := time.Unix(1_700_000_000, 0).UTC()

	// Append a handful of loop_exhausted events (step-scoped via the completing
	// instance), one per transaction.
	for i := 0; i < 5; i++ {
		ev := store.LoopExhaustedEvent{
			LoopSourceStep:     "critique",
			LoopSourceInstance: "critique#" + string(rune('0'+i)),
			BodyEntry:          "draft",
			Iteration:          i,
			MaxIterations:      3,
			Policy:             "proceed",
			Action:             "proceed",
		}
		if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			return store.RecordLoopExhausted(ctx, q, run.ID, ev, now)
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Backfill from seq 2: expect seqs 3,4,5.
	rows, err := s.Events().List(ctx, run.ID, 2, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("backfill from seq 2 returned %d rows, want 3", len(rows))
	}
	for i, row := range rows {
		wantSeq := int64(i + 3)
		if row.Seq != wantSeq {
			t.Fatalf("row %d seq = %d, want %d", i, row.Seq, wantSeq)
		}
		env, err := store.EventEnvelope(row)
		if err != nil {
			t.Fatalf("row %d EventEnvelope: %v", i, err)
		}
		// The envelope lifts the completing instance as the step id.
		wantStep := "critique#" + string(rune('0'+(i+2)))
		if env.StepID != wantStep {
			t.Fatalf("row %d envelope StepID = %q, want %q", i, env.StepID, wantStep)
		}
	}
}
