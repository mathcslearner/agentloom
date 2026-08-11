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
// graceful shutdown that drains the in-flight handler.
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
	"github.com/mathcslearner/agentloom/internal/obs/log"
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

	q := queue.New(client, "", "") // ADR-005 default stream + group
	workerID := queue.NewConsumerName()
	dispatcher, err := engine.NewDispatcher(st, q, engine.DispatcherConfig{
		Interval: cfg.Worker.DispatchInterval,
		Batch:    cfg.Worker.DispatchBatch,
	})
	if err != nil {
		return err
	}
	reconciler, err := engine.NewReconciler(st, engine.ReconcilerConfig{
		Interval:     cfg.Worker.ReconcileInterval,
		ReadyStale:   cfg.Worker.ReconcileReadyStale,
		RunningStale: cfg.Worker.ReconcileRunningStale,
		Limit:        cfg.Worker.ReconcileLimit,
	}, engine.WithReconcilerNudge(dispatcher.Nudge))
	if err != nil {
		return err
	}
	eng, err := engine.New(st, exec.Builtins(), workerID,
		engine.WithDispatchNudge(dispatcher.Nudge))
	if err != nil {
		return err
	}
	consumer := q.NewConsumer(workerID, eng.Handle, consumerConfig(cfg.Queue))

	logger = logger.With(log.WorkerID(workerID))
	ctx = log.Into(ctx, logger)
	logger.InfoContext(ctx, "worker started",
		slog.String("version", version.Version),
		slog.String("stream", q.Stream()),
		slog.String("group", q.Group()),
		slog.Duration("health_interval", cfg.Worker.HealthInterval),
		slog.Duration("dispatch_interval", cfg.Worker.DispatchInterval),
		slog.Duration("reconcile_interval", cfg.Worker.ReconcileInterval))
	defer logger.InfoContext(ctx, "worker stopped")

	// The dispatch duties stop on the same ctx as the consumer; wait for
	// them before the deferred store/client closes tear down their
	// backends (an in-flight drain transaction simply rolls back).
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(2)
	go func() { defer wg.Done(); dispatcher.Run(ctx) }()
	go func() { defer wg.Done(); reconciler.Run(ctx) }()
	go healthLoop(ctx, q, cfg.Worker.HealthInterval)

	// Blocks until ctx is canceled, then drains the in-flight handler
	// (3.3's contract). The only error it can return is group bootstrap.
	return consumer.Run(ctx)
}

// consumerConfig maps the deployable's queue tuning onto the consumer's.
// The mapping lives here because config must not import queue (queue logs
// through obs/log, which imports config). PoisonHandler stays nil on
// purpose: 3.4's default logs and leaves poison entries pending — visible
// spin beats a silent drop — until M5.4 wires dead-lettering.
func consumerConfig(cfg config.QueueConfig) queue.ConsumerConfig {
	return queue.ConsumerConfig{
		Batch:                cfg.ConsumerBatch,
		Block:                cfg.ConsumerBlock,
		LeaseTTL:             cfg.LeaseTTL,
		HeartbeatInterval:    cfg.HeartbeatInterval,
		ReclaimInterval:      cfg.ReclaimInterval,
		PoisonThreshold:      cfg.PoisonThreshold,
		JanitorInterval:      cfg.JanitorInterval,
		JanitorIdleThreshold: cfg.JanitorIdleThreshold,
		TrimInterval:         cfg.TrimInterval,
		PromoterTick:         cfg.PromoterTick,
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
