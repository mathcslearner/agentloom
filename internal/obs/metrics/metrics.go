// Package metrics builds the Prometheus registries and the admin HTTP
// listener for agentloom's deployables (ticket 7.1, ADR-008).
//
// Registries are instance-scoped — never the package-global default — and
// are threaded explicitly to the components that record on them, the same
// injection discipline as clocks and loggers. Every metric follows
// ADR-008's naming scheme (`engine_<subsystem>_<name>[_<unit>]`) and its
// label cardinality budget: run_id/step_id and friends are log and trace
// fields, never metric labels.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mathcslearner/agentloom/internal/version"
)

// Namespace is ADR-008's single metric namespace. Both deployables share
// it: the emitting service is a scrape label (and the `service` label on
// engine_build_info), never a second namespace.
const Namespace = "engine"

// Service names, ADR-008's `service` label / OTel service.name values.
const (
	ServiceAPI    = "agentloom-api"
	ServiceWorker = "agentloom-worker"
)

// NewRegistry builds the deployable's metric registry: the standard Go
// runtime and process collectors plus the conventional
// engine_build_info{service, version} = 1 info gauge, so a scrape proves
// the pipeline end-to-end before any engine instrumentation (7.2) lands.
func NewRegistry(service string) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Subsystem: "build",
		Name:      "info",
		Help:      "Build metadata; constant 1. The value is in the labels.",
	}, []string{"service", "version"})
	reg.MustRegister(buildInfo)
	buildInfo.WithLabelValues(service, version.Version).Set(1)
	return reg
}

// Server is the admin HTTP listener serving GET /metrics (the Prometheus
// scrape endpoint for reg) and GET /healthz (a plain liveness probe).
// It is deliberately separate from the API's public port — that one is
// bearer-authed and contract-tested (ADR-007/6.6), and Prometheus does
// not present bearer keys — and it is the worker's first listener at all.
type Server struct {
	ln  net.Listener
	srv *http.Server
}

// Listen binds the admin listener on addr. Binding is split from Serve so
// a misconfigured address fails the deployable's boot (fail fast), while
// serve-time errors only log — telemetry must never take the service
// down (ADR-008). Tests pass "127.0.0.1:0" and read Addr.
func Listen(addr string, reg *prometheus.Registry) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return &Server{
		ln: ln,
		srv: &http.Server{
			Handler: mux,
			// A scrape is one small read and one bounded write; generous
			// caps so a stuck client can never pin a connection forever.
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  2 * time.Minute,
		},
	}, nil
}

// Addr reports the bound address (real port for ":0" binds).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve serves the admin endpoints until ctx is canceled, then shuts the
// listener down. Serve-time errors are logged and swallowed: an admin
// endpoint failure must never take the deployable down.
func (s *Server) Serve(ctx context.Context, logger *slog.Logger) {
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(s.ln) }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.ErrorContext(ctx, "obs: admin server failed; metrics unavailable",
				slog.String("addr", s.Addr()), slog.Any("error", err))
		}
		return
	case <-ctx.Done():
	}
	// A scrape response is milliseconds; bound the drain well under any
	// deployable's shutdown budget.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		logger.WarnContext(ctx, "obs: admin server shutdown incomplete",
			slog.String("addr", s.Addr()), slog.Any("error", err))
	}
	<-errc
}
