//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// syncBuffer is a concurrency-safe log sink: run's logger writes from the
// consumer and health goroutines while the test reads.
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

// waitContains polls the log sink until want appears, failing on timeout.
func waitContains(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log never contained %q; logs so far:\n%s", want, logs.String())
}

// TestWorkerBootstrapFailureExits pins the shutdown ordering fixed in the
// post-M4 audit: when consumer.Run fails at group bootstrap (here: the
// stream key already exists with the wrong type, so XGROUP CREATE gets
// WRONGTYPE), run must cancel the dispatch/reconcile/health loops and
// return the error — not deadlock in wg.Wait holding the pools open.
func TestWorkerBootstrapFailureExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := queuetest.New(t)
	if err := h.Client().Set(ctx, h.Queue().Stream(), "not-a-stream", 0).Err(); err != nil {
		t.Fatalf("planting wrong-type stream key: %v", err)
	}

	env := map[string]string{
		config.EnvQueueStream: h.Queue().Stream(),
	}
	if v, ok := os.LookupEnv(storetest.EnvTestPostgresDSN); ok && v != "" {
		env[config.EnvPostgresDSN] = v
	}
	if v, ok := os.LookupEnv(queuetest.EnvTestRedisAddr); ok && v != "" {
		env[config.EnvRedisAddr] = v
	}
	lookup := func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}

	var logs syncBuffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, lookup, &logs) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run returned nil on group bootstrap failure, want error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run hung on group bootstrap failure instead of returning the error")
	}
}

// TestWorkerStartStop drives a full worker lifecycle in-process against
// the compose stack: boot, health logging, graceful shutdown on context
// cancellation — the automatable core of 4.2's "starts/stops cleanly
// under compose with health logging".
func TestWorkerStartStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env := map[string]string{
		// Fast ticks so the test observes a health line and shuts down
		// promptly (shutdown latency is bounded by the consumer block).
		config.EnvWorkerHealthInterval: "100ms",
		config.EnvQueueConsumerBlock:   "200ms",
	}
	// Honor the harness overrides so CI's worker talks to the same stack
	// the other integration tests use.
	if v, ok := os.LookupEnv(storetest.EnvTestPostgresDSN); ok && v != "" {
		env[config.EnvPostgresDSN] = v
	}
	if v, ok := os.LookupEnv(queuetest.EnvTestRedisAddr); ok && v != "" {
		env[config.EnvRedisAddr] = v
	}
	lookup := func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}

	var logs syncBuffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, lookup, &logs) }()

	waitContains(t, &logs, "worker started")
	waitContains(t, &logs, "queue consumer started")
	waitContains(t, &logs, "worker health")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error on graceful shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
	for _, want := range []string{"queue consumer stopped", "worker stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("shutdown logs missing %q; logs:\n%s", want, logs.String())
		}
	}
}
