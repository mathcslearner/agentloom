// Command worker is agentloom's step-execution deployable (ADR-001,
// ticket 4.2): one member of the worker fleet. It consumes step-ready
// deliveries from the Redis stream, attempts the Postgres claim CAS for
// each (internal/engine), and runs the claimed step's executor. The
// consumer loop also carries the fleet's shared duties from M3: lease
// heartbeats, expired-lease reclaim, delayed-delivery promotion, orphan
// cleanup, and stream trimming. Ticket 4.4 adds the dispatch duties every
// worker runs (ADR-002 — no central scheduler): the outbox drain loop and
// the periodic reconciler.
//
// Configuration comes entirely from AGENTLOOM_* environment variables
// (internal/config); there are no flags. SIGINT/SIGTERM trigger a
// graceful shutdown (ticket 5.7): the consumer stops claiming but
// finishes the entries it already holds — heartbeating until done, acking
// after each completion commits — while the dispatch loops keep draining
// the outbox; after AGENTLOOM_WORKER_DRAIN_TIMEOUT the remainder is
// abandoned un-acked, and its leases expire naturally into reclaim by a
// surviving worker.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/cache/redisstore"
	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/steplog"
	"github.com/mathcslearner/agentloom/internal/limits"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/obs/trace"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
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
	if err := run(ctx, os.LookupEnv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run wires and runs one worker until ctx is canceled. Parameterized on
// the environment and the log sink so the integration test can drive a
// full start/stop cycle in-process.
func run(ctx context.Context, lookup config.LookupFunc, logSink io.Writer) error {
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

	client, err := queue.Open(ctx, cfg.Redis.Addr)
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck // best-effort cleanup at shutdown

	// Empty names fall back to ADR-005's fleet-wide defaults; the env
	// overrides exist for test isolation (the 4.7 crash suite runs real
	// worker processes against per-test keys).
	q := queue.New(client, cfg.Queue.Stream, cfg.Queue.Group)
	workerID := queue.NewConsumerName()

	// Telemetry (ticket 7.1, ADR-008). The OTel pipeline installs a no-op
	// provider when disabled (no spans until 7.3 either way — installing
	// the provider here proves the on/off seam); the admin listener serving
	// /metrics and /healthz only exists when an addr is configured. Both
	// default off so tests and the crash suite's subprocess workers bind
	// no ports and dial no collector.
	traceShutdown, err := trace.Setup(ctx, cfg.Obs, metrics.ServiceWorker, workerID, logger)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := traceShutdown(flushCtx); err != nil {
			logger.Warn("worker: otel shutdown incomplete", slog.Any("error", err))
		}
	}()
	promRegistry := metrics.NewRegistry(metrics.ServiceWorker)
	// The engine instruments (ticket 7.2) always exist — recording on an
	// unexposed registry is cheap — while the depth-gauge sampler below
	// only runs when the admin listener is configured.
	engineMetrics := metrics.NewWorkerMetrics(promRegistry)
	var admin *metrics.Server
	if cfg.Obs.MetricsAddr != "" {
		admin, err = metrics.Listen(cfg.Obs.MetricsAddr, promRegistry)
		if err != nil {
			return fmt.Errorf("obs: binding admin listener on %s: %w", cfg.Obs.MetricsAddr, err)
		}
	}

	dispatcher, err := engine.NewDispatcher(st, q, engine.DispatcherConfig{
		Interval: cfg.Worker.DispatchInterval,
		Batch:    cfg.Worker.DispatchBatch,
	}, engine.WithDispatcherMetrics(engineMetrics))
	if err != nil {
		return err
	}
	reconciler, err := engine.NewReconciler(st, engine.ReconcilerConfig{
		Interval:     cfg.Worker.ReconcileInterval,
		ReadyStale:   cfg.Worker.ReconcileReadyStale,
		RunningStale: cfg.Worker.ReconcileRunningStale,
		RetryStale:   cfg.Worker.ReconcileRetryStale,
		Limit:        cfg.Worker.ReconcileLimit,
	}, engine.WithReconcilerNudge(dispatcher.Nudge), engine.WithReconcilerMetrics(engineMetrics))
	if err != nil {
		return err
	}
	// Model providers (tickets 8.4/8.5/8.6, ADR-009): the llm executor
	// routes step models to whichever providers this worker configured —
	// each built iff its key is present (or, for the mock, enabled), an
	// empty registry valid so a worker running no llm steps boots keyless.
	providerKeys := llm.ProviderKeys{
		Anthropic: cfg.LLM.AnthropicAPIKey,
		OpenAI:    cfg.LLM.OpenAIAPIKey,
	}
	if cfg.LLM.MockEnabled {
		providerKeys.Mock = &llm.MockConfig{}
	}
	providers, err := llm.NewRegistryFromKeys(providerKeys)
	if err != nil {
		return fmt.Errorf("configuring model providers: %w", err)
	}
	// Built-in tools (ticket 8.7, ADR-009): the tool executor invokes them
	// by name. http_request's allowlist (empty = deny all) and limits come
	// from config; json_transform is config-free.
	toolReg, err := tools.NewBuiltins(tools.HTTPOptions{
		Allowlist:        cfg.Tools.HTTPAllowlist,
		DefaultTimeout:   cfg.Tools.HTTPTimeout,
		MaxResponseBytes: cfg.Tools.HTTPMaxResponseBytes,
	})
	if err != nil {
		return fmt.Errorf("configuring tools: %w", err)
	}
	// Reference retriever (ticket 8.8, ADR-009): the retrieve executor
	// queries it by name. pg_fulltext runs over the same Postgres the
	// engine already depends on — zero new infrastructure, so it is always
	// wired (no key, no toggle); pgvector/external stores are backlog
	// plugins registered here the same way.
	retrievers, err := retrieval.NewRegistry(pgfts.New(st))
	if err != nil {
		return fmt.Errorf("configuring retrievers: %w", err)
	}
	// Fleet-wide resource limits (ticket 9.1, ADR-010): the named external
	// resources whose request/token throughput the M9 limiter middleware
	// governs. Parsed and validated here so a bad limit config fails boot,
	// not the first throttled step. An empty set (neither env source given)
	// leaves every resource unlimited. The 9.2 middleware consumes this set;
	// for now the worker only loads and reports it.
	resourceLimits, err := limits.Load(cfg.Resources.Inline, cfg.Resources.File)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "resource limits loaded",
		slog.Int("resources", resourceLimits.Len()),
		slog.Any("names", resourceLimits.Names()))
	// Versioned pricing catalog (ticket 10.1, ADR-012): the embedded default
	// $/1M-token catalog merged with an optional operator override, resolved
	// to the per-model rates M10's cost ledger will price attempts against.
	// Loaded and validated here so a malformed override fails boot, not the
	// first priced attempt. No runtime consumer yet — the 10.2 ledger hands it
	// to the engine; cmd/api never prices (it reads ledger rows from
	// Postgres). The unknown-model policy string is validated by config; 10.3
	// maps it onto cost's typed enum at the claim-time budget check.
	pricing, err := cost.Load(cfg.Cost.Inline, cfg.Cost.File)
	if err != nil {
		return fmt.Errorf("loading pricing catalog: %w", err)
	}
	// Map the validated unknown-model policy string onto cost's typed enum
	// for the claim-time budget check (ticket 10.3): fail-closed blocks an
	// unpriced model before any money is spent; estimate (the default) prices
	// it at the fallback and proceeds. Config validated the string at load.
	unknownModelPolicy, err := cost.ParseUnknownModelPolicy(cfg.Cost.UnknownModelPolicy)
	if err != nil {
		return fmt.Errorf("mapping unknown-model policy: %w", err)
	}
	logger.InfoContext(ctx, "pricing catalog loaded",
		slog.Int("models", pricing.ModelCount()),
		slog.Int("tools", pricing.ToolCount()),
		slog.String("override_source", pricingOverrideSource(cfg.Cost)),
		slog.String("unknown_model_policy", cfg.Cost.UnknownModelPolicy))
	// The fleet-wide rate limiter (ticket 9.2, ADR-010): built over the same
	// Redis client the queue uses (the shared coordination Redis, ADR-002 —
	// Postgres stays the API's only hard dependency, but the worker already
	// depends on Redis for the queue). Wired into the engine only when the
	// config names any resource; an empty set means every resource is
	// unlimited, so the limiter would never do anything — skip it and the
	// per-step middleware short-circuits on a nil limiter.
	var resourceLimiter *resource.Limiter
	if resourceLimits.Len() > 0 {
		resourceLimiter, err = resource.New(client, resourceLimits, cfg.Resources.KeyPrefix)
		if err != nil {
			return fmt.Errorf("configuring resource limiter: %w", err)
		}
		logger.InfoContext(ctx, "resource rate limiter enabled",
			slog.String("key_prefix", cfg.Resources.KeyPrefix),
			slog.Duration("throttle_floor", cfg.Resources.ThrottleFloor),
			slog.Duration("throttle_cap", cfg.Resources.ThrottleCap),
			slog.Float64("throttle_jitter_frac", cfg.Resources.ThrottleJitterFrac))
	}
	// Production workers register the core set; the filesystem-writing
	// test executors (counter, effectful_echo) are opt-in via
	// AGENTLOOM_WORKER_TEST_EXECUTORS (ticket 6.2) — the compose dev
	// stack and the crash/chaos suites set it.
	registry := exec.CoreBuiltins(providers, toolReg, retrievers)
	if cfg.Worker.TestExecutors {
		registry = exec.Builtins(providers, toolReg, retrievers)
		// Crash-injection seam (ticket 13.5): arm the expansion crash matrix's
		// kill-at-boundary points from AGENTLOOM_WORKER_CRASH_POINT. Gated to
		// test-executor mode alongside the filesystem-writing test executors, so
		// a real deployment never installs it — the seam is inert without the
		// env anyway, and this gate keeps it doubly out of production paths.
		engine.InstallCrashPointFromEnv(os.Getenv)
	}
	logger.InfoContext(ctx, "plugin registry built",
		slog.Int("plugins", len(registry.Manifests())),
		slog.Int("providers", len(providers.Names())),
		slog.Int("tools", len(toolReg.Names())),
		slog.Int("retrievers", len(retrievers.Names())),
		slog.Bool("test_executors", cfg.Worker.TestExecutors))
	// Output validators (ticket 11.1, ADR-013): the validator-kind plugins
	// the engine's validate stage resolves a step's chain against — the five
	// deterministic built-ins (11.2) plus the cost-bearing llm_judge (11.5),
	// which routes its judge model through the same provider registry the llm
	// executor uses.
	validators, err := validate.NewBuiltins(providers)
	if err != nil {
		return fmt.Errorf("building validator registry: %w", err)
	}
	// Per-step log capture (ticket 7.4): the sink tees every executor's
	// StepContext.Logger into the step_logs store; its flusher runs on
	// loopCtx below so lines from steps finishing during the consumer's
	// drain still land, with one final bounded flush at shutdown.
	engineOpts := []engine.Option{
		engine.WithDispatchNudge(dispatcher.Nudge),
		engine.WithRetryScheduler(q.NewDelayed(cfg.Queue.DelayedKey)),
		engine.WithStrictEffects(cfg.Worker.EffectsStrict),
		engine.WithCancelPollInterval(cfg.Worker.CancelPollInterval),
		engine.WithMetrics(engineMetrics),
		// The cost ledger (ticket 10.2, ADR-012): meters cost-bearing
		// attempts against the boot-loaded catalog, writing a cost_ledger
		// row + run-aggregate bump in each success completion transaction.
		engine.WithPricing(pricing),
		// Claim-time budget enforcement (ticket 10.3, ADR-012): the same
		// catalog prices each cost-bearing step's pre-flight estimate; the
		// unknown-model policy governs the fail-closed pre-flight gate.
		engine.WithUnknownModelPolicy(unknownModelPolicy),
		// Output validation (ticket 11.1, ADR-013): the validate stage
		// resolves and runs a step's chain against this registry.
		engine.WithValidators(validators),
	}
	// Run-scoped blackboard (ticket 12.2, ADR-014): a step-scoped handle is
	// bound onto each StepContext (programmatic reads/writes) and a step's
	// declarative `blackboard` writes are applied in the completion
	// transaction. Over the shared store and the engine's clock.
	board, err := pgboard.New(st, pgboard.WithClock(time.Now))
	if err != nil {
		return fmt.Errorf("building blackboard: %w", err)
	}
	engineOpts = append(engineOpts, engine.WithBlackboard(board))
	// Context assembly (ticket 12.3, ADR-014): the engine resolves a
	// `retrieval` context source against the same retriever registry the
	// retrieve executor uses, so a context spec and a `retrieve` step share
	// backends. Blackboard and step-output sources need no extra wiring
	// (the board above and the store).
	engineOpts = append(engineOpts, engine.WithRetrievers(retrievers))
	// Summarization compaction (ticket 12.5, ADR-014): the summarize
	// compaction strategy summarizes an evicted span with a cheap model routed
	// through the same provider registry the llm executor uses. When a response
	// cache is wired (below), the engine wraps this in a caching decorator so a
	// repeated compaction of the same span is a cache hit, not a second billed
	// call.
	engineOpts = append(engineOpts, engine.WithSummarizer(contextmgr.NewLLMSummarizer(providers)))
	if resourceLimiter != nil {
		engineOpts = append(engineOpts,
			engine.WithResourceLimiter(resourceLimiter),
			engine.WithThrottleBackoff(cfg.Resources.ThrottleFloor, cfg.Resources.ThrottleCap, cfg.Resources.ThrottleJitterFrac))
	}
	// Response cache (ticket 9.5, ADR-011): the read-through/write-through
	// middleware ahead of the rate limiter, over the same coordination Redis
	// the queue uses (the API never reads the cache — ADR-002 untouched).
	// Enabled by default; a hit skips the limiter and the provider entirely.
	if cfg.Cache.Enabled {
		cacheStore, cerr := redisstore.New(client, cfg.Cache.KeyPrefix, cfg.Cache.MaxValueBytes)
		if cerr != nil {
			return fmt.Errorf("configuring response cache: %w", cerr)
		}
		engineOpts = append(engineOpts, engine.WithResponseCache(cacheStore, cfg.Cache.DefaultTTL))
		logger.InfoContext(ctx, "response cache enabled",
			slog.String("key_prefix", cfg.Cache.KeyPrefix),
			slog.Duration("default_ttl", cfg.Cache.DefaultTTL),
			slog.Int64("max_value_bytes", cfg.Cache.MaxValueBytes))
	}
	var logSinkStep *steplog.Sink
	if cfg.Worker.StepLogEnabled {
		logSinkStep = steplog.New(st, steplog.Config{
			Level:         cfg.Worker.StepLogLevel,
			Cap:           cfg.Worker.StepLogCap,
			Buffer:        cfg.Worker.StepLogBuffer,
			MaxLineBytes:  cfg.Worker.StepLogMaxLineBytes,
			FlushInterval: cfg.Worker.StepLogFlushInterval,
			FlushBatch:    cfg.Worker.StepLogFlushBatch,
			Metrics:       engineMetrics,
		}, logger)
		engineOpts = append(engineOpts, engine.WithStepLogs(logSinkStep))
	}
	// The engine's own delayed handle shares the consumer's key: retry
	// re-dispatches scheduled here are promoted by any worker's promoter
	// duty (ticket 5.2).
	eng, err := engine.New(st, registry, workerID, engineOpts...)
	if err != nil {
		return err
	}
	consumer := q.NewConsumer(workerID, eng.Handle, consumerConfig(cfg.Queue, cfg.Worker.DrainTimeout, eng.HandlePoison, engineMetrics))

	logger = logger.With(log.WorkerID(workerID))
	ctx = log.Into(ctx, logger)
	logger.InfoContext(ctx, "worker started",
		slog.String("version", version.Version),
		slog.String("stream", q.Stream()),
		slog.String("group", q.Group()),
		slog.Duration("health_interval", cfg.Worker.HealthInterval),
		slog.Duration("dispatch_interval", cfg.Worker.DispatchInterval),
		slog.Duration("reconcile_interval", cfg.Worker.ReconcileInterval),
		slog.Duration("drain_timeout", cfg.Worker.DrainTimeout),
		slog.Bool("test_executors", cfg.Worker.TestExecutors),
		slog.Bool("steplog_enabled", cfg.Worker.StepLogEnabled),
		slog.String("metrics_addr", cfg.Obs.MetricsAddr),
		slog.Bool("otel_enabled", cfg.Obs.OTelEnabled))
	defer logger.InfoContext(ctx, "worker stopped")

	// The dispatch duties run on loopCtx, which deliberately OUTLIVES the
	// signal context (ticket 5.7): a SIGTERM must stop the consumer's
	// claiming, not the outbox drain — the steps finishing during the
	// consumer's drain fan successors out through the outbox, and this
	// worker's own dispatcher is what hands them to a survivor promptly.
	// The loops stop when run returns (consumer drained, or group
	// bootstrap failed), via the deferred cancel; wg.Wait is deferred
	// AFTER it so LIFO runs the cancel first, and both run before the
	// deferred store/client closes tear down the backends (an in-flight
	// drain transaction simply rolls back).
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Add(4)
	go func() { defer wg.Done(); dispatcher.Run(loopCtx) }()
	go func() { defer wg.Done(); reconciler.Run(loopCtx) }()
	go func() { defer wg.Done(); healthLoop(loopCtx, q, cfg.Worker.HealthInterval) }()
	if logSinkStep != nil {
		// Rides loopCtx like the dispatch duties: keeps flushing through the
		// consumer's drain, final bounded flush when run returns. The
		// deferred wg.Wait runs before the store closes, so that flush still
		// has its backend.
		wg.Add(1)
		go func() { defer wg.Done(); logSinkStep.Run(loopCtx) }()
	}
	if admin != nil {
		// The admin server rides loopCtx like the dispatch duties: /metrics
		// stays scrapeable through the consumer's drain and stops when run
		// returns. The depth-gauge sampler (ticket 7.2) rides along — it
		// only makes sense while something can scrape the gauges.
		wg.Add(2)
		go func() { defer wg.Done(); admin.Serve(loopCtx, logger) }()
		go func() {
			defer wg.Done()
			sampleLoop(loopCtx, q, q.NewDelayed(cfg.Queue.DelayedKey), st, engineMetrics, cfg.Worker.MetricsSampleInterval, activeIdleMax(cfg.Queue))
		}()
		logger.InfoContext(ctx, "worker admin listener started",
			slog.String("addr", admin.Addr()),
			slog.Duration("metrics_sample_interval", cfg.Worker.MetricsSampleInterval))
	}
	go func() { // narrate the shutdown sequence for the ops log
		defer wg.Done()
		select {
		case <-ctx.Done():
			logger.InfoContext(ctx, "worker shutdown: signal received; consumer draining",
				slog.Duration("drain_timeout", cfg.Worker.DrainTimeout))
		case <-loopCtx.Done():
		}
	}()

	// Blocks until ctx is canceled, then drains per ConsumerConfig's
	// DrainTimeout (ticket 5.7). The only error it can return is group
	// bootstrap.
	err = consumer.Run(ctx)
	if ctx.Err() != nil {
		logger.InfoContext(ctx, "worker shutdown: consumer drained; stopping dispatch loops")
	}
	return err
}

// consumerConfig maps the deployable's queue tuning onto the consumer's.
// The mapping lives here because config must not import queue (queue logs
// through obs/log, which imports config). The poison handler is the
// engine's dead-lettering path (ticket 5.4, ADR-006): an over-threshold
// entry lands its step in the DLQ and is acked — the durable row is the
// consumption of the message. The drain timeout comes from WorkerConfig —
// a deployable-lifecycle knob, not queue tuning (ticket 5.7) — and is
// always positive here: the production worker always drains.
func consumerConfig(cfg config.QueueConfig, drainTimeout time.Duration, poison queue.PoisonHandler, m queue.ConsumerMetrics) queue.ConsumerConfig {
	return queue.ConsumerConfig{
		DelayedKey:           cfg.DelayedKey,
		Batch:                cfg.ConsumerBatch,
		Block:                cfg.ConsumerBlock,
		LeaseTTL:             cfg.LeaseTTL,
		HeartbeatInterval:    cfg.HeartbeatInterval,
		ReclaimInterval:      cfg.ReclaimInterval,
		PoisonThreshold:      cfg.PoisonThreshold,
		PoisonHandler:        poison,
		JanitorInterval:      cfg.JanitorInterval,
		JanitorIdleThreshold: cfg.JanitorIdleThreshold,
		TrimInterval:         cfg.TrimInterval,
		PromoterTick:         cfg.PromoterTick,
		DrainTimeout:         drainTimeout,
		Metrics:              m,
	}
}

// activeIdleMax derives the active-worker idle threshold from the
// consumer's read cadence: a live consumer re-issues its blocking
// XREADGROUP every Block, so three missed cycles separates live members
// from dead ones the janitor has not yet collected.
func activeIdleMax(cfg config.QueueConfig) time.Duration {
	block := cfg.ConsumerBlock
	if block <= 0 {
		block = queue.DefaultConsumerBlock
	}
	return 3 * block
}

// pricingOverrideSource reports how the pricing catalog was overridden, for
// the boot log: an inline document, a file path, or the embedded defaults.
func pricingOverrideSource(cfg config.CostConfig) string {
	switch {
	case cfg.Inline != "":
		return "inline"
	case cfg.File != "":
		return cfg.File
	default:
		return "defaults"
	}
}

// sampleLoop periodically samples the depth gauges (ticket 7.2): queue
// ready depth / stream length / PEL size from Stats, the delayed-set
// cardinality, the outbox backlog + oldest-row age from Postgres, and the
// active-consumer count. Each source samples independently — one backend
// being down must not blank the others' gauges — and failures are logged
// and retried next tick, like healthLoop. Runs only when the admin
// metrics listener is configured.
func sampleLoop(ctx context.Context, q *queue.Queue, delayed *queue.Delayed, st *store.Store, m *metrics.WorkerMetrics, interval, activeIdleMax time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		logger := log.From(ctx)
		if stats, err := q.Stats(ctx); err != nil {
			logSampleErr(ctx, logger, "queue stats", err)
		} else if delayedLen, derr := delayed.Len(ctx); derr != nil {
			logSampleErr(ctx, logger, "delayed length", derr)
		} else {
			m.SetQueueDepths(stats.ReadyDepth(), stats.Length, stats.Pending, delayedLen)
		}
		if active, err := q.ActiveConsumers(ctx, activeIdleMax); err != nil {
			logSampleErr(ctx, logger, "active consumers", err)
		} else {
			m.SetActiveWorkers(active)
		}
		if ob, err := st.Outbox().Stats(ctx); err != nil {
			logSampleErr(ctx, logger, "outbox stats", err)
		} else {
			var oldest time.Duration
			if ob.OldestCreatedAt != nil {
				oldest = max(time.Since(*ob.OldestCreatedAt), 0)
			}
			m.SetOutbox(ob.Backlog, oldest)
		}
	}
}

// logSampleErr logs one failed gauge sample unless the loop is shutting
// down (a canceled context makes every backend call fail noisily).
func logSampleErr(ctx context.Context, logger *slog.Logger, what string, err error) {
	if ctx.Err() == nil {
		logger.WarnContext(ctx, "metrics sample failed; gauges keep their last value",
			slog.String("sample", what), slog.Any("error", err))
	}
}

// healthLoop periodically logs a liveness line with queue depth and PEL
// size — the health signal `docker compose logs` and the 4.2 acceptance
// test look for. Failures are logged and retried next tick: health
// reporting must never kill a worker that is otherwise fine.
func healthLoop(ctx context.Context, q *queue.Queue, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			stats, err := q.Stats(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.From(ctx).WarnContext(ctx, "worker health: queue stats unavailable",
						slog.Any("error", err))
				}
				continue
			}
			log.From(ctx).InfoContext(ctx, "worker health",
				slog.Int64("stream_length", stats.Length),
				slog.Int64("pending", stats.Pending))
		}
	}
}
