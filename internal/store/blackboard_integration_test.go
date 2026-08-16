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
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// putBB runs PutBlackboardEntry in its own transaction (the pgboard's
// short-transaction-per-write model).
func putBB(t *testing.T, s *store.Store, args store.BlackboardPutArgs) (gen.BlackboardEntry, error) {
	t.Helper()
	if args.Now.IsZero() {
		args.Now = testNow
	}
	var entry gen.BlackboardEntry
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		entry, err = store.PutBlackboardEntry(ctx, q, args)
		return err
	})
	return entry, err
}

// TestBlackboardVersioning: successive writes to one key append versions,
// Head returns the latest, History returns them all in order.
func TestBlackboardVersioning(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	for i, v := range []string{`"one"`, `"two"`, `"three"`} {
		entry, err := putBB(t, s, store.BlackboardPutArgs{
			RunID: run.ID, Key: "draft", Value: json.RawMessage(v),
			TokenCount: int32(i + 1), TokenCounter: "fallback/chars4@1",
			AuthorStepID: "a", AuthorAttempt: 1,
		})
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		if entry.Version != int32(i+1) {
			t.Fatalf("put %d: version = %d, want %d", i, entry.Version, i+1)
		}
	}

	head, err := s.Blackboard().Head(t.Context(), run.ID, "draft")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Version != 3 || string(head.Value) != `"three"` {
		t.Fatalf("Head = v%d %s, want v3 \"three\"", head.Version, head.Value)
	}
	if head.AuthorStepID == nil || *head.AuthorStepID != "a" {
		t.Fatalf("Head author = %v, want \"a\"", head.AuthorStepID)
	}

	hist, err := s.Blackboard().History(t.Context(), run.ID, "draft")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 || hist[0].Version != 1 || hist[2].Version != 3 {
		t.Fatalf("History = %d rows (%v), want 3 ascending", len(hist), versionsOf(hist))
	}

	// A never-written key: Head is ErrNotFound, History is empty.
	if _, err := s.Blackboard().Head(t.Context(), run.ID, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Head(missing) = %v, want ErrNotFound", err)
	}
	if h, err := s.Blackboard().History(t.Context(), run.ID, "missing"); err != nil || len(h) != 0 {
		t.Fatalf("History(missing) = %v rows, %v; want 0, nil", len(h), err)
	}
}

// TestBlackboardTagQueries: ListHeads filters by key and by tag (AND), and
// the tag filter applies to the head, not to a superseded version.
func TestBlackboardTagQueries(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	put := func(key string, tags []string) {
		if _, err := putBB(t, s, store.BlackboardPutArgs{
			RunID: run.ID, Key: key, Value: json.RawMessage(`1`), Tags: tags, AuthorStepID: "a", AuthorAttempt: 1,
		}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	put("draft", []string{"pinned", "writer"})
	put("notes", []string{"writer"})
	put("scratch", nil)
	// draft v2 drops the pinned tag: the head is not pinned anymore.
	put("draft", []string{"writer"})

	all, err := s.Blackboard().ListHeads(t.Context(), run.ID, nil, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListHeads(all) = %d (%v), want 3", len(all), err)
	}
	// Ordered by key.
	if all[0].Key != "draft" || all[1].Key != "notes" || all[2].Key != "scratch" {
		t.Fatalf("ListHeads order = %s,%s,%s", all[0].Key, all[1].Key, all[2].Key)
	}

	writers, err := s.Blackboard().ListHeads(t.Context(), run.ID, nil, []string{"writer"})
	if err != nil || len(writers) != 2 {
		t.Fatalf("ListHeads(tag=writer) = %d (%v), want 2 (draft, notes)", len(writers), err)
	}

	// The pinned tag is on draft v1 only; the head (v2) is not pinned, so the
	// tag filter must not surface draft.
	pinned, err := s.Blackboard().ListHeads(t.Context(), run.ID, nil, []string{"pinned"})
	if err != nil || len(pinned) != 0 {
		t.Fatalf("ListHeads(tag=pinned) = %d (%v), want 0 (superseded tag)", len(pinned), err)
	}

	byKey, err := s.Blackboard().ListHeads(t.Context(), run.ID, []string{"notes", "scratch"}, nil)
	if err != nil || len(byKey) != 2 {
		t.Fatalf("ListHeads(keys) = %d (%v), want 2", len(byKey), err)
	}
}

// TestBlackboardCASConflict: an ExpectedVersion mismatch is a typed
// conflict that writes nothing (no version, no event burned); the correct
// expected version succeeds.
func TestBlackboardCASConflict(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	// expected 0 = must not exist: first write wins.
	zero := 0
	if _, err := putBB(t, s, store.BlackboardPutArgs{
		RunID: run.ID, Key: "k", Value: json.RawMessage(`1`), ExpectedVersion: &zero, AuthorStepID: "a",
	}); err != nil {
		t.Fatalf("initial CAS put: %v", err)
	}
	// A second expected-0 write conflicts (head is now 1).
	_, err := putBB(t, s, store.BlackboardPutArgs{
		RunID: run.ID, Key: "k", Value: json.RawMessage(`2`), ExpectedVersion: &zero, AuthorStepID: "a",
	})
	var ce *store.BlackboardVersionConflict
	if !errors.As(err, &ce) || ce.Expected != 0 || ce.Current != 1 {
		t.Fatalf("second CAS put = %v, want BlackboardVersionConflict{expected 0, current 1}", err)
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflict does not unwrap to ErrConflict: %v", err)
	}
	// The conflict wrote nothing: head is still v1 with value 1.
	head, _ := s.Blackboard().Head(t.Context(), run.ID, "k")
	if head.Version != 1 || string(head.Value) != `1` {
		t.Fatalf("after conflict head = v%d %s, want v1 1", head.Version, head.Value)
	}

	// Correct expected version (1) succeeds → v2.
	one := 1
	entry, err := putBB(t, s, store.BlackboardPutArgs{
		RunID: run.ID, Key: "k", Value: json.RawMessage(`2`), ExpectedVersion: &one, AuthorStepID: "a",
	})
	if err != nil || entry.Version != 2 {
		t.Fatalf("CAS put with expected=1 = v%d, %v; want v2, nil", entry.Version, err)
	}
}

// TestBlackboardParallelWritersDistinctKeys: two writers to different keys
// both land at v1 (no interference).
func TestBlackboardParallelWritersDistinctKeys(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	e1, err1 := putBB(t, s, store.BlackboardPutArgs{RunID: run.ID, Key: "ka", Value: json.RawMessage(`1`), AuthorStepID: "a"})
	e2, err2 := putBB(t, s, store.BlackboardPutArgs{RunID: run.ID, Key: "kb", Value: json.RawMessage(`2`), AuthorStepID: "b"})
	if err1 != nil || err2 != nil {
		t.Fatalf("distinct-key puts: %v / %v", err1, err2)
	}
	if e1.Version != 1 || e2.Version != 1 {
		t.Fatalf("distinct-key versions = %d, %d; want 1, 1", e1.Version, e2.Version)
	}
}

// TestBlackboardFence: a write presenting a claim that does not match the
// authoring step's row is rejected as fenced; the matching claim succeeds.
func TestBlackboardFence(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	claimed, err := claimStep(t, s, run.ID, "a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	realClaim := *claimed.ClaimID

	// A stale claim id is fenced out.
	stale := uuid.New()
	_, err = putBB(t, s, store.BlackboardPutArgs{
		RunID: run.ID, Key: "k", Value: json.RawMessage(`1`),
		AuthorStepID: "a", AuthorAttempt: 1, FenceClaimID: &stale,
	})
	if !errors.Is(err, store.ErrBlackboardFenced) {
		t.Fatalf("stale-claim write = %v, want ErrBlackboardFenced", err)
	}
	// The real claim writes fine.
	if _, err := putBB(t, s, store.BlackboardPutArgs{
		RunID: run.ID, Key: "k", Value: json.RawMessage(`1`),
		AuthorStepID: "a", AuthorAttempt: 1, FenceClaimID: &realClaim,
	}); err != nil {
		t.Fatalf("matching-claim write: %v", err)
	}
}

func versionsOf(rows []gen.BlackboardEntry) []int32 {
	out := make([]int32, len(rows))
	for i, r := range rows {
		out[i] = r.Version
	}
	return out
}
