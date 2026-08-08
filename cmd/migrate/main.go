// Command migrate is the dev-time schema migration tool, wrapping the
// embedded migrations in internal/store. It is invoked through the Make
// targets migrate-up / migrate-down / migrate-new and reads the target
// database from AGENTLOOM_POSTGRES_DSN (default: the local compose stack).
//
// Usage:
//
//	migrate up             apply every pending migration
//	migrate down           roll back the most recent migration
//	migrate status         print current version and dirty flag
//	migrate force <ver>    clear the dirty flag after manual repair
//	migrate new <name>     create the next NNNN_<name>.{up,down}.sql pair
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/store"
)

// migrationsDir is where `migrate new` writes files, relative to the repo
// root (the tool is run via `go run ./cmd/migrate` from there).
const migrationsDir = "internal/store/migrations"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 {
		return usageError()
	}
	cmd, rest := args[0], args[1:]

	// `new` is purely file-system-side; everything else talks to Postgres.
	if cmd == "new" {
		if len(rest) != 1 {
			return fmt.Errorf("usage: migrate new <name> (e.g. migrate new add_runs_table)")
		}
		return newMigration(migrationsDir, rest[0])
	}

	// Validate the command (and force's argument) before touching the
	// database — an unknown command must not require a reachable Postgres.
	var forceVersion int
	switch cmd {
	case "up", "down", "status":
	case "force":
		if len(rest) != 1 {
			return fmt.Errorf("usage: migrate force <version>")
		}
		v, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("force: version must be an integer, got %q", rest[0])
		}
		forceVersion = v
	default:
		return usageError()
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	mg, err := store.NewMigrator(cfg.Postgres.DSN)
	if err != nil {
		return err
	}
	defer mg.Close() //nolint:errcheck // best-effort cleanup on a dev tool

	switch cmd {
	case "up":
		if err := mg.Up(); err != nil {
			return err
		}
		return printStatus(mg, "up complete")
	case "down":
		if err := mg.Down(); err != nil {
			return err
		}
		return printStatus(mg, "down complete (one step)")
	case "status":
		return printStatus(mg, "")
	default: // "force" — validated above
		if err := mg.Force(forceVersion); err != nil {
			return err
		}
		return printStatus(mg, "dirty flag cleared")
	}
}

func usageError() error {
	return fmt.Errorf("usage: migrate <up | down | status | force <version> | new <name>>")
}

func printStatus(mg *store.Migrator, prefix string) error {
	version, dirty, applied, err := mg.Version()
	if err != nil {
		return err
	}
	if prefix != "" {
		prefix += "; "
	}
	switch {
	case !applied:
		fmt.Printf("%sschema version: none (no migrations applied)\n", prefix)
	case dirty:
		fmt.Printf("%sschema version: %d DIRTY (see `migrate force`)\n", prefix, version)
	default:
		fmt.Printf("%sschema version: %d\n", prefix, version)
	}
	return nil
}

// migrationName restricts names to snake_case so file names stay uniform.
var migrationName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// newMigration creates an empty NNNN_<name>.up.sql / .down.sql pair in dir
// using the next sequence number after the highest one present. dir is a
// parameter so tests can point it at a temp directory; production passes
// migrationsDir.
func newMigration(dir, name string) error {
	if !migrationName.MatchString(name) {
		return fmt.Errorf("new: name must be snake_case ([a-z][a-z0-9_]*), got %q", name)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("new: reading %s (run from the repo root): %w", dir, err)
	}
	next := 1
	seq := regexp.MustCompile(`^(\d+)_`)
	for _, e := range entries {
		m := seq.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err == nil && n >= next {
			next = n + 1
		}
	}
	for _, suffix := range []string{"up", "down"} {
		// name is constrained to snake_case by migrationName above, so the
		// joined path cannot escape dir.
		path := filepath.Join(dir, fmt.Sprintf("%04d_%s.%s.sql", next, name, suffix))
		if err := os.WriteFile(path, []byte("-- Write the "+suffix+" migration here.\n"), fs.FileMode(0o644)); err != nil { //nolint:gosec // G306: migration stubs are world-readable source files, not secrets
			return fmt.Errorf("new: %w", err)
		}
		fmt.Println("created", path)
	}
	return nil
}
