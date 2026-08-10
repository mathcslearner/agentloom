package queue

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// Process exposes the unexported per-message path to tests: the
// kill-pre-ACK redelivery test replays a reclaimed entry through it,
// exactly as ticket 3.4's reclaimer will in production code.
func (c *Consumer) Process(ctx context.Context, msg redis.XMessage, deliveryCount int64) bool {
	return c.process(ctx, msg, deliveryCount)
}

// SafeHandle exposes the panic-containment wrapper for unit tests.
func SafeHandle(ctx context.Context, h Handler, d Delivery) error {
	return safeHandle(ctx, h, d)
}

// WithDefaults exposes ConsumerConfig defaulting for unit tests.
func (c ConsumerConfig) WithDefaults() ConsumerConfig { return c.withDefaults() }
