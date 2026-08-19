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
const latestVersion = 29

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
	// earlier ones untouched. 0029 drops the two ops-view indexes
	// (dead_letters_created_idx, run_steps_dead_lettered_idx) — their revert is
	// observable while 0028's step_attempts.worker_id survives.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0029): %v", err)
	}
	if tableExists(ctx, t, pool, "dead_letters_created_idx") {
		t.Fatal("after one Down: dead_letters_created_idx was not dropped by 0029")
	}
	if tableExists(ctx, t, pool, "run_steps_dead_lettered_idx") {
		t.Fatal("after one Down: run_steps_dead_lettered_idx was not dropped by 0029")
	}
	if !columnExists(ctx, t, pool, "step_attempts", "worker_id") {
		t.Fatal("after one Down: step_attempts.worker_id (0028) was dropped prematurely")
	}

	// Down rolls back 0028: it drops step_attempts.worker_id (the claim
	// identity) — its revert is observable while everything below it survives,
	// starting with 0027's approvals.expired_at.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0028): %v", err)
	}
	if columnExists(ctx, t, pool, "step_attempts", "worker_id") {
		t.Fatal("after two Downs: step_attempts.worker_id was not dropped by 0028")
	}
	if !columnExists(ctx, t, pool, "approvals", "expired_at") {
		t.Fatal("after one Down: approvals.expired_at (0027) was dropped prematurely")
	}

	// Down rolls back 0027: it drops approvals.expired_at (the timeout
	// marker) — its revert is observable while 0026's run_edges.decision
	// survives.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0027): %v", err)
	}
	if columnExists(ctx, t, pool, "approvals", "expired_at") {
		t.Fatal("after two Downs: approvals.expired_at was not dropped by 0027")
	}
	if !columnExists(ctx, t, pool, "run_edges", "decision") {
		t.Fatal("after two Downs: run_edges.decision (0026) was dropped prematurely")
	}

	// Down again rolls back 0026: it drops run_edges.decision (the approval
	// edge marker) — its revert is observable while everything below it
	// survives, starting with 0025's approvals table.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0026): %v", err)
	}
	if columnExists(ctx, t, pool, "run_edges", "decision") {
		t.Fatal("after three Downs: run_edges.decision was not dropped by 0026")
	}
	if !tableExists(ctx, t, pool, "approvals") {
		t.Fatal("after three Downs: approvals (0025) was dropped prematurely")
	}

	// Down again rolls back 0025: it drops the approvals table (and narrows
	// the run_steps status CHECK back off awaiting_human) — its revert is
	// observable while everything below it survives, starting with 0024's
	// runs.steps_collected.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0025): %v", err)
	}
	if tableExists(ctx, t, pool, "approvals") {
		t.Fatal("after three Downs: approvals table was not dropped by 0025")
	}
	if !columnExists(ctx, t, pool, "runs", "steps_collected") {
		t.Fatal("after three Downs: runs.steps_collected (0024) was dropped prematurely")
	}

	// Down again rolls back 0024: it drops runs.steps_collected — its revert
	// is observable while everything below it survives, starting with 0023's
	// run_steps.depth / runs.expansion_caps.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0024): %v", err)
	}
	if columnExists(ctx, t, pool, "runs", "steps_collected") {
		t.Fatal("after three Downs: runs.steps_collected was not dropped by 0024")
	}
	if !columnExists(ctx, t, pool, "run_steps", "depth") {
		t.Fatal("after one Down: run_steps.depth (0023) was dropped prematurely")
	}
	if !columnExists(ctx, t, pool, "runs", "expansion_caps") {
		t.Fatal("after one Down: runs.expansion_caps (0023) was dropped prematurely")
	}
	if !columnExists(ctx, t, pool, "run_steps", "context_policy") {
		t.Fatal("after one Down: run_steps.context_policy was dropped by the wrong migration")
	}

	// Down again rolls back 0023: it drops run_steps.depth and
	// runs.expansion_caps — their revert is observable while everything below
	// (starting with 0022's context_policy) survives.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0023): %v", err)
	}
	if columnExists(ctx, t, pool, "run_steps", "depth") {
		t.Fatal("after two Downs: run_steps.depth was not dropped by 0023")
	}
	if columnExists(ctx, t, pool, "runs", "expansion_caps") {
		t.Fatal("after two Downs: runs.expansion_caps was not dropped by 0023")
	}
	if !columnExists(ctx, t, pool, "run_steps", "context_policy") {
		t.Fatal("after two Downs: run_steps.context_policy (0022) was dropped prematurely")
	}

	// Down again rolls back 0022: it drops the run_steps.context_policy column
	// — its revert is observable while everything below it survives, starting
	// with 0021's blackboard_policy column.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0022): %v", err)
	}
	if columnExists(ctx, t, pool, "run_steps", "context_policy") {
		t.Fatal("after three Downs: run_steps.context_policy was not dropped by 0022")
	}
	if !columnExists(ctx, t, pool, "run_steps", "blackboard_policy") {
		t.Fatal("after three Downs: run_steps.blackboard_policy was dropped by the wrong migration")
	}

	// Down again rolls back 0021: it drops the blackboard_entries table and
	// the run_steps.blackboard_policy column — its revert is observable by
	// them being gone while everything below it survives: 0020's semantic-retry
	// feedback columns, 0019's step_attempts.repair, 0018's
	// step_attempts.verdict and run_steps.validation_policy, 0017's runs
	// budget columns and run_steps.budget_policy, 0016's cost_ledger table and
	// runs cost columns, 0015's run_steps.cache_policy column, 0013's
	// retrieval_docs table, 0012's step_attempts.usage column, 0011's
	// step_logs table, 0010's trace columns, 0009's fingerprint column,
	// 0008's api_keys table, 0007's run-control columns, 0006's side_effects
	// table, 0005's dead_letters table and runs columns, 0004's timeout
	// column, 0003's retry columns, and the 0002 tables all persist.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0021): %v", err)
	}
	if tableExists(ctx, t, pool, "blackboard_entries") {
		t.Fatal("after two Downs: blackboard_entries table was not dropped by 0021")
	}
	if columnExists(ctx, t, pool, "run_steps", "blackboard_policy") {
		t.Fatal("after two Downs: run_steps.blackboard_policy was not dropped by 0021")
	}
	if !columnExists(ctx, t, pool, "run_steps", "feedback") {
		t.Fatal("after two Downs: run_steps.feedback was dropped by the wrong migration")
	}
	// Roll back 0020 too so the assertions below (which predate it) hold.
	if err := mg.Down(); err != nil {
		t.Fatalf("Down (0020): %v", err)
	}
	if columnExists(ctx, t, pool, "run_steps", "feedback") {
		t.Fatal("after three Downs: run_steps.feedback was not dropped by 0020")
	}
	if columnExists(ctx, t, pool, "step_attempts", "feedback") {
		t.Fatal("after one Down: step_attempts.feedback was not dropped by 0020")
	}
	if !columnExists(ctx, t, pool, "step_attempts", "repair") {
		t.Fatal("after one Down: step_attempts.repair was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "step_attempts", "verdict") {
		t.Fatal("after one Down: step_attempts.verdict was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "run_steps", "validation_policy") {
		t.Fatal("after one Down: run_steps.validation_policy was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "runs", "budget_nano_usd") {
		t.Fatal("after one Down: runs.budget_nano_usd was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "run_steps", "budget_policy") {
		t.Fatal("after one Down: run_steps.budget_policy was dropped by the wrong migration")
	}
	if !tableExists(ctx, t, pool, "cost_ledger") {
		t.Fatal("after one Down: cost_ledger was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "runs", "spent_nano_usd") {
		t.Fatal("after one Down: runs.spent_nano_usd was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "run_steps", "cache_policy") {
		t.Fatal("after one Down: run_steps.cache_policy was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "run_steps", "timeout") {
		t.Fatal("after one Down: run_steps.timeout was dropped by the wrong migration")
	}
	if !tableExists(ctx, t, pool, "retrieval_docs") {
		t.Fatal("after one Down: retrieval_docs was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "step_attempts", "usage") {
		t.Fatal("after one Down: step_attempts.usage was dropped by the wrong migration")
	}
	if !tableExists(ctx, t, pool, "step_logs") {
		t.Fatal("after one Down: step_logs was dropped by the wrong migration")
	}
	for table, column := range map[string]string{
		"runs": "trace_parent", "task_outbox": "trace_parent", "run_steps": "trace_span",
	} {
		if !columnExists(ctx, t, pool, table, column) {
			t.Fatalf("after one Down: %s.%s was dropped by the wrong migration", table, column)
		}
	}
	if !columnExists(ctx, t, pool, "runs", "idempotency_fingerprint") {
		t.Fatal("after one Down: runs.idempotency_fingerprint was dropped by the wrong migration")
	}
	if !tableExists(ctx, t, pool, "api_keys") {
		t.Fatal("after one Down: api_keys was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "runs", "deadline_at") {
		t.Fatal("after one Down: runs.deadline_at was dropped by the wrong migration")
	}
	if !columnExists(ctx, t, pool, "runs", "park_reason") {
		t.Fatal("after one Down: runs.park_reason was dropped by the wrong migration")
	}
	if !tableExists(ctx, t, pool, "side_effects") {
		t.Fatal("after one Down: side_effects was dropped by the wrong migration")
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
