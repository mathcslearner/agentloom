// Command api is agentloom's ingest/inspection deployable (ADR-001,
// ticket 4.6). It serves internal/api's routes: POST /v1/runs,
// GET /v1/runs/{id}, the /v1/keys key management, the /v1/plugins
// catalog (ticket 8.1), and GET /healthz.
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
//
// Telemetry (ticket 7.1, ADR-008): AGENTLOOM_OBS_METRICS_ADDR starts the
// admin listener serving /metrics; AGENTLOOM_OBS_OTEL_ENABLED turns on
// OTLP trace export, and the public handler is then wrapped in HTTP
// server spans. Both default off — no listener, no-op provider.
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
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/cache/redisstore"
	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/obs/trace"
	"github.com/mathcslearner/agentloom/internal/ratelimit"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/tools"
	"github.com/mathcslearner/agentloom/internal/validate"
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

	// Telemetry (ticket 7.1, ADR-008). The OTel pipeline installs a no-op
	// provider when disabled; the admin listener only exists when an addr
	// is configured. Both are off by default so tests bind no extra ports
	// and dial no collector.
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	traceShutdown, err := trace.Setup(ctx, cfg.Obs, metrics.ServiceAPI, hostname, logger)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := traceShutdown(flushCtx); err != nil {
			logger.Warn("api: otel shutdown incomplete", slog.Any("error", err))
		}
	}()
	registry := metrics.NewRegistry(metrics.ServiceAPI)
	// The request + rate-limit instruments (ticket 7.2) always exist —
	// recording on an unexposed registry is cheap; the admin listener
	// below is what makes them scrapeable.
	apiMetrics := metrics.NewAPIMetrics(registry)
	if cfg.Obs.MetricsAddr != "" {
		admin, err := metrics.Listen(cfg.Obs.MetricsAddr, registry)
		if err != nil {
			return fmt.Errorf("obs: binding admin listener on %s: %w", cfg.Obs.MetricsAddr, err)
		}
		// The admin server outlives the signal context so /metrics stays
		// scrapeable through the public server's drain; it stops via the
		// deferred cancel when run returns (LIFO: cancel before the wait).
		adminCtx, cancelAdmin := context.WithCancel(context.WithoutCancel(ctx))
		adminDone := make(chan struct{})
		defer func() { <-adminDone }()
		defer cancelAdmin()
		go func() { defer close(adminDone); admin.Serve(adminCtx, logger) }()
		logger.InfoContext(ctx, "api admin listener started", slog.String("addr", admin.Addr()))
	}

	st, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		return err
	}
	defer st.Close()

	// Redis client, opened without a hard boot-time dependency — go-redis
	// dials lazily and both consumers below fail soft, so a down Redis
	// degrades rate limiting and the cache ops surface instead of preventing
	// the API from serving (ADR-002 — Postgres stays the only hard
	// dependency). One client serves two opt-in uses: the 6.4 rate-limit
	// token buckets and the 9.6 cache ops surface. Built only when at least
	// one is enabled; the advisory ping surfaces a misconfigured address in
	// the boot logs.
	var rdb *redis.Client
	if cfg.API.RateLimit.Enabled || cfg.Cache.Enabled {
		rdb = redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
		defer rdb.Close() //nolint:errcheck // best-effort close on shutdown
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			logger.WarnContext(ctx, "api: redis unreachable at boot — rate limiting and cache ops fail soft until it recovers",
				slog.String("addr", cfg.Redis.Addr), slog.Any("error", err))
		}
		cancel()
	}

	// Rate limiting (ticket 6.4): the per-client token buckets over the
	// shared Redis client. The middleware fails open on a Redis error.
	var rl api.RateLimitOptions
	if cfg.API.RateLimit.Enabled {
		rl = api.RateLimitOptions{
			Acquirer:  ratelimit.New(rdb),
			KeyPrefix: cfg.API.RateLimit.KeyPrefix,
			Submit:    api.ClassLimit(cfg.API.RateLimit.Submit),
			Read:      api.ClassLimit(cfg.API.RateLimit.Read),
			Admin:     api.ClassLimit(cfg.API.RateLimit.Admin),
			Global:    api.ClassLimit(cfg.API.RateLimit.Global),
			// The 6.4 metrics seam gets its Prometheus implementation
			// (ticket 7.2).
			Metrics: apiMetrics,
		}
	}

	// Plugin catalog for GET /v1/plugins (ticket 8.1, ADR-009): the
	// builtin registry this binary compiles in, gated by the API-side
	// test-executor knob mirroring the worker's — set both alike so the
	// listing matches what the fleet executes. The API never executes
	// steps (ADR-002), so the llm executor's provider registry is nil
	// here — its self-described manifest is identical either way.
	pluginRegistry := exec.CoreBuiltins(nil, nil, nil)
	if cfg.API.TestExecutors {
		pluginRegistry = exec.Builtins(nil, nil, nil)
	}
	plugins := pluginRegistry.Manifests()

	// Built-in tools (ticket 8.7, ADR-009): the tool-kind plugins the tool
	// executor invokes. Their manifests are config-independent (the
	// allowlist governs execution, not the listing), so the API folds them
	// in verbatim so operators and UI forms see the tools the fleet runs.
	toolReg, err := tools.NewBuiltins(tools.HTTPOptions{})
	if err != nil {
		return fmt.Errorf("configuring tools: %w", err)
	}
	plugins = append(plugins, toolReg.Manifests()...)

	// Model providers (ticket 8.4/8.5, ADR-009): only the providers whose
	// key is configured (or, for the mock, enabled) are constructed, so
	// GET /v1/plugins lists exactly the providers a matching worker fleet
	// could route to. An absent key leaves the provider out of the
	// catalog, never a boot error.
	keys := llm.ProviderKeys{
		Anthropic: cfg.LLM.AnthropicAPIKey,
		OpenAI:    cfg.LLM.OpenAIAPIKey,
	}
	if cfg.LLM.MockEnabled {
		keys.Mock = &llm.MockConfig{}
	}
	providers, err := llm.NewRegistryFromKeys(keys)
	if err != nil {
		return fmt.Errorf("configuring model providers: %w", err)
	}
	plugins = append(plugins, providers.Manifests()...)

	// Reference retrievers (ticket 8.8, ADR-009): the retriever-kind
	// plugins the retrieve executor queries. The API never executes steps,
	// but it lists the catalog the fleet runs — pg_fulltext is always
	// present (it needs no key, only the shared Postgres). WithPlugins
	// re-sorts the combined slice into (kind, name) order.
	retrievers, err := retrieval.NewRegistry(pgfts.New(st))
	if err != nil {
		return fmt.Errorf("configuring retrievers: %w", err)
	}
	plugins = append(plugins, retrievers.Manifests()...)

	// Output validators (ticket 11.1, ADR-013): the validator-kind plugins
	// the fleet's validate stage runs. Empty in 11.1 (no built-ins ship yet
	// — 11.2 fills it); the listing shape is present so a client sees the
	// kind. WithPlugins re-sorts the combined slice into (kind, name) order.
	validators, err := validate.NewBuiltins()
	if err != nil {
		return fmt.Errorf("configuring validators: %w", err)
	}
	plugins = append(plugins, validators.Manifests()...)

	// Response-cache ops surface (ticket 9.6, ADR-011): the admin bust/stats
	// endpoints operate over a redisstore built on the shared client, the
	// same layout the worker fleet writes (cache.RedisKey prefix/namespacing,
	// value cap). Wired only when caching is enabled; otherwise the
	// /v1/cache/* routes answer 503. The API never reads a cached result —
	// this is an ops surface, ADR-002 intact.
	apiOpts := []api.Option{api.WithRequestMetrics(apiMetrics), api.WithPlugins(plugins)}
	if cfg.Cache.Enabled && rdb != nil {
		cacheStore, err := redisstore.New(rdb, cfg.Cache.KeyPrefix, cfg.Cache.MaxValueBytes)
		if err != nil {
			return fmt.Errorf("configuring cache ops: %w", err)
		}
		apiOpts = append(apiOpts, api.WithCacheOps(cacheStore))
	}

	apiHandler, err := api.New(st, time.Now, logger, cfg.API.RootKey, rl, apiOpts...)
	if err != nil {
		return err
	}
	var handler http.Handler = apiHandler
	if cfg.Obs.OTelEnabled {
		// HTTP server spans (ticket 7.1): one span per request. The
		// formatter's method-only name is a placeholder — the chi route
		// pattern isn't known this far out, and raw paths are unbounded —
		// which the api requestLog middleware renames to
		// "<METHOD> <route pattern>" once routing has resolved (ticket
		// 7.3). The POST /v1/runs span doubles as the run's root: the
		// submit handler persists its context on the run row.
		handler = otelhttp.NewHandler(handler, "http.server",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return "HTTP " + r.Method
			}))
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
		slog.String("addr", ln.Addr().String()),
		slog.String("metrics_addr", cfg.Obs.MetricsAddr),
		slog.Bool("otel_enabled", cfg.Obs.OTelEnabled),
		slog.Int("plugins", len(plugins)),
		slog.Any("providers", providers.Names()),
		slog.Bool("test_executors", cfg.API.TestExecutors))
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
