//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// syncBuffer is a concurrency-safe log sink (the server goroutines write
// while the test reads), mirroring cmd/worker's.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAPIStartStop drives a full api lifecycle in-process against the
// compose stack (post-M4 audit — 4.6 described this test; now it exists):
// boot on ":0", learn the real port through the ready channel, serve a
// live /healthz, then drain cleanly on context cancellation.
func TestAPIStartStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env := map[string]string{
		config.EnvAPIAddr: "127.0.0.1:0",
	}
	if v, ok := os.LookupEnv(storetest.EnvTestPostgresDSN); ok && v != "" {
		env[config.EnvPostgresDSN] = v
	}
	lookup := func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}

	var logs syncBuffer
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, lookup, &logs, ready) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run exited before signaling ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("run never signaled ready")
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // nothing read from the body
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error on graceful shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("api did not stop after context cancellation")
	}
	for _, want := range []string{"api started", "api stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q; logs:\n%s", want, logs.String())
		}
	}
}
