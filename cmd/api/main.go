// Command api is agentloom's ingest/inspection deployable (ADR-001,
// ticket 4.6). It serves internal/api's routes: POST /v1/runs,
// GET /v1/runs/{id}, the /v1/keys key management, and GET /healthz.
// Every /v1 route requires a scoped bearer key (tickets 6.1/6.2,
// ADR-007); only the health probe is anonymous. Durable state lives only
// in Postgres — run submission writes the transactional outbox, and the
// worker fleet dispatches from there (ADR-002). The Redis client here
// serves rate-limit token buckets alone (ticket 6.4) and fails open:
// an unreachable Redis degrades rate limiting, never the API.
//
// Configuration comes entirely from AGENTLOOM_* environment variables
// (internal/config); there are no flags. SIGINT/SIGTERM trigger a graceful
// drain bounded by AGENTLOOM_API_SHUTDOWN_TIMEOUT.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/ratelimit"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.LookupEnv, os.Stdout, nil); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run wires and serves the API until ctx is canceled. Parameterized on the
// environment and the log sink so tests can drive a full start/stop cycle
// in-process; ready (may be nil) receives the bound address once the
// listener is up, so a test binding ":0" can learn the real port.
func run(ctx context.Context, lookup config.LookupFunc, logSink io.Writer, ready chan<- string) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	logger := log.New(cfg.Log, logSink)

	st, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		return err
	}
	defer st.Close()

	// Rate limiting (ticket 6.4): a Redis client for the token buckets,
	// opened without a hard boot-time dependency — go-redis dials lazily
	// and the middleware fails open (ADR-007), so a down Redis degrades
	// rate limiting instead of preventing the API from serving. The ping
	// is advisory: it surfaces a misconfigured address in the boot logs.
	var rl api.RateLimitOptions
	if cfg.API.RateLimit.Enabled {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
		defer rdb.Close() //nolint:errcheck // best-effort close on shutdown
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			logger.WarnContext(ctx, "api: redis unreachable at boot — rate limiting fails open until it recovers",
				slog.String("addr", cfg.Redis.Addr), slog.Any("error", err))
		}
		cancel()
		rl = api.RateLimitOptions{
			Acquirer:  ratelimit.New(rdb),
			KeyPrefix: cfg.API.RateLimit.KeyPrefix,
			Submit:    api.ClassLimit(cfg.API.RateLimit.Submit),
			Read:      api.ClassLimit(cfg.API.RateLimit.Read),
			Admin:     api.ClassLimit(cfg.API.RateLimit.Admin),
			Global:    api.ClassLimit(cfg.API.RateLimit.Global),
		}
	}

	handler, err := api.New(st, time.Now, logger, cfg.API.RootKey, rl)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.API.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.API.Addr, err)
	}
	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  cfg.API.ReadTimeout,
		WriteTimeout: cfg.API.WriteTimeout,
		IdleTimeout:  cfg.API.IdleTimeout,
		BaseContext:  func(net.Listener) context.Context { return ctx },
	}

	logger.InfoContext(ctx, "api started",
		slog.String("version", version.Version),
		slog.String("addr", ln.Addr().String()))
	defer logger.InfoContext(ctx, "api stopped")
	if ready != nil {
		ready <- ln.Addr().String()
	}

	// Serve until ctx cancellation, then drain in-flight requests bounded
	// by the shutdown timeout.
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.API.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}
