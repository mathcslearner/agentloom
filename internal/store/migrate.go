package store

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5 database driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// migrationsFS embeds the SQL migration files so binaries carry their schema
// with them — no migration files to ship or locate at run time.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// DirtyError reports a database whose last migration attempt died mid-apply:
// golang-migrate recorded the target version with a dirty flag before running
// it, and the run never finished. No further migrations will apply until the
// flag is cleared.
type DirtyError struct {
	// Version is the migration version whose apply did not complete.
	Version int
}

func (e *DirtyError) Error() string {
	return fmt.Sprintf(
		"database is dirty at migration version %d: a previous migration run failed mid-apply; "+
			"inspect the database, manually complete or undo that migration's effects, "+
			"then clear the flag with `go run ./cmd/migrate force %d` and retry",
		e.Version, e.Version)
}

// Migrator applies and rolls back schema migrations against one database.
// Close it when done; it holds a database connection.
type Migrator struct {
	m *migrate.Migrate
}

// NewMigrator opens a Migrator for the database at dsn using the embedded
// migration files. dsn must be in URL form (postgres://...).
func NewMigrator(dsn string) (*Migrator, error) {
	return NewMigratorFS(dsn, migrationsFS, "migrations")
}

// NewMigratorFS is NewMigrator with an explicit migration source: fsys is the
// filesystem holding the migration files and dir the directory within it.
// Production code uses NewMigrator; this exists so tests can inject broken or
// synthetic migrations.
func NewMigratorFS(dsn string, fsys fs.FS, dir string) (*Migrator, error) {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("store: loading migration source: %w", err)
	}
	migrateDSN, err := toMigrateDSN(dsn)
	if err != nil {
		return nil, err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateDSN)
	if err != nil {
		return nil, fmt.Errorf("store: opening migrator: %w", err)
	}
	return &Migrator{m: m}, nil
}

// toMigrateDSN rewrites a postgres:// DSN to the pgx5:// scheme golang-migrate
// uses to select its pgx/v5 driver (which restores the scheme before
// connecting). Migrations require URL-form DSNs; keyword/value form is
// rejected here with a clear error rather than a parse failure downstream.
func toMigrateDSN(dsn string) (string, error) {
	// Never echo the DSN in errors: it can embed credentials.
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return "", errors.New("store: migrations need a URL-form DSN (postgres://user:pass@host:port/db)")
	}
	switch u.Scheme {
	case "postgres", "postgresql", "pgx5":
		u.Scheme = "pgx5"
		return u.String(), nil
	default:
		return "", fmt.Errorf("store: unsupported DSN scheme %q (want postgres://)", u.Scheme)
	}
}

// Up applies every pending migration. Nothing pending is not an error.
func (mg *Migrator) Up() error {
	if err := mg.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store: migrate up: %w", wrapDirty(err))
	}
	return nil
}

// Down rolls back the most recent migration (one step, not all the way).
func (mg *Migrator) Down() error {
	if err := mg.m.Steps(-1); err != nil {
		return fmt.Errorf("store: migrate down: %w", wrapDirty(err))
	}
	return nil
}

// Force overwrites the recorded schema version and clears the dirty flag
// without running any migrations (-1 means "nothing applied"). It is the
// manual recovery step after a failed apply — only use it once the database
// state has been fixed by hand.
func (mg *Migrator) Force(version int) error {
	if err := mg.m.Force(version); err != nil {
		return fmt.Errorf("store: migrate force %d: %w", version, err)
	}
	return nil
}

// Version reports the current schema version and whether the database is
// dirty. applied is false when no migration has ever been applied.
func (mg *Migrator) Version() (version uint, dirty bool, applied bool, err error) {
	version, dirty, err = mg.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("store: reading migration version: %w", err)
	}
	return version, dirty, true, nil
}

// Close releases the migrator's source and database connections.
func (mg *Migrator) Close() error {
	srcErr, dbErr := mg.m.Close()
	if err := errors.Join(srcErr, dbErr); err != nil {
		return fmt.Errorf("store: closing migrator: %w", err)
	}
	return nil
}

// wrapDirty converts golang-migrate's dirty-database error into the typed,
// actionable DirtyError; every other error passes through unchanged.
func wrapDirty(err error) error {
	var dirty migrate.ErrDirty
	if errors.As(err, &dirty) {
		return &DirtyError{Version: dirty.Version}
	}
	return err
}
