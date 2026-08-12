package metrics_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/version"
)

// TestNewRegistryServesBuildInfo proves the registry carries the ADR-008
// proof-of-life gauge plus the standard collectors, end to end through
// the scrape handler.
func TestNewRegistryServesBuildInfo(t *testing.T) {
	t.Parallel()

	srv, err := metrics.Listen("127.0.0.1:0", metrics.NewRegistry(metrics.ServiceWorker))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx, slog.New(slog.DiscardHandler)) }()
	defer func() { cancel(); <-done }()

	body := scrape(t, srv.Addr())
	want := fmt.Sprintf(`engine_build_info{service=%q,version=%q} 1`, metrics.ServiceWorker, version.Version)
	if !strings.Contains(body, want) {
		t.Errorf("scrape does not contain %q; body:\n%s", want, body)
	}
	for _, family := range []string{"go_goroutines", "process_open_fds"} {
		if !strings.Contains(body, family) {
			t.Errorf("scrape missing standard collector family %q", family)
		}
	}
}

// TestServerHealthzAndShutdown covers the liveness probe and the
// cancel-driven drain: after ctx cancels, Serve returns and the port
// stops answering.
func TestServerHealthzAndShutdown(t *testing.T) {
	t.Parallel()

	srv, err := metrics.Listen("127.0.0.1:0", metrics.NewRegistry(metrics.ServiceAPI))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx, slog.New(slog.DiscardHandler)) }()

	resp, err := http.Get("http://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // nothing read from the body
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of cancellation")
	}
	if _, err := http.Get("http://" + srv.Addr() + "/healthz"); err == nil {
		t.Error("admin port still answering after shutdown")
	}
}

// TestListenInvalidAddrFails pins the fail-fast contract: a bad address
// is a boot error, not a logged warning.
func TestListenInvalidAddrFails(t *testing.T) {
	t.Parallel()

	if _, err := metrics.Listen("999.999.999.999:0", metrics.NewRegistry(metrics.ServiceAPI)); err == nil {
		t.Fatal("Listen with invalid address: want error, got nil")
	}
}

func scrape(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only scrape
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading scrape body: %v", err)
	}
	return string(body)
}
