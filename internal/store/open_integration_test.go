//go:build integration

package store_test

import (
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// TestOpen covers the production entry point the pool-injecting tests
// bypass: parse → pool → ping wiring, and the promise that errors never
// echo the DSN (it can embed credentials).
func TestOpen(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-password"

	t.Run("invalid DSN rejected without echoing it", func(t *testing.T) {
		t.Parallel()
		_, err := store.Open(t.Context(), "postgres://user:"+secret+"@bad host:5432/db")
		if err == nil {
			t.Fatal("Open accepted an unparseable DSN")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("parse error echoes the DSN credentials: %v", err)
		}
	})

	t.Run("unreachable server fails the ping without the password", func(t *testing.T) {
		t.Parallel()
		// Parseable DSN, nothing listening on port 1. connect_timeout keeps
		// the failure fast.
		_, err := store.Open(t.Context(), "postgres://user:"+secret+"@127.0.0.1:1/db?connect_timeout=2")
		if err == nil {
			t.Fatal("Open reached a server on port 1")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("ping error echoes the password: %v", err)
		}
	})

	t.Run("round-trip", func(t *testing.T) {
		t.Parallel()
		_, dsn := storetest.NewEmptyDB(t)
		s, err := store.Open(t.Context(), dsn)
		if err != nil {
			t.Fatalf("Open on a live database: %v", err)
		}
		s.Close()
	})
}
