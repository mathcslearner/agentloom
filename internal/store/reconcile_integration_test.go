//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 4.4's store surface: the drain read (ListForDrain), the
// reconciler scans, and the sweep lock. Behavior end-to-end (drain →
// lose → heal) is proven in internal/engine's integration suite; these
// tests pin the store-level contracts directly.

const reconcileSingleNoop = `{
	"schema_version": 1,
	"name": "reconcile-fixture",
	"steps": [{"id": "only", "type": "noop"}],
	"edges": []
}`

// staleCutoff is comfortably after testNow — every row written with
// testNow looks stale against it.
var staleCutoff = testNow.Add(time.Hour)

func TestListForDrainRequiresTx(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))

	if _, err := s.Outbox().ListForDrain(t.Context(), 10); !errors.Is(err, store.ErrNoTx) {
		t.Errorf("ListForDrain outside WithTx: error = %v, want ErrNoTx", err)
	}

	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		tasks, err := q.Outbox().ListForDrain(ctx, 10)
		if err != nil {
			return err
		}
		if len(tasks) != 0 {
			t.Errorf("ListForDrain on empty outbox = %+v, want none", tasks)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

func TestReconcileReadsRequireTx(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()

	if _, err := store.TryReconcileLock(ctx, s); !errors.Is(err, store.ErrNoTx) {
		t.Errorf("TryReconcileLock outside WithTx: error = %v, want ErrNoTx", err)
	}
	if _, err := store.ListStaleReadySteps(ctx, s, staleCutoff, 10); !errors.Is(err, store.ErrNoTx) {
		t.Errorf("ListStaleReadySteps outside WithTx: error = %v, want ErrNoTx", err)
	}
	if _, err := store.ListStaleRunningSteps(ctx, s, staleCutoff, 10); !errors.Is(err, store.ErrNoTx) {
		t.Errorf("ListStaleRunningSteps outside WithTx: error = %v, want ErrNoTx", err)
	}
	if _, err := store.ListStalledRuns(ctx, s, 10); !errors.Is(err, store.ErrNoTx) {
		t.Errorf("ListStalledRuns outside WithTx: error = %v, want ErrNoTx", err)
	}
}

// TestListStaleReadyStepsAntiJoin: a ready step with a pending outbox row
// is one drain away from dispatch — never listed; with the row drained it
// is listed once past the threshold, and never before the threshold.
func TestListStaleReadyStepsAntiJoin(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, reconcileSingleNoop))

	list := func(cutoff time.Time) []store.StepRef {
		t.Helper()
		var refs []store.StepRef
		err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			var err error
			refs, err = store.ListStaleReadySteps(ctx, q, cutoff, 10)
			return err
		})
		if err != nil {
			t.Fatalf("ListStaleReadySteps: %v", err)
		}
		return refs
	}

	// Instantiation left the entry step ready WITH its outbox row.
	if got := list(staleCutoff); len(got) != 0 {
		t.Errorf("stale-ready with pending outbox row = %+v, want none (anti-join)", got)
	}

	// Drain the row away without dispatching — the lost-message shape.
	tasks, err := s.Outbox().List(ctx, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("outbox = %+v (err %v), want the instantiation row", tasks, err)
	}
	if _, err := s.Outbox().Delete(ctx, []int64{tasks[0].ID}); err != nil {
		t.Fatalf("deleting outbox row: %v", err)
	}

	if got := list(testNow.Add(-time.Minute)); len(got) != 0 {
		t.Errorf("stale-ready before the threshold = %+v, want none", got)
	}
	got := list(staleCutoff)
	if len(got) != 1 || got[0].RunID != run.ID || got[0].StepID != "only" {
		t.Errorf("stale-ready past the threshold = %+v, want exactly the stuck step", got)
	}
	if !got[0].UpdatedAt.Equal(testNow) {
		t.Errorf("stale-ready UpdatedAt = %v, want the instantiation time %v", got[0].UpdatedAt, testNow)
	}
}

// TestListStaleRunningSteps: a claimed step is listed past the threshold
// with its claim ID; a healthy run has no stalled-run flags.
func TestListStaleRunningStepsAndStalledRuns(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, reconcileSingleNoop))

	claimed, err := claimStep(t, s, run.ID, "only")
	if err != nil {
		t.Fatalf("claiming step: %v", err)
	}

	err = s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		early, err := store.ListStaleRunningSteps(ctx, q, testNow.Add(-time.Minute), 10)
		if err != nil {
			return err
		}
		if len(early) != 0 {
			t.Errorf("stale-running before the threshold = %+v, want none", early)
		}
		stale, err := store.ListStaleRunningSteps(ctx, q, staleCutoff, 10)
		if err != nil {
			return err
		}
		if len(stale) != 1 || stale[0].StepID != "only" || stale[0].RunID != run.ID {
			t.Errorf("stale-running past the threshold = %+v, want exactly the claimed step", stale)
		} else if stale[0].ClaimID == nil || *stale[0].ClaimID != *claimed.ClaimID {
			t.Errorf("stale-running claim ID = %v, want the holder's %v", stale[0].ClaimID, claimed.ClaimID)
		}
		stalled, err := store.ListStalledRuns(ctx, q, 10)
		if err != nil {
			return err
		}
		if len(stalled) != 0 {
			t.Errorf("stalled runs with a live step = %v, want none", stalled)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

// TestListStalledRunsFlagsImpossibleState: a run whose steps are all
// terminal but whose status is still running (fabricated corruption —
// the rollup makes this impossible through the real paths) is flagged.
func TestListStalledRunsFlagsImpossibleState(t *testing.T) {
	t.Parallel()
	pool := storetest.NewDB(t)
	s := store.NewFromPool(pool)
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, reconcileSingleNoop))

	claimed, err := claimStep(t, s, run.ID, "only")
	if err != nil {
		t.Fatalf("claiming step: %v", err)
	}
	if _, err := succeedStep(t, s, run.ID, "only", *claimed.ClaimID, nil); err != nil {
		t.Fatalf("succeeding step: %v", err)
	}
	// The completion rollup is the engine's job (4.3); here the run is
	// still running with every step terminal only because nothing rolled
	// it up — but make the corruption explicit and durable regardless.
	if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'running' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("fabricating corrupt run status: %v", err)
	}

	err = s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		stalled, err := store.ListStalledRuns(ctx, q, 10)
		if err != nil {
			return err
		}
		if len(stalled) != 1 || stalled[0] != run.ID {
			t.Errorf("stalled runs = %v, want exactly the corrupted run %s", stalled, run.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

// TestTryReconcileLockMutualExclusion: while one transaction holds the
// sweep lock, another cannot take it; once released, it can.
func TestTryReconcileLockMutualExclusion(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()

	err := s.WithTx(ctx, func(txCtx context.Context, q store.Querier) error {
		got, err := store.TryReconcileLock(txCtx, q)
		if err != nil {
			return err
		}
		if !got {
			t.Fatal("first TryReconcileLock = false, want the lock")
		}
		// A second transaction (fresh context — not nested) must lose.
		return s.WithTx(ctx, func(ctx2 context.Context, q2 store.Querier) error {
			got2, err := store.TryReconcileLock(ctx2, q2)
			if err != nil {
				return err
			}
			if got2 {
				t.Error("concurrent TryReconcileLock = true, want false while held")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	// The xact lock died with the transaction: takeable again.
	err = s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		got, err := store.TryReconcileLock(ctx, q)
		if err != nil {
			return err
		}
		if !got {
			t.Error("TryReconcileLock after release = false, want the lock")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}
