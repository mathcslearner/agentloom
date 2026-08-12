package engine

// Step execution timeouts (ticket 5.3, ADR-006 taxonomy row 3). The
// engine — never the executor — decides that an attempt timed out, from
// context state: the executor runs under a deadline-bearing child context
// and surfaces ctx.Err(); if the deadline elapsed, the failure is classed
// `timeout` regardless of what error the executor wrapped.
//
// Enforcement is cooperative and synchronous: the executor keeps running
// on the handler goroutine, so there is no detached goroutine to leak and
// the same step can never execute twice concurrently inside one process.
// The lease heartbeater wraps the whole handler invocation (queue.Consumer),
// so a slow or hung executor keeps its lease while the watchdog waits —
// timeout ≠ reclaim: the attempt is judged `timeout` by a live worker,
// where a crash records `lost` via another worker's takeover. An executor
// that ignores cancellation (an SPI violation) stalls the consumer visibly
// — the watchdog logs at the deadline and the heartbeat keeps the entry
// pending — until ReconcileRunningStale's wall-clock cap takes the step
// over (ADR-005).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mathcslearner/agentloom/internal/exec"
)

// runExecutor invokes one executor attempt, under a deadline-bearing child
// context when timeout > 0 (zero means no timeout — the executor runs
// under the caller's context untouched). expired reports whether the
// deadline elapsed, judged from context state after the executor returns
// — the caller routes an expired failure to ClassTimeout and merely logs
// an expired success. Everything watchdog-related (the deadline timer,
// the observer goroutine) is torn down before returning, so a completed
// invocation leaves no goroutine behind. Completions must run on the
// caller's own ctx, never the expired child — which is why the child
// context does not escape.
func runExecutor(ctx context.Context, executor exec.Executor, sc exec.StepContext, timeout time.Duration) (out exec.Output, expired bool, execErr error) {
	execCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		defer watchdogLog(execCtx, sc.Logger, timeout)()
	}
	out, execErr = executor.Execute(execCtx, sc)
	return out, timedOut(execCtx), execErr
}

// stepTimeout parses the materialized per-attempt timeout off the
// run_steps row; nil or empty means no timeout. The column is written
// from a validated definition, so a parse failure is corrupt stored state
// — the caller records a permanent step failure, mirroring
// decodeRetryPolicy's corrupt-policy handling.
func stepTimeout(raw *string) (time.Duration, error) {
	if raw == nil || *raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(*raw)
	if err != nil {
		return 0, fmt.Errorf("engine: parsing materialized step timeout %q: %w", *raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("engine: materialized step timeout %q is not positive", *raw)
	}
	return d, nil
}

// timedOut reports whether the execution context's deadline elapsed — the
// engine's discriminator for assigning ClassTimeout. A parent cancellation
// (shutdown) surfaces as context.Canceled, deliberately not a timeout:
// run-control cancellation semantics are ticket 5.6's.
func timedOut(execCtx context.Context) bool {
	return errors.Is(execCtx.Err(), context.DeadlineExceeded)
}

// watchdogLog starts the timeout watchdog's observer: one goroutine that
// logs when the deadline fires while the executor is still running — the
// only sign of life an operator gets from an executor that ignores
// cancellation. The returned stop joins the goroutine, so a completed
// execution leaves nothing behind (the no-leak half of ticket 5.3's
// contract; the context timer itself is freed by the caller's cancel).
func watchdogLog(execCtx context.Context, logger *slog.Logger, timeout time.Duration) func() {
	if logger == nil {
		logger = slog.Default()
	}
	stopped := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-execCtx.Done():
			if timedOut(execCtx) {
				logger.WarnContext(execCtx, "step timeout deadline fired; awaiting executor cancellation (lease heartbeat continues)",
					slog.Duration("timeout", timeout))
			}
		case <-stopped:
		}
	}()
	return func() {
		close(stopped)
		<-finished
	}
}
