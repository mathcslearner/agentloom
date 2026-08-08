//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// txRun creates a run through q so tx tests can observe whether the write
// survived the transaction.
func txRun(ctx context.Context, q store.Querier, id uuid.UUID) error {
	_, err := q.Runs().Create(ctx, gen.CreateRunParams{
		ID:         id,
		Definition: json.RawMessage(`{}`),
		Status:     store.RunStatusRunning,
	})
	return err
}

func TestWithTxCommits(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID := uuid.New()

	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := txRun(ctx, q, runID); err != nil {
			return err
		}
		// Reads inside the tx see its own writes.
		if _, err := q.Runs().Get(ctx, runID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if _, err := s.Runs().Get(ctx, runID); err != nil {
		t.Fatalf("committed run not visible: %v", err)
	}
}

func TestWithTxErrorRollsBack(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID := uuid.New()
	sentinel := errors.New("boom")

	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := txRun(ctx, q, runID); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error: got %v, want the callback's error (wrapped)", err)
	}
	if _, err := s.Runs().Get(ctx, runID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back run still visible: %v", err)
	}
}

func TestWithTxTypedErrorsSurviveWrapping(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	token := "tok"

	mustCreateRun(t, s, func(arg *gen.CreateRunParams) { arg.IdempotencyToken = &token })
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, err := q.Runs().Create(ctx, gen.CreateRunParams{
			Definition: json.RawMessage(`{}`), Status: store.RunStatusRunning, IdempotencyToken: &token,
		})
		return err
	})
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("conflict through WithTx wrapping: got %v, want ConflictError", err)
	}
}

func TestWithTxPanicRollsBackAndPropagates(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID := uuid.New()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic in fn did not propagate")
			} else if r != "kaboom" {
				t.Fatalf("panic value: got %v, want kaboom", r)
			}
		}()
		_ = s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			if err := txRun(ctx, q, runID); err != nil {
				return err
			}
			panic("kaboom")
		})
	}()
	if _, err := s.Runs().Get(ctx, runID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("panicked tx's run still visible: %v", err)
	}
}

func TestWithTxNestedRejected(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	outerRunID := uuid.New()

	var nestedErr error
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := txRun(ctx, q, outerRunID); err != nil {
			return err
		}
		nestedErr = s.WithTx(ctx, func(context.Context, store.Querier) error { return nil })
		// The outer transaction is unaffected by the rejected nested call.
		return nil
	})
	if err != nil {
		t.Fatalf("outer WithTx: %v", err)
	}
	if !errors.Is(nestedErr, store.ErrNestedTx) {
		t.Fatalf("nested WithTx: got %v, want ErrNestedTx", nestedErr)
	}
	if _, err := s.Runs().Get(ctx, outerRunID); err != nil {
		t.Fatalf("outer tx did not commit after rejected nested call: %v", err)
	}
}

func TestWithTxCancelledContext(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	if err := s.WithTx(cancelled, func(context.Context, store.Querier) error { return nil }); err == nil {
		t.Fatal("WithTx on a cancelled context succeeded")
	}
}

// TestWithTxCancelledMidCallback exercises the context.WithoutCancel
// rollback path — the reason that call exists: the caller's context dies
// after the callback has written, the commit fails, and the rollback must
// still leave nothing behind despite running under a cancelled context.
func TestWithTxCancelledMidCallback(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	runID := uuid.New()

	cancellable, cancel := context.WithCancel(t.Context())
	err := s.WithTx(cancellable, func(ctx context.Context, q store.Querier) error {
		if err := txRun(ctx, q, runID); err != nil {
			return err
		}
		cancel() // the caller's deadline fires mid-transaction
		return nil
	})
	if err == nil {
		t.Fatal("WithTx committed under a context cancelled mid-callback")
	}
	if _, err := s.Runs().Get(t.Context(), runID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run visible after cancelled-mid-callback tx: %v, want ErrNotFound", err)
	}
}

func TestWithTxComposesRepos(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID := uuid.New()

	// The 2.5/2.6 shape in miniature: run + step + seq + event + outbox,
	// one transaction, all-or-nothing.
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := txRun(ctx, q, runID); err != nil {
			return err
		}
		if _, err := q.Steps().Create(ctx, gen.CreateRunStepParams{
			RunID: runID, StepID: "a", StepType: "llm", Status: store.StepStatusReady,
			UpdatedAt: testNow,
		}); err != nil {
			return err
		}
		seq, err := q.Runs().AllocateEventSeq(ctx, runID)
		if err != nil {
			return err
		}
		if _, err := q.Events().Append(ctx, gen.AppendEventParams{
			RunID: runID, Seq: seq, Type: "step_ready",
		}); err != nil {
			return err
		}
		_, err = q.Outbox().Create(ctx, runID, "a", store.OutboxReasonStepReady)
		return err
	})
	if err != nil {
		t.Fatalf("composed tx: %v", err)
	}

	if _, err := s.Steps().Get(ctx, runID, "a"); err != nil {
		t.Fatalf("step not committed: %v", err)
	}
	evs, err := s.Events().List(ctx, runID, 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events not committed: %v, %v", evs, err)
	}
	tasks, err := s.Outbox().List(ctx, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("outbox not committed: %v, %v", tasks, err)
	}
}
