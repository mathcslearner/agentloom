package queue

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// startHeartbeat launches the heartbeater goroutine for one in-flight
// entry: every HeartbeatInterval (±20% jitter per beat) it XCLAIMs the
// entry to this consumer with JUSTID, resetting the PEL idle time so the
// lease never expires while the handler runs. JUSTID is load-bearing per
// ADR-005: a plain XCLAIM would increment the delivery counter, and the
// counter must stay a pure redelivery signal — a long step heartbeating
// for an hour must not look like a poison message.
//
// The returned stop function signals the goroutine and waits for it to
// exit, so no beat can race the subsequent ack.
func (c *Consumer) startHeartbeat(ctx context.Context, entryID string) (stop func()) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			timer := time.NewTimer(heartbeatJitter(c.cfg.HeartbeatInterval))
			select {
			case <-stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
			c.heartbeat(ctx, entryID)
		}
	}()
	return func() {
		close(stopCh)
		<-done
	}
}

// heartbeat issues one XCLAIM JUSTID to self. It runs on a detached
// context so a handler draining through shutdown keeps its lease. Failure
// is logged, never fatal: per ADR-005 (crash cell R1b), a worker whose
// heartbeat fails — Redis down, entry reclaimed, or PEL lost — logs and
// keeps executing, because correctness rests on the Postgres claim_id
// fence, not on the lease.
func (c *Consumer) heartbeat(ctx context.Context, entryID string) {
	hbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedOpTimeout)
	defer cancel()
	claimed, err := c.queue.client.XClaimJustID(hbCtx, &redis.XClaimArgs{
		Stream:   c.queue.stream,
		Group:    c.queue.group,
		Consumer: c.name,
		MinIdle:  0,
		Messages: []string{entryID},
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.From(ctx).WarnContext(ctx, "lease heartbeat failed; execution continues (claim_id fences)",
			slog.Any("error", err))
		return
	}
	if len(claimed) == 0 {
		log.From(ctx).WarnContext(ctx, "lease heartbeat found no PEL entry; execution continues (claim_id fences)")
	}
}

// heartbeatJitter perturbs the base interval by ±20% so a fleet of
// heartbeaters never aligns (ADR-005 tuning table).
func heartbeatJitter(interval time.Duration) time.Duration {
	return time.Duration(float64(interval) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // jitter, not cryptography
}
