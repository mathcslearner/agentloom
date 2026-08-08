// Package storetest provides the integration-test harness for code that
// talks to Postgres: each test gets its own freshly created database
// (migrated to the current schema by default), dropped again on cleanup, so
// parallel tests — including tests in different packages, which `go test`
// runs as separate processes — never see each other's data.
//
// The admin connection comes from AGENTLOOM_TEST_POSTGRES_DSN and defaults
// to the local compose stack (`make up`). Tests fail fast with a pointer to
// the stack when Postgres is unreachable: the `integration` build tag is an
// explicit opt-in, so a missing stack is an error, not a skip.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/store"
)

// EnvTestPostgresDSN overrides the admin DSN the harness connects with. It
// must be URL-form and its user must be allowed to CREATE/DROP DATABASE.
const EnvTestPostgresDSN = "AGENTLOOM_TEST_POSTGRES_DSN"

// createDBLockKey serializes CREATE DATABASE across test processes via a
// Postgres advisory lock: concurrent creates cloning template1 can fail with
// "source database is being accessed by other users", and a process-local
// mutex cannot cover `go test`'s per-package processes.
const createDBLockKey = 0x616c6f6f6d2202 // "aloom" + ticket 2.2

const opTimeout = 30 * time.Second

// NewDB creates a fresh database, applies all embedded migrations, and
// returns a pool connected to it. The database is dropped on test cleanup.
func NewDB(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	pool, dsn := NewEmptyDB(tb)
	mg, err := store.NewMigrator(dsn)
	if err != nil {
		tb.Fatalf("storetest: opening migrator: %v", err)
	}
	defer mg.Close() //nolint:errcheck // best-effort cleanup in tests
	if err := mg.Up(); err != nil {
		tb.Fatalf("storetest: migrating test database: %v", err)
	}
	return pool
}

// NewEmptyDB creates a fresh database with no migrations applied and returns
// a pool connected to it plus its DSN (for code, like the migration tests,
// that needs to open its own connections). The database is dropped on test
// cleanup.
func NewEmptyDB(tb testing.TB) (*pgxpool.Pool, string) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	adminDSN := adminDSN()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		tb.Fatalf("storetest: cannot reach Postgres at the admin DSN (is the dev stack running? try `make up`): %v", err)
	}
	defer admin.Close(ctx) //nolint:errcheck // best-effort cleanup in tests

	name := "agentloom_test_" + randomSuffix(tb)
	createDatabase(ctx, tb, admin, name)
	tb.Cleanup(func() { dropDatabase(tb, adminDSN, name) })

	dsn, err := dsnWithDatabase(adminDSN, name)
	if err != nil {
		tb.Fatalf("storetest: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		tb.Fatalf("storetest: opening pool for %s: %v", name, err)
	}
	tb.Cleanup(pool.Close)
	return pool, dsn
}

// adminDSN reads AGENTLOOM_TEST_POSTGRES_DSN, defaulting to the compose
// stack. Deliberately not AGENTLOOM_POSTGRES_DSN: pointing the dev tools at
// another database must never silently redirect the test harness too.
func adminDSN() string {
	if v, ok := os.LookupEnv(EnvTestPostgresDSN); ok && v != "" {
		return v
	}
	return config.DefaultPostgresDSN
}

func createDatabase(ctx context.Context, tb testing.TB, admin *pgx.Conn, name string) {
	tb.Helper()
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", createDBLockKey); err != nil {
		tb.Fatalf("storetest: acquiring create-database lock: %v", err)
	}
	_, createErr := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_unlock($1)", createDBLockKey); err != nil {
		tb.Errorf("storetest: releasing create-database lock: %v", err)
	}
	if createErr != nil {
		tb.Fatalf("storetest: creating database %s: %v", name, createErr)
	}
}

// dropDatabase removes the per-test database. WITH (FORCE) terminates any
// backend still connected (e.g. pool connections whose Close raced us).
func dropDatabase(tb testing.TB, adminDSN, name string) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		tb.Errorf("storetest: reconnecting to drop database %s: %v", name, err)
		return
	}
	defer admin.Close(ctx) //nolint:errcheck // best-effort cleanup in tests
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		tb.Errorf("storetest: dropping database %s: %v", name, err)
	}
}

// dsnWithDatabase returns adminDSN pointed at database name. The harness
// requires URL-form DSNs (the default is one).
func dsnWithDatabase(adminDSN, name string) (string, error) {
	u, err := url.Parse(adminDSN)
	if err != nil || u.Scheme == "" {
		// Never echo the DSN: it can embed credentials.
		return "", fmt.Errorf("%s must be a URL-form DSN (postgres://user:pass@host:port/db)", EnvTestPostgresDSN)
	}
	u.Path = "/" + name
	return u.String(), nil
}

func randomSuffix(tb testing.TB) string {
	tb.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		tb.Fatalf("storetest: generating database name: %v", err)
	}
	return hex.EncodeToString(b[:])
}
