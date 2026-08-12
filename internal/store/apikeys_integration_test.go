//go:build integration

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// keyParams builds minimal CreateAPIKeyParams. The "hash" and prefix are
// arbitrary strings by design: the store layer never sees plaintext and
// treats both as opaque.
func keyParams(name, prefix, hash string, scopes []string, at time.Time) gen.CreateAPIKeyParams {
	return gen.CreateAPIKeyParams{
		Name:      name,
		Prefix:    prefix,
		KeyHash:   hash,
		Scopes:    scopes,
		CreatedAt: at,
	}
}

func TestAPIKeysCRUD(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	keys := s.APIKeys()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	expires := now.Add(24 * time.Hour)
	created, err := keys.Create(ctx, gen.CreateAPIKeyParams{
		Name:      "ci",
		Prefix:    "sk_11111111",
		KeyHash:   "hash-1",
		Scopes:    []string{"submit", "read"},
		CreatedAt: now,
		ExpiresAt: &expires,
	})
	if err != nil {
		t.Fatalf("creating key: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("zero-ID create did not assign a UUID")
	}
	if created.RevokedAt != nil {
		t.Fatalf("fresh key already revoked: %v", created.RevokedAt)
	}

	byPrefix, err := keys.GetByPrefix(ctx, "sk_11111111")
	if err != nil {
		t.Fatalf("get by prefix: %v", err)
	}
	if byPrefix.ID != created.ID || byPrefix.KeyHash != "hash-1" {
		t.Fatalf("get by prefix returned the wrong row: %+v", byPrefix)
	}
	if _, err := keys.GetByPrefix(ctx, "sk_nope0000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown prefix: want ErrNotFound, got %v", err)
	}

	if _, err := keys.Get(ctx, created.ID); err != nil {
		t.Fatalf("get by id: %v", err)
	}

	// Second key, created later, to pin newest-first list order.
	second, err := keys.Create(ctx, keyParams("ops", "sk_22222222", "hash-2", []string{"admin"}, now.Add(time.Hour)))
	if err != nil {
		t.Fatalf("creating second key: %v", err)
	}
	list, err := keys.List(ctx)
	if err != nil {
		t.Fatalf("listing keys: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 keys, got %d", len(list))
	}
	if list[0].ID != second.ID || list[1].ID != created.ID {
		t.Fatalf("list not newest-first: %v then %v", list[0].Name, list[1].Name)
	}
}

func TestAPIKeysUniqueViolationsAreConflicts(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	keys := s.APIKeys()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	if _, err := keys.Create(ctx, keyParams("a", "sk_aaaaaaaa", "hash-a", []string{"read"}, now)); err != nil {
		t.Fatalf("creating key: %v", err)
	}

	var conflict *store.ConflictError
	_, err := keys.Create(ctx, keyParams("b", "sk_aaaaaaaa", "hash-b", []string{"read"}, now))
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate prefix: want ConflictError, got %v", err)
	}
	_, err = keys.Create(ctx, keyParams("c", "sk_cccccccc", "hash-a", []string{"read"}, now))
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate hash: want ConflictError, got %v", err)
	}

	// The scopes CHECK rejects out-of-vocabulary and empty sets; neither
	// maps to a typed store error (they are programming errors, not races).
	if _, err := keys.Create(ctx, keyParams("d", "sk_dddddddd", "hash-d", []string{"root"}, now)); err == nil {
		t.Fatal("out-of-vocabulary scope accepted")
	}
	if _, err := keys.Create(ctx, keyParams("e", "sk_eeeeeeee", "hash-e", []string{}, now)); err == nil {
		t.Fatal("empty scope set accepted")
	}
}

func TestAPIKeysRevokeIsFirstWins(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	keys := s.APIKeys()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	key, err := keys.Create(ctx, keyParams("a", "sk_aaaaaaaa", "hash-a", []string{"read"}, now))
	if err != nil {
		t.Fatalf("creating key: %v", err)
	}

	revoked, err := keys.Revoke(ctx, key.ID, now.Add(time.Minute))
	if err != nil || !revoked {
		t.Fatalf("first revoke: want (true, nil), got (%v, %v)", revoked, err)
	}
	// Second revoke is an idempotent no-op keeping the original timestamp.
	revoked, err = keys.Revoke(ctx, key.ID, now.Add(2*time.Minute))
	if err != nil || revoked {
		t.Fatalf("second revoke: want (false, nil), got (%v, %v)", revoked, err)
	}
	row, err := keys.Get(ctx, key.ID)
	if err != nil {
		t.Fatalf("get after revoke: %v", err)
	}
	if row.RevokedAt == nil || !row.RevokedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("revoked_at overwritten: %v", row.RevokedAt)
	}

	if _, err := keys.Revoke(ctx, uuid.New(), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoking unknown id: want ErrNotFound, got %v", err)
	}
}
