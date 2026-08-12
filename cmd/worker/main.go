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

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/steplog"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/obs/trace"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
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
	// Production workers register the core set; the filesystem-writing
	// test executors (counter, effectful_echo) are opt-in via
	// AGENTLOOM_WORKER_TEST_EXECUTORS (ticket 6.2) — the compose dev
	// stack and the crash/chaos suites set it.
	registry := exec.CoreBuiltins()
	if cfg.Worker.TestExecutors {
		registry = exec.Builtins()
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
