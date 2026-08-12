//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
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

// adminAddrFromLogs polls the log sink for the admin-listener boot line
// (msg contains marker) and extracts its addr field — how a test binding
// the admin port to ":0" learns the real port.
func adminAddrFromLogs(t *testing.T, logs *syncBuffer, marker string) string {
	t.Helper()
	re := regexp.MustCompile(`"addr":"([^"]+)"`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(logs.String(), "\n") {
			if !strings.Contains(line, marker) {
				continue
			}
			if m := re.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log never contained %q with an addr field; logs so far:\n%s", marker, logs.String())
	return ""
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
		// Telemetry wiring (ticket 7.1): the admin listener binds an
		// ephemeral port; its real address is read back from the boot log.
		config.EnvObsMetricsAddr: "127.0.0.1:0",
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

	// The admin listener serves the ADR-008 proof-of-life gauge (ticket
	// 7.1). Its ephemeral address comes from the boot log line.
	waitContains(t, &logs, "worker admin listener started")
	adminAddr := adminAddrFromLogs(t, &logs, "worker admin listener started")
	scrape, err := http.Get(fmt.Sprintf("http://%s/metrics", adminAddr))
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, err := io.ReadAll(scrape.Body)
	scrape.Body.Close() //nolint:errcheck // body already read
	if err != nil {
		t.Fatalf("reading /metrics body: %v", err)
	}
	if scrape.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics status = %d, want 200", scrape.StatusCode)
	}
	if want := `engine_build_info{service="agentloom-worker"`; !strings.Contains(string(body), want) {
		t.Errorf("scrape missing %q; body:\n%s", want, body)
	}

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
