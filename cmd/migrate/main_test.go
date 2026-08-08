package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunUsageErrors covers the argument validation that needs no database:
// missing/unknown commands and `new` without a name.
func TestRunUsageErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"new"},
		{"new", "too", "many"},
		{"force"},
		{"force", "1", "2"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run(%q) succeeded, want a usage error", args)
		} else if !strings.Contains(err.Error(), "usage:") {
			t.Errorf("run(%q) error = %v, want a usage error", args, err)
		}
	}

	// force with a non-integer version is rejected before any database
	// connection is attempted.
	if err := run([]string{"force", "abc"}); err == nil || !strings.Contains(err.Error(), "must be an integer") {
		t.Errorf("run(force abc) error = %v, want the integer-validation error", err)
	}
}

func TestNewMigration(t *testing.T) {
	t.Parallel()

	t.Run("rejects non-snake_case names", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for _, name := range []string{"Add-Table", "1starts_with_digit", "has space", "UPPER", ""} {
			if err := newMigration(dir, name); err == nil {
				t.Errorf("newMigration accepted %q", name)
			}
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("rejected names still created files: %v", entries)
		}
	})

	t.Run("writes the pair with the next sequence number", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Seed existing migrations, including a gap and a non-migration file
		// the sequence scan must ignore.
		for _, f := range []string{"0001_baseline.up.sql", "0001_baseline.down.sql", "0004_later.up.sql", "notes.txt"} {
			if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
				t.Fatalf("seeding %s: %v", f, err)
			}
		}
		if err := newMigration(dir, "add_widgets"); err != nil {
			t.Fatalf("newMigration: %v", err)
		}
		for _, want := range []string{"0005_add_widgets.up.sql", "0005_add_widgets.down.sql"} {
			if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
				t.Errorf("expected %s: %v", want, err)
			}
		}
	})

	t.Run("starts at 0001 in an empty directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := newMigration(dir, "first"); err != nil {
			t.Fatalf("newMigration: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "0001_first.up.sql")); err != nil {
			t.Errorf("expected 0001_first.up.sql: %v", err)
		}
	})

	t.Run("missing directory errors", func(t *testing.T) {
		t.Parallel()
		if err := newMigration(filepath.Join(t.TempDir(), "nope"), "x_y"); err == nil {
			t.Error("newMigration on a missing directory succeeded")
		}
	})
}
