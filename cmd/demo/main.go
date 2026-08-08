// Command demo is a throwaway binary proving the config + structured
// logging wiring from ROADMAP ticket 0.5. It is deleted in M4 when the real
// binaries (cmd/api, cmd/worker) arrive.
//
// Try:
//
//	go run ./cmd/demo
//	AGENTLOOM_LOG_LEVEL=debug go run ./cmd/demo
//	AGENTLOOM_LOG_LEVEL=verbose AGENTLOOM_LOG_FORMAT=yaml go run ./cmd/demo   # fails fast
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/obs/log"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(1)
	}

	logger := log.New(cfg.Log, os.Stdout)
	logger.Info("demo starting",
		slog.String("component", "demo"),
		slog.String("log_level", cfg.Log.Level.String()),
		slog.String("log_format", string(cfg.Log.Format)),
	)

	// Simulate a worker stamping its identity once, then executing a step:
	// downstream code pulls the logger from the context and inherits fields.
	ctx := log.Into(context.Background(), logger)
	ctx = log.With(ctx, log.WorkerID("worker-demo-1"))
	executeStep(ctx, "run-42", "step-hello", 1)

	logger.Info("demo finished")
}

func executeStep(ctx context.Context, runID, stepID string, attempt int) {
	ctx = log.With(ctx, log.RunID(runID), log.StepID(stepID), log.Attempt(attempt))
	logger := log.From(ctx)
	logger.Info("step claimed")
	logger.Debug("step config resolved", slog.String("executor", "noop"))
	logger.Info("step completed", slog.String("status", "succeeded"))
}
