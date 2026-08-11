//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Postgres error codes asserted below (pgconn exposes them as strings).
const (
	pgCheckViolation      = "23514"
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

// wantPgError asserts err is a Postgres error with the given SQLSTATE code.
func wantPgError(t *testing.T, err error, code, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want SQLSTATE %s, got no error", context, code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: error %v is not a *pgconn.PgError", context, err)
	}
	if pgErr.Code != code {
		t.Fatalf("%s: SQLSTATE = %s (%s), want %s", context, pgErr.Code, pgErr.Message, code)
	}
}

// insertRun inserts a minimal valid run and returns its id.
func insertRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO runs (definition, status) VALUES ('{}'::jsonb, 'running') RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("inserting run: %v", err)
	}
	return id
}

// insertStep inserts a minimal valid run_steps row.
func insertStep(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID, stepID string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO run_steps (run_id, step_id, step_type, retry_policy) VALUES ($1, $2, 'noop', '{}'::jsonb)`,
		runID, stepID)
	if err != nil {
		t.Fatalf("inserting run_steps %s: %v", stepID, err)
	}
}

func TestSchemaV1TablesAndIndexesExist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := storetest.NewDB(t)

	for _, table := range []string{
		"workflow_definitions", "runs", "run_steps", "run_edges",
		"step_attempts", "events", "task_outbox",
	} {
		var regclass *string
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1)::text", table).Scan(&regclass); err != nil {
			t.Fatalf("to_regclass(%s): %v", table, err)
		}
		if regclass == nil {
			t.Errorf("table %s does not exist", table)
		}
	}

	// The hot-path indexes named in ADR-004.
	for _, index := range []string{
		"run_steps_inflight_idx",     // reconciler: in-flight steps by staleness
		"runs_status_created_at_idx", // run listing / status rollup
		"runs_idempotency_token_key", // idempotent submission (partial unique)
	} {
		var one int
		err := pool.QueryRow(ctx,
			"SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1", index,
		).Scan(&one)
		if err != nil {
			t.Errorf("index %s does not exist (%v)", index, err)
		}
	}
}

func TestSchemaV1StatusChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := storetest.NewDB(t)
	runID := insertRun(ctx, t, pool)

	_, err := pool.Exec(ctx,
		`INSERT INTO runs (definition, status) VALUES ('{}'::jsonb, 'parked')`)
	wantPgError(t, err, pgCheckViolation, "run status outside v1 vocabulary")

	_, err = pool.Exec(ctx,
		`INSERT INTO run_steps (run_id, step_id, step_type, retry_policy, status) VALUES ($1, 'a', 'noop', '{}'::jsonb, 'awaiting_human')`,
		runID)
	wantPgError(t, err, pgCheckViolation, "step status outside v1 vocabulary")

	_, err = pool.Exec(ctx,
		`INSERT INTO run_edges (run_id, ordinal, from_step, to_step, edge_type) VALUES ($1, 0, 'a', 'b', 'sideways')`,
		runID)
	wantPgError(t, err, pgCheckViolation, "invalid edge_type")

	_, err = pool.Exec(ctx,
		`INSERT INTO run_edges (run_id, ordinal, from_step, to_step, resolution) VALUES ($1, 0, 'a', 'b', 'pending')`,
		runID)
	wantPgError(t, err, pgCheckViolation, "invalid edge resolution")
}

func TestSchemaV1UniquenessRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := storetest.NewDB(t)
	runID := insertRun(ctx, t, pool)

	// events: (run_id, seq) is the primary key.
	if _, err := pool.Exec(ctx,
		`INSERT INTO events (run_id, seq, type) VALUES ($1, 1, 'run_created')`, runID); err != nil {
		t.Fatalf("inserting event: %v", err)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO events (run_id, seq, type) VALUES ($1, 1, 'step_ready')`, runID)
	wantPgError(t, err, pgUniqueViolation, "duplicate (run_id, seq) event")

	// runs: idempotency_token unique among non-null; nulls unconstrained.
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (definition, status, idempotency_token) VALUES ('{}'::jsonb, 'running', 'tok-1')`); err != nil {
		t.Fatalf("inserting run with token: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO runs (definition, status, idempotency_token) VALUES ('{}'::jsonb, 'running', 'tok-1')`)
	wantPgError(t, err, pgUniqueViolation, "duplicate idempotency token")
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (definition, status) VALUES ('{}'::jsonb, 'running')`); err != nil {
		t.Fatalf("second run with null token should be allowed: %v", err)
	}

	// workflow_definitions: (name, version) unique.
	if _, err := pool.Exec(ctx,
		`INSERT INTO workflow_definitions (name, version, spec) VALUES ('wf', 1, '{}'::jsonb)`); err != nil {
		t.Fatalf("inserting definition: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO workflow_definitions (name, version, spec) VALUES ('wf', 1, '{}'::jsonb)`)
	wantPgError(t, err, pgUniqueViolation, "duplicate (name, version) definition")
	if _, err := pool.Exec(ctx,
		`INSERT INTO workflow_definitions (name, version, spec) VALUES ('wf', 2, '{}'::jsonb)`); err != nil {
		t.Fatalf("new version of same name should be allowed: %v", err)
	}
}

func TestSchemaV1AttemptRequiresStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := storetest.NewDB(t)
	runID := insertRun(ctx, t, pool)

	_, err := pool.Exec(ctx,
		`INSERT INTO step_attempts (run_id, step_id, attempt_no, claim_id)
		 VALUES ($1, 'ghost', 1, gen_random_uuid())`, runID)
	wantPgError(t, err, pgForeignKeyViolation, "attempt without its run_steps row")
}

func TestSchemaV1DeleteRunCascades(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := storetest.NewDB(t)
	runID := insertRun(ctx, t, pool)
	insertStep(ctx, t, pool, runID, "a")

	for _, ins := range []string{
		`INSERT INTO run_edges (run_id, ordinal, from_step, to_step) VALUES ($1, 0, 'a', 'a')`,
		`INSERT INTO step_attempts (run_id, step_id, attempt_no, claim_id) VALUES ($1, 'a', 1, gen_random_uuid())`,
		`INSERT INTO events (run_id, seq, type) VALUES ($1, 1, 'run_created')`,
	} {
		if _, err := pool.Exec(ctx, ins, runID); err != nil {
			t.Fatalf("seeding child row: %v", err)
		}
	}

	if _, err := pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, runID); err != nil {
		t.Fatalf("deleting run: %v", err)
	}
	for _, table := range []string{"run_steps", "run_edges", "step_attempts", "events"} {
		var n int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE run_id = $1", runID).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows survived the run delete, want 0", table, n)
		}
	}
}

func TestSchemaV1DefinitionWithRunsCannotBeDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := storetest.NewDB(t)

	var defID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workflow_definitions (name, version, spec) VALUES ('wf', 1, '{}'::jsonb) RETURNING id`,
	).Scan(&defID); err != nil {
		t.Fatalf("inserting definition: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (definition_id, definition, status) VALUES ($1, '{}'::jsonb, 'running')`,
		defID); err != nil {
		t.Fatalf("inserting run referencing definition: %v", err)
	}

	_, err := pool.Exec(ctx, `DELETE FROM workflow_definitions WHERE id = $1`, defID)
	wantPgError(t, err, pgForeignKeyViolation, "deleting a definition with runs on record")
}
