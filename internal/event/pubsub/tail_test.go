package pubsub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/event"
)

// scriptedBackfill is a Backfiller over an in-memory ordered event list. It
// records how many times EventsAfter was called so tests can assert backfill
// happened (or did not).
type scriptedBackfill struct {
	runID uuid.UUID
	all   []event.Envelope // seq-ascending, contiguous from 1
	calls int
	err   error
}

func (b *scriptedBackfill) EventsAfter(_ context.Context, runID uuid.UUID, afterSeq int64, limit int32) ([]event.Envelope, error) {
	b.calls++
	if b.err != nil {
		return nil, b.err
	}
	var out []event.Envelope
	for _, e := range b.all {
		if e.RunID == runID && e.Seq > afterSeq {
			out = append(out, e)
			if len(out) == int(limit) {
				break
			}
		}
	}
	return out, nil
}

func env(runID uuid.UUID, seq int64) event.Envelope {
	return event.NewEnvelope(runID, seq, time.Unix(seq, 0).UTC(), event.RunSucceeded{})
}

func makeEnvs(runID uuid.UUID, n int) []event.Envelope {
	out := make([]event.Envelope, n)
	for i := range out {
		out[i] = env(runID, int64(i+1))
	}
	return out
}

// collect delivers into a slice of seqs.
func collector() (func(event.Envelope), *[]int64) {
	var got []int64
	return func(e event.Envelope) { got = append(got, e.Seq) }, &got
}

func TestTailerInOrder(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	deliver, got := collector()
	tailer := NewTailer(runID, 0, &scriptedBackfill{runID: runID}, deliver, 100)
	for seq := int64(1); seq <= 5; seq++ {
		if err := tailer.Offer(context.Background(), env(runID, seq)); err != nil {
			t.Fatalf("Offer(%d): %v", seq, err)
		}
	}
	if want := []int64{1, 2, 3, 4, 5}; !equal(*got, want) {
		t.Fatalf("delivered %v, want %v", *got, want)
	}
	if tailer.LastSeq() != 5 {
		t.Fatalf("lastSeq = %d, want 5", tailer.LastSeq())
	}
}

func TestTailerDropsDuplicates(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	deliver, got := collector()
	tailer := NewTailer(runID, 0, &scriptedBackfill{runID: runID}, deliver, 100)
	seqs := []int64{1, 2, 2, 3, 1, 3, 4}
	for _, s := range seqs {
		if err := tailer.Offer(context.Background(), env(runID, s)); err != nil {
			t.Fatalf("Offer(%d): %v", s, err)
		}
	}
	if want := []int64{1, 2, 3, 4}; !equal(*got, want) {
		t.Fatalf("delivered %v, want %v", *got, want)
	}
}

func TestTailerBackfillsSinglePageGap(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	// Head is 5 when the live seq=5 arrives (publish is after-commit): backfill
	// reads to head, delivering the gap 2..5; the live 5 is then a dupe.
	backfill := &scriptedBackfill{runID: runID, all: makeEnvs(runID, 5)}
	deliver, got := collector()
	tailer := NewTailer(runID, 0, backfill, deliver, 100)
	_ = tailer.Offer(context.Background(), env(runID, 1))
	_ = tailer.Offer(context.Background(), env(runID, 5))
	if want := []int64{1, 2, 3, 4, 5}; !equal(*got, want) {
		t.Fatalf("delivered %v, want %v", *got, want)
	}
	if backfill.calls == 0 {
		t.Fatal("expected a backfill on the gap, got none")
	}
}

func TestTailerBackfillsMultiPageGap(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	backfill := &scriptedBackfill{runID: runID, all: makeEnvs(runID, 20)}
	deliver, got := collector()
	// pageSize 3 forces several backfill pages to cover the 2..19 gap.
	tailer := NewTailer(runID, 0, backfill, deliver, 3)
	_ = tailer.Offer(context.Background(), env(runID, 1))
	_ = tailer.Offer(context.Background(), env(runID, 20))
	want := make([]int64, 20)
	for i := range want {
		want[i] = int64(i + 1)
	}
	if !equal(*got, want) {
		t.Fatalf("delivered %v, want %v", *got, want)
	}
}

// TestTailerLiveOvertakenByBackfill covers the case where the gap-triggering
// live envelope is already covered by the backfill (its seq ≤ head): it must not
// be delivered twice.
func TestTailerLiveOvertakenByBackfill(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	backfill := &scriptedBackfill{runID: runID, all: makeEnvs(runID, 10)}
	deliver, got := collector()
	tailer := NewTailer(runID, 0, backfill, deliver, 100)
	_ = tailer.Offer(context.Background(), env(runID, 1))
	// Live message seq=3 arrives, but backfill already goes to 10; 3 is covered.
	_ = tailer.Offer(context.Background(), env(runID, 3))
	if want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}; !equal(*got, want) {
		t.Fatalf("delivered %v, want %v", *got, want)
	}
}

func TestTailerBackfillErrorPropagates(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	boom := errors.New("db down")
	backfill := &scriptedBackfill{runID: runID, err: boom}
	deliver, _ := collector()
	tailer := NewTailer(runID, 0, backfill, deliver, 100)
	_ = tailer.Offer(context.Background(), env(runID, 1))
	err := tailer.Offer(context.Background(), env(runID, 5)) // gap → backfill errors
	if !errors.Is(err, boom) {
		t.Fatalf("Offer err = %v, want %v", err, boom)
	}
}

func TestTailerIgnoresOtherRun(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	other := uuid.New()
	deliver, got := collector()
	tailer := NewTailer(runID, 0, &scriptedBackfill{runID: runID}, deliver, 100)
	_ = tailer.Offer(context.Background(), env(runID, 1))
	_ = tailer.Offer(context.Background(), env(other, 2)) // different run — ignored
	_ = tailer.Offer(context.Background(), env(runID, 2))
	if want := []int64{1, 2}; !equal(*got, want) {
		t.Fatalf("delivered %v, want %v", *got, want)
	}
}

func TestTailerCatchupFromResumeCursor(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	backfill := &scriptedBackfill{runID: runID, all: makeEnvs(runID, 10)}
	deliver, got := collector()
	// Resume at seq 7: Catchup delivers 8,9,10 only.
	tailer := NewTailer(runID, 7, backfill, deliver, 100)
	if err := tailer.Catchup(context.Background()); err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if want := []int64{8, 9, 10}; !equal(*got, want) {
		t.Fatalf("delivered %v, want %v", *got, want)
	}
	// A second Catchup delivers nothing new.
	before := len(*got)
	if err := tailer.Catchup(context.Background()); err != nil {
		t.Fatalf("Catchup 2: %v", err)
	}
	if len(*got) != before {
		t.Fatalf("second Catchup delivered %d extra", len(*got)-before)
	}
}

func TestChannelNaming(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	if got := RunChannel("events", id); got != "events:run:11111111-1111-1111-1111-111111111111" {
		t.Errorf("RunChannel = %q", got)
	}
	if got := FirehoseChannel("events"); got != "events:firehose" {
		t.Errorf("FirehoseChannel = %q", got)
	}
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
