//go:build integration

package storetest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// TestParallelTestsAreIsolated is the acceptance test for ticket 2.2's
// "no cross-test data bleed": parallel subtests write different numbers of
// rows into an identically named table in their own NewDB databases; any
// shared state would make the counts wrong.
func TestParallelTestsAreIsolated(t *testing.T) {
	t.Parallel()
	const subtests = 4
	for i := range subtests {
		t.Run(fmt.Sprintf("db%d", i), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pool := storetest.NewDB(t)

			rows := i + 1 // a distinct count per subtest
			for range rows {
				if _, err := pool.Exec(ctx, "INSERT INTO schema_baseline (note) VALUES ($1)", t.Name()); err != nil {
					t.Fatalf("insert: %v", err)
				}
			}
			var got int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_baseline").Scan(&got); err != nil {
				t.Fatalf("count: %v", err)
			}
			if got != rows {
				t.Errorf("schema_baseline has %d rows, want %d — data bled across test databases", got, rows)
			}
		})
	}
}

// TestNewDBIsMigrated pins that NewDB hands tests a database already at the
// current schema version.
func TestNewDBIsMigrated(t *testing.T) {
	t.Parallel()
	pool := storetest.NewDB(t)
	var regclass *string
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass('schema_baseline')::text").Scan(&regclass); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	if regclass == nil {
		t.Fatal("NewDB database is missing schema_baseline — migrations did not run")
	}
}
