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
		// Telemetry wiring (ticket 7.1): the admin listener binds an
		// ephemeral port; its real address is read back from the boot log.
		config.EnvObsMetricsAddr: "127.0.0.1:0",
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

	// The admin listener serves the ADR-008 proof-of-life gauge (ticket
	// 7.1). Its ephemeral address comes from the boot log line.
	adminAddr := adminAddrFromLogs(t, &logs, "api admin listener started")
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
	if want := `engine_build_info{service="agentloom-api"`; !strings.Contains(string(body), want) {
		t.Errorf("scrape missing %q; body:\n%s", want, body)
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
