//go:build integration

package store_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 5.5's store-level suite: the side_effects repo primitives the
// journal protocol (internal/exec/effects) composes — fresh intent,
// dangling-intent takeover, the first-wins completion guard, and the
// schema's referential/uniqueness rules.

// insertIntent records a fresh attempt-1 intent (the takeover test re-arms
// later attempts through the repo directly).
func insertIntent(t *testing.T, s *store.Store, runID uuid.UUID, stepID, effectID string, claim uuid.UUID, at time.Time) (gen.SideEffect, error) {
	t.Helper()
	return s.SideEffects().InsertIntent(t.Context(), gen.InsertSideEffectIntentParams{
		RunID: runID, StepID: stepID, EffectID: effectID,
		Attempt: 1, ClaimID: claim, IntentAt: at,
	})
}

// TestSideEffectLifecycle: intent → done, with the status guard making the
// journaled result immutable — a second completer and a late takeover both
// bounce off ErrNotFound and read back the stored row.
func TestSideEffectLifecycle(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))
	claimed := mustClaim(t, s, run.ID, "a")

	eff, err := insertIntent(t, s, run.ID, "a", "echo", *claimed.ClaimID, testNow)
	if err != nil {
		t.Fatalf("InsertIntent: %v", err)
	}
	if eff.Status != store.SideEffectStatusIntent || eff.Attempt != 1 || eff.ResultAt != nil {
		t.Errorf("intent row = %+v, want status intent, attempt 1, no result_at", eff)
	}

	// A duplicate intent insert is a primary-key conflict, not silence.
	var conflict *store.ConflictError
	if _, err := insertIntent(t, s, run.ID, "a", "echo", *claimed.ClaimID, testNow); !errors.As(err, &conflict) {
		t.Errorf("duplicate InsertIntent: %v, want *ConflictError", err)
	}

	result := json.RawMessage(`{"echoed": true}`)
	done, err := s.SideEffects().Complete(ctx, gen.CompleteSideEffectParams{
		RunID: run.ID, StepID: "a", EffectID: "echo",
		Result: result, ResultAt: &testNow,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.Status != store.SideEffectStatusDone || string(done.Result) != string(result) || done.ResultAt == nil {
		t.Errorf("done row = %+v, want status done with the journaled result", done)
	}

	// First writer won: a second completion matches nothing.
	if _, err := s.SideEffects().Complete(ctx, gen.CompleteSideEffectParams{
		RunID: run.ID, StepID: "a", EffectID: "echo",
		Result: json.RawMessage(`{"other": 1}`), ResultAt: &testNow,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second Complete: %v, want ErrNotFound (status guard)", err)
	}
	// And a late intent takeover can never regress a completed effect.
	if _, err := s.SideEffects().TakeoverIntent(ctx, gen.TakeoverSideEffectIntentParams{
		RunID: run.ID, StepID: "a", EffectID: "echo",
		Attempt: 2, ClaimID: uuid.New(), IntentAt: testNow,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TakeoverIntent after done: %v, want ErrNotFound (status guard)", err)
	}

	got, err := s.SideEffects().Get(ctx, run.ID, "a", "echo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.SideEffectStatusDone || string(got.Result) != string(result) {
		t.Errorf("stored row = %+v, want the first writer's result intact", got)
	}
}

// TestSideEffectIntentTakeover: a dangling intent (recorder died between
// intent and result) is re-armed for the new attempt in place.
func TestSideEffectIntentTakeover(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))
	claimed := mustClaim(t, s, run.ID, "a")

	if _, err := insertIntent(t, s, run.ID, "a", "call", *claimed.ClaimID, testNow); err != nil {
		t.Fatalf("InsertIntent: %v", err)
	}
	newClaim := uuid.New()
	later := testNow.Add(time.Minute)
	eff, err := s.SideEffects().TakeoverIntent(ctx, gen.TakeoverSideEffectIntentParams{
		RunID: run.ID, StepID: "a", EffectID: "call",
		Attempt: 2, ClaimID: newClaim, IntentAt: later,
	})
	if err != nil {
		t.Fatalf("TakeoverIntent: %v", err)
	}
	if eff.Status != store.SideEffectStatusIntent || eff.Attempt != 2 ||
		eff.ClaimID != newClaim || !eff.IntentAt.Equal(later) {
		t.Errorf("taken-over intent = %+v, want attempt 2 under the new claim at the new time", eff)
	}

	// Takeover of a row that was never written is ErrNotFound.
	if _, err := s.SideEffects().TakeoverIntent(ctx, gen.TakeoverSideEffectIntentParams{
		RunID: run.ID, StepID: "a", EffectID: "never",
		Attempt: 1, ClaimID: newClaim, IntentAt: later,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TakeoverIntent on missing row: %v, want ErrNotFound", err)
	}
}

// TestSideEffectReferentialRules: the journal is anchored to real steps and
// lists per step in effect_id order.
func TestSideEffectReferentialRules(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))
	claimed := mustClaim(t, s, run.ID, "a")

	// A row for a step that does not exist violates the FK.
	var conflict *store.ConflictError
	if _, err := insertIntent(t, s, run.ID, "ghost", "x", uuid.New(), testNow); !errors.As(err, &conflict) {
		t.Errorf("InsertIntent for missing step: %v, want *ConflictError (FK)", err)
	}

	for _, id := range []string{"b-second", "a-first"} {
		if _, err := insertIntent(t, s, run.ID, "a", id, *claimed.ClaimID, testNow); err != nil {
			t.Fatalf("InsertIntent %q: %v", id, err)
		}
	}
	rows, err := s.SideEffects().ListByStep(ctx, run.ID, "a")
	if err != nil {
		t.Fatalf("ListByStep: %v", err)
	}
	if len(rows) != 2 || rows[0].EffectID != "a-first" || rows[1].EffectID != "b-second" {
		t.Errorf("ListByStep = %+v, want [a-first b-second]", rows)
	}

	if _, err := s.SideEffects().Get(ctx, run.ID, "a", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get on missing row: %v, want ErrNotFound", err)
	}
}
