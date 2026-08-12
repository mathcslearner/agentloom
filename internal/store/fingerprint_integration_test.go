//go:build integration

package store_test

// Ticket 6.5's idempotency hardening at the store layer: the token is
// fingerprinted to its payload (definition snapshot + canonicalized params
// + definition ref), a mismatched replay is a typed refusal, the token
// length is bounded, and pre-0009 rows (NULL fingerprint) stay reuseable.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

func TestCreateRunFingerprintMismatch(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	def := loadFixture(t, "linear.json")

	first, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: def, Params: json.RawMessage(`{"a": 1, "b": 2}`),
		IdempotencyToken: "fp-tok", Now: testNow,
	})
	if err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}

	// Same payload, params reordered: canonicalization absorbs formatting.
	replay, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: def, Params: json.RawMessage(`{"b":2,"a":1}`),
		IdempotencyToken: "fp-tok", Now: testNow,
	})
	if err != nil {
		t.Fatalf("reordered replay: %v", err)
	}
	if !replay.Reused || replay.Run.ID != first.Run.ID {
		t.Errorf("reordered replay = %+v, want reuse of %s", replay, first.Run.ID)
	}

	// Different params: typed mismatch naming the original run.
	var mismatch *store.IdempotencyMismatchError
	_, err = s.CreateRun(ctx, store.CreateRunArgs{
		Definition: def, Params: json.RawMessage(`{"a": 999}`),
		IdempotencyToken: "fp-tok", Now: testNow,
	})
	if !errors.As(err, &mismatch) {
		t.Fatalf("different-params replay error = %v, want *IdempotencyMismatchError", err)
	}
	if mismatch.RunID != first.Run.ID || mismatch.Token != "fp-tok" {
		t.Errorf("mismatch = %+v, want run %s / token fp-tok", mismatch, first.Run.ID)
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Error("mismatch does not unwrap to ErrConflict")
	}

	// Different definition under the same token: mismatch too.
	other := loadFixture(t, "fanout.json")
	_, err = s.CreateRun(ctx, store.CreateRunArgs{
		Definition: other, Params: json.RawMessage(`{"a": 1, "b": 2}`),
		IdempotencyToken: "fp-tok", Now: testNow,
	})
	if !errors.As(err, &mismatch) {
		t.Fatalf("different-definition replay error = %v, want *IdempotencyMismatchError", err)
	}

	// The ref form is part of the identity: the same document submitted
	// with a DefinitionID is not the inline submission.
	defID := mustCreateDefinition(t, s, def)
	_, err = s.CreateRun(ctx, store.CreateRunArgs{
		Definition: def, DefinitionID: &defID, Params: json.RawMessage(`{"a": 1, "b": 2}`),
		IdempotencyToken: "fp-tok", Now: testNow,
	})
	if !errors.As(err, &mismatch) {
		t.Fatalf("ref-form replay error = %v, want *IdempotencyMismatchError", err)
	}
}

func TestCreateRunTokenLengthBound(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	def := loadFixture(t, "linear.json")

	_, err := s.CreateRun(t.Context(), store.CreateRunArgs{
		Definition:       def,
		IdempotencyToken: strings.Repeat("k", store.MaxIdempotencyTokenLength+1),
		Now:              testNow,
	})
	if err == nil || !strings.Contains(err.Error(), "idempotency token exceeds") {
		t.Fatalf("over-long token error = %v, want the length-bound refusal", err)
	}

	// The bound itself passes.
	if _, err := s.CreateRun(t.Context(), store.CreateRunArgs{
		Definition:       def,
		IdempotencyToken: strings.Repeat("k", store.MaxIdempotencyTokenLength),
		Now:              testNow,
	}); err != nil {
		t.Fatalf("bound-length token: %v", err)
	}
}

func TestCreateRunLegacyRowsGrandfathered(t *testing.T) {
	t.Parallel()
	pool := storetest.NewDB(t)
	s := store.NewFromPool(pool)
	ctx := t.Context()
	def := loadFixture(t, "linear.json")

	first, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: def, IdempotencyToken: "legacy-tok", Now: testNow,
	})
	if err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}
	// Simulate a pre-0009 row: token present, fingerprint NULL.
	if _, err := pool.Exec(context.Background(),
		`UPDATE runs SET idempotency_fingerprint = NULL WHERE id = $1`, first.Run.ID); err != nil {
		t.Fatalf("clearing fingerprint: %v", err)
	}

	// Any payload reuses it — the pre-6.5 semantics, grandfathered.
	replay, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: loadFixture(t, "fanout.json"), Params: json.RawMessage(`{"different": true}`),
		IdempotencyToken: "legacy-tok", Now: testNow,
	})
	if err != nil {
		t.Fatalf("legacy replay: %v", err)
	}
	if !replay.Reused || replay.Run.ID != first.Run.ID {
		t.Errorf("legacy replay = %+v, want unchecked reuse of %s", replay, first.Run.ID)
	}
}
