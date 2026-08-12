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

// columnExists reports whether a table column is visible in the connected
// database.
func columnExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var found bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = $1 AND column_name = $2)`, table, column).Scan(&found); err != nil {
		t.Fatalf("column lookup %s.%s: %v", table, column, err)
	}
	return found
}

// latestVersion is the highest migration in internal/store/migrations —
// bump when adding a migration (the round-trip test below walks every
// down migration regardless, so forgetting only fails the version check).
const latestVersion = 6

func TestMigrateUpDownRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, dsn := storetest.NewEmptyDB(t)

	mg, err := store.NewMigrator(dsn)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	defer mg.Close() //nolint:errcheck // best-effort cleanup in tests

	// Fresh database: no version yet, and Down names the situation instead
	// of leaking golang-migrate's ErrNilVersion.
	if _, _, applied, err := mg.Version(); err != nil || applied {
		t.Fatalf("Version on fresh database = applied %v, err %v; want not applied, nil", applied, err)
	}
	if err := mg.Down(); err == nil || !strings.Contains(err.Error(), "nothing to roll back") {
		t.Fatalf("Down on fresh database: %v, want the nothing-to-roll-back error", err)
	}

	// Up applies everything; the latest migration's tables exist, clean.
	if err := mg.Up(); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, table := range []string{"schema_baseline", "runs"} {
		if !tableExists(ctx, t, pool, table) {
			t.Fatalf("after Up: %s does not exist", table)
		}
	}
	version, dirty, applied, err := mg.Version()
	if err != nil || !applied || dirty || version != latestVersion {
		t.Fatalf("after Up: version=%d dirty=%v applied=%v err=%v; want %d, clean, applied",
			version, dirty, applied, err, latestVersion)
	}

	// Up with nothing pending is a no-op, not an error.
	if err := mg.Up(); err != nil {
		t.Fatalf("Up with nothing pending: %v", err)
	}

	// Down rolls back one step: the newest migration's additions are gone,
	// earlier ones untouched — 0006's side_effects table disappears while
	// 0005's dead_letters table and runs columns, 0004's timeout column,
	// 0003's retry columns, and the 0002 tables survive.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if tableExists(ctx, t, pool, "side_effects") {
		t.Fatal("after one Down: side_effects still exists")
	}
	if !tableExists(ctx, t, pool, "dead_letters") {
		t.Fatal("after one Down: dead_letters was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "runs", "on_failure") {
		t.Fatal("after one Down: runs.on_failure was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "runs", "steps_cancelled") {
		t.Fatal("after one Down: runs.steps_cancelled was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "run_steps", "timeout") {
		t.Fatal("after one Down: run_steps.timeout was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "run_steps", "retry_policy") {
		t.Fatal("after one Down: run_steps.retry_policy was dropped by the wrong migration")
	}
	if !tableExists(ctx, t, pool, "runs") {
		t.Fatal("after one Down: runs was dropped by the wrong migration")
	}
	if !tableExists(ctx, t, pool, "schema_baseline") {
		t.Fatal("after one Down: schema_baseline was dropped by the wrong migration")
	}

	// Walk the rest of the way to zero, exercising every down migration.
	for steps := 0; ; steps++ {
		if _, _, applied, err := mg.Version(); err != nil {
			t.Fatalf("Version while walking down: %v", err)
		} else if !applied {
			break
		}
		if steps > latestVersion {
			t.Fatalf("walked down %d steps without reaching an empty database", steps)
		}
		if err := mg.Down(); err != nil {
			t.Fatalf("Down (step %d): %v", steps, err)
		}
	}
	if tableExists(ctx, t, pool, "schema_baseline") {
		t.Fatal("after walking down to zero: schema_baseline still exists")
	}

	// And up again — the down migrations left a re-migratable database.
	if err := mg.Up(); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	for _, table := range []string{"schema_baseline", "runs"} {
		if !tableExists(ctx, t, pool, table) {
			t.Fatalf("after re-Up: %s does not exist", table)
		}
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
