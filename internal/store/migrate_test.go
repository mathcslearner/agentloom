package store_test

import (
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/store"
)

// TestToMigrateDSN pins the DSN handling NewMigrator promises: URL schemes
// postgres/postgresql/pgx5 rewrite to pgx5 (golang-migrate's pgx/v5 driver
// registration), everything else is rejected — and no error ever echoes
// the DSN, which can embed credentials.
func TestToMigrateDSN(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-password"

	t.Run("accepted schemes rewrite to pgx5", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ in, want string }{
			{"postgres://u:pw@localhost:5432/db?sslmode=disable", "pgx5://u:pw@localhost:5432/db?sslmode=disable"},
			{"postgresql://u:pw@localhost:5432/db", "pgx5://u:pw@localhost:5432/db"},
			{"pgx5://u:pw@localhost:5432/db", "pgx5://u:pw@localhost:5432/db"},
		} {
			got, err := store.ToMigrateDSN(tc.in)
			if err != nil {
				t.Errorf("ToMigrateDSN(%q): %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("ToMigrateDSN(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("rejections never echo the DSN", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{
			"host=localhost user=u password=" + secret + " dbname=db", // keyword/value form
			"mysql://u:" + secret + "@localhost:3306/db",              // unsupported scheme
			"",
		} {
			_, err := store.ToMigrateDSN(in)
			if err == nil {
				t.Errorf("ToMigrateDSN(%q) accepted", in)
				continue
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("ToMigrateDSN(%q) error echoes credentials: %v", in, err)
			}
		}
	})
}
