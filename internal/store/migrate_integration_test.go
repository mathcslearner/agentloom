//go:build integration

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// tableExists reports whether a table is visible in the connected database.
func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var regclass *string
	if err := pool.QueryRow(ctx, "SELECT to_regclass($1)::text", name).Scan(&regclass); err != nil {
		t.Fatalf("to_regclass(%s): %v", name, err)
	}
	return regclass != nil
}

func TestMigrateUpDownRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, dsn := storetest.NewEmptyDB(t)

	mg, err := store.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	defer mg.Close() //nolint:errcheck // best-effort cleanup in tests

	// Fresh database: no version yet.
	if _, _, applied, err := mg.Version(); err != nil || applied {
		t.Fatalf("Version on fresh database = applied %v, err %v; want not applied, nil", applied, err)
	}

	// Up applies the baseline; version becomes 1, clean.
	if err := mg.Up(); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !tableExists(ctx, t, pool, "schema_baseline") {
		t.Fatal("after Up: schema_baseline does not exist")
	}
	version, dirty, applied, err := mg.Version()
	if err != nil || !applied || dirty || version != 1 {
		t.Fatalf("after Up: version=%d dirty=%v applied=%v err=%v; want 1, clean, applied", version, dirty, applied, err)
	}

	// Up with nothing pending is a no-op, not an error.
	if err := mg.Up(); err != nil {
		t.Fatalf("Up with nothing pending: %v", err)
	}

	// Down rolls back one step: table gone, version back to none.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if tableExists(ctx, t, pool, "schema_baseline") {
		t.Fatal("after Down: schema_baseline still exists")
	}
	if _, _, applied, err := mg.Version(); err != nil || applied {
		t.Fatalf("after Down: applied=%v err=%v; want not applied, nil", applied, err)
	}

	// And up again — the down migration left a re-migratable database.
	if err := mg.Up(); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	if !tableExists(ctx, t, pool, "schema_baseline") {
		t.Fatal("after re-Up: schema_baseline does not exist")
	}
}

func TestMigrateDirtyStateSurfacesClearError(t *testing.T) {
	t.Parallel()
	_, dsn := storetest.NewEmptyDB(t)

	broken := fstest.MapFS{
		"migrations/0001_broken.up.sql":   {Data: []byte("CREATE TABLE this is not valid sql;")},
		"migrations/0001_broken.down.sql": {Data: []byte("SELECT 1;")},
	}

	// First run dies mid-apply and leaves the dirty flag set.
	mg, err := store.NewMigratorFS(dsn, broken, "migrations")
	if err != nil {
		t.Fatalf("NewMigratorFS: %v", err)
	}
	if err := mg.Up(); err == nil {
		t.Fatal("Up with broken migration: want error, got nil")
	}
	if err := mg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A later run must refuse to proceed with a typed, actionable error.
	mg2, err := store.NewMigratorFS(dsn, broken, "migrations")
	if err != nil {
		t.Fatalf("NewMigratorFS (second): %v", err)
	}
	defer mg2.Close() //nolint:errcheck // best-effort cleanup in tests

	version, dirty, applied, err := mg2.Version()
	if err != nil || !applied || !dirty || version != 1 {
		t.Fatalf("Version after failed apply: version=%d dirty=%v applied=%v err=%v; want 1, dirty, applied", version, dirty, applied, err)
	}

	upErr := mg2.Up()
	if upErr == nil {
		t.Fatal("Up on dirty database: want error, got nil")
	}
	var dirtyErr *store.DirtyError
	if !errors.As(upErr, &dirtyErr) {
		t.Fatalf("Up on dirty database: error %v is not a *store.DirtyError", upErr)
	}
	if dirtyErr.Version != 1 {
		t.Errorf("DirtyError.Version = %d, want 1", dirtyErr.Version)
	}
	for _, want := range []string{"dirty", "version 1", "force"} {
		if !strings.Contains(upErr.Error(), want) {
			t.Errorf("dirty error %q does not mention %q", upErr, want)
		}
	}

	// Force(-1) clears the flag (back to "nothing applied") and the
	// database accepts migrations again.
	if err := mg2.Force(-1); err != nil {
		t.Fatalf("Force(-1): %v", err)
	}
	if _, dirty, applied, err := mg2.Version(); err != nil || dirty || applied {
		t.Fatalf("after Force(-1): dirty=%v applied=%v err=%v; want clean, not applied", dirty, applied, err)
	}
}
