//go:build integration

package pgboard_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

const twoSteps = `{
	"schema_version": 1,
	"name": "pgboard-test",
	"steps": [{"id": "a", "type": "noop"}, {"id": "b", "type": "noop"}],
	"edges": []
}`

func newRun(t *testing.T) (*store.Store, uuid.UUID) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t))
	def, err := dag.Decode([]byte(twoSteps))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return s, res.Run.ID
}

func newBoard(t *testing.T, s *store.Store) *pgboard.Board {
	t.Helper()
	b, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	return b
}

// TestPutGetTokenCountAndCounter: a write is token-counted and its counter
// fingerprint stored; the value is canonicalized.
func TestPutGetTokenCountAndCounter(t *testing.T) {
	t.Parallel()
	s, runID := newRun(t)
	sb := newBoard(t, s).ForStep(runID, "a", 1, uuid.Nil, tokens.Fallback(), nil)

	// The value round-trips through Postgres JSONB (deterministic
	// reformatting), and its token_count is computed on the canonical
	// (compacted) form before storage — the ADR-014 determinism the stored
	// count rests on.
	entry, err := sb.Put(t.Context(), blackboard.PutArgs{
		Key: "draft", Value: json.RawMessage(`{ "text":  "hello world" }`), Tags: []string{"draft"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(entry.Value, &decoded); err != nil || decoded["text"] != "hello world" {
		t.Fatalf("value not stored correctly: %s (%v)", entry.Value, err)
	}
	if entry.TokenCounter != tokens.Fallback().ID() {
		t.Fatalf("counter = %q, want %q", entry.TokenCounter, tokens.Fallback().ID())
	}
	// Counted on the canonical (space-free) form, not the raw input.
	want := tokens.Fallback().Count(`{"text":"hello world"}`)
	if entry.TokenCount != want {
		t.Fatalf("token_count = %d, want %d (counted on the canonical form)", entry.TokenCount, want)
	}
	if !entry.Pinned() && entry.Tags[0] != "draft" {
		t.Fatalf("tags = %v", entry.Tags)
	}

	got, ok, err := sb.Get(t.Context(), "draft")
	if err != nil || !ok || got.Version != 1 {
		t.Fatalf("Get = %+v ok=%v err=%v", got, ok, err)
	}
}

// TestPinnedSugarAndTags: an explicit pinned tag survives; Get sees it.
func TestPinnedTag(t *testing.T) {
	t.Parallel()
	s, runID := newRun(t)
	sb := newBoard(t, s).ForStep(runID, "a", 1, uuid.Nil, tokens.Fallback(), nil)
	if _, err := sb.Put(t.Context(), blackboard.PutArgs{Key: "fact", Value: json.RawMessage(`"x"`), Tags: []string{"pinned", "fact"}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, _, _ := sb.Get(t.Context(), "fact")
	if !got.Pinned() {
		t.Fatalf("entry not pinned: tags %v", got.Tags)
	}
}

// TestCASConflictTranslated: a store CAS conflict surfaces as the leaf's
// typed error (the type the engine maps to a transient retry).
func TestCASConflictTranslated(t *testing.T) {
	t.Parallel()
	s, runID := newRun(t)
	sb := newBoard(t, s).ForStep(runID, "a", 1, uuid.Nil, tokens.Fallback(), nil)
	zero := 0
	if _, err := sb.Put(t.Context(), blackboard.PutArgs{Key: "k", Value: json.RawMessage(`1`), ExpectedVersion: &zero}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	_, err := sb.Put(t.Context(), blackboard.PutArgs{Key: "k", Value: json.RawMessage(`2`), ExpectedVersion: &zero})
	var ce *blackboard.VersionConflictError
	if !errors.As(err, &ce) || ce.Current != 1 {
		t.Fatalf("second Put = %v, want *blackboard.VersionConflictError{current 1}", err)
	}
}

// TestInvalidInputsPermanent: bad key/tag/value are typed permanent errors
// before any store call.
func TestInvalidInputsPermanent(t *testing.T) {
	t.Parallel()
	s, runID := newRun(t)
	sb := newBoard(t, s).ForStep(runID, "a", 1, uuid.Nil, tokens.Fallback(), nil)

	if _, err := sb.Put(t.Context(), blackboard.PutArgs{Key: "bad key", Value: json.RawMessage(`1`)}); err == nil {
		t.Fatal("bad key: want error")
	}
	if _, err := sb.Put(t.Context(), blackboard.PutArgs{Key: "k", Value: nil}); err == nil {
		t.Fatal("empty value: want error")
	}
	if _, err := sb.Put(t.Context(), blackboard.PutArgs{Key: "k", Value: json.RawMessage(`1`), Tags: []string{"Bad Tag"}}); err == nil {
		t.Fatal("bad tag: want error")
	}
}

func TestGetMissingKey(t *testing.T) {
	t.Parallel()
	s, runID := newRun(t)
	sb := newBoard(t, s).ForStep(runID, "a", 1, uuid.Nil, tokens.Fallback(), nil)
	_, ok, err := sb.Get(context.Background(), "nope")
	if err != nil || ok {
		t.Fatalf("Get(missing) = ok=%v err=%v; want false, nil", ok, err)
	}
}
