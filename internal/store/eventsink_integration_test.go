//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// captureSink is a test EventSink that records every committed batch. It is
// non-blocking and never panics, satisfying the store.EventSink contract.
type captureSink struct {
	mu      sync.Mutex
	batches [][]event.Envelope
}

func (c *captureSink) EventsCommitted(_ context.Context, envs []event.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Copy: the store may reuse the slice's backing array after this returns.
	cp := make([]event.Envelope, len(envs))
	copy(cp, envs)
	c.batches = append(c.batches, cp)
}

func (c *captureSink) all() []event.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []event.Envelope
	for _, b := range c.batches {
		out = append(out, b...)
	}
	return out
}

func (c *captureSink) batchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.batches)
}

// storeWithSink builds a store over a fresh test DB with the given sink wired.
func storeWithSink(t *testing.T, sink store.EventSink) *store.Store {
	t.Helper()
	return store.NewFromPool(storetest.NewDB(t), store.WithEventSink(sink))
}

func guardEvent(current int64) store.GuardTrippedEvent {
	return store.GuardTrippedEvent{Guard: "max_total_steps", Current: current, Cap: 999999, Unit: "steps", Action: "fail"}
}

// TestEventSinkAfterCommit checks the after-commit sink (ticket 16.2, ADR-018)
// receives exactly the events a committed transaction appended, projected to
// typed envelopes in seq order.
func TestEventSinkAfterCommit(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	s := storeWithSink(t, sink)
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)
	now := time.Unix(1_700_000_000, 0).UTC()

	// One transaction appending three events → one batch of three, in order.
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		for i := int64(0); i < 3; i++ {
			if err := store.RecordGuardTripped(ctx, q, run.ID, guardEvent(i), now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	if got := sink.batchCount(); got != 1 {
		t.Fatalf("batch count = %d, want 1 (one committed tx = one batch)", got)
	}
	envs := sink.all()
	if len(envs) != 3 {
		t.Fatalf("delivered %d envelopes, want 3", len(envs))
	}
	for i, env := range envs {
		wantSeq := int64(i + 1)
		if env.Seq != wantSeq {
			t.Errorf("env %d seq = %d, want %d", i, env.Seq, wantSeq)
		}
		if env.RunID != run.ID {
			t.Errorf("env %d run = %s, want %s", i, env.RunID, run.ID)
		}
		if env.Type != event.TypeGuardTripped {
			t.Errorf("env %d type = %q, want %q", i, env.Type, event.TypeGuardTripped)
		}
		if env.SchemaVersion != event.SchemaVersion {
			t.Errorf("env %d schema_version = %d, want %d", i, env.SchemaVersion, event.SchemaVersion)
		}
		// The sink delivers the writer's constructed payload; the wire form the
		// publisher emits (marshal → ParseEnvelope) must round-trip to a typed
		// payload — this is exactly what a subscriber decodes.
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("env %d marshal: %v", i, err)
		}
		parsed, err := event.ParseEnvelope(raw)
		if err != nil {
			t.Fatalf("env %d ParseEnvelope: %v", i, err)
		}
		if _, ok := parsed.Payload.(*event.GuardTripped); !ok {
			t.Errorf("env %d parsed payload = %T, want *event.GuardTripped", i, parsed.Payload)
		}
	}

	// The delivered envelopes must equal what a DB read projects — publish and
	// backfill agree by construction.
	rows, err := s.Events().List(ctx, run.ID, 0, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("stored rows = %d, want 3", len(rows))
	}
	for i, row := range rows {
		projected, err := store.EventEnvelope(row)
		if err != nil {
			t.Fatalf("row %d EventEnvelope: %v", i, err)
		}
		if projected.Seq != envs[i].Seq || projected.Type != envs[i].Type || projected.StepID != envs[i].StepID {
			t.Errorf("row %d: published %+v != backfilled %+v", i, envs[i], projected)
		}
	}
}

// TestEventSinkRollbackDeliversNothing checks a rolled-back transaction fans out
// no events: the durable truth never persisted, so neither may the hint.
func TestEventSinkRollbackDeliversNothing(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	s := storeWithSink(t, sink)
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)
	now := time.Unix(1_700_000_000, 0).UTC()

	sentinel := errors.New("boom")
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := store.RecordGuardTripped(ctx, q, run.ID, guardEvent(0), now); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx err = %v, want sentinel", err)
	}
	if got := sink.batchCount(); got != 0 {
		t.Fatalf("rolled-back tx delivered %d batches, want 0", got)
	}
}

// TestEventSinkNoAppendDeliversNothing checks a committed tx that appended no
// events produces no batch (the sink fires only for len(events) > 0).
func TestEventSinkNoAppendDeliversNothing(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	s := storeWithSink(t, sink)
	ctx := t.Context()

	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		// A read-only transaction: no event appended.
		_, err := q.Runs().List(ctx, 1)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if got := sink.batchCount(); got != 0 {
		t.Fatalf("no-append tx delivered %d batches, want 0", got)
	}
}

// TestEventSinkConcurrentWritersUnion checks that under concurrent single-event
// transactions the union of delivered envelopes is exactly seqs 1..N, with no
// gaps or duplicates — the after-commit sink preserves the per-run seq invariant.
func TestEventSinkConcurrentWritersUnion(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	s := storeWithSink(t, sink)
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
				err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
					return store.RecordGuardTripped(ctx, q, run.ID, guardEvent(int64(w*1000+i)), now)
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

	envs := sink.all()
	if len(envs) != total {
		t.Fatalf("delivered %d envelopes, want %d", len(envs), total)
	}
	seqs := make([]int64, len(envs))
	for i, env := range envs {
		seqs[i] = env.Seq
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("union seq at index %d = %d, want %d (gaps or dupes)", i, seq, i+1)
		}
	}
}
