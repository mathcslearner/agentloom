package queue

import (
	"context"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// janitorTick deletes orphan consumer records: per-incarnation names mean
// every worker restart strands one, and XINFO CONSUMERS output and group
// memory would otherwise grow forever. Only consumers with zero pending
// entries idle beyond JanitorIdleThreshold are deleted — the zero-pending
// guard makes deletion safe by construction (XGROUP DELCONSUMER drops PEL
// state, and a merely quiet consumer still heartbeats its entries, keeping
// them visibly pending). Janitor races between workers are benign:
// deleting an absent consumer is a no-op.
func (c *Consumer) janitorTick(ctx context.Context) {
	consumers, err := c.queue.client.XInfoConsumers(ctx, c.queue.stream, c.queue.group).Result()
	if err != nil {
		if ctx.Err() == nil {
			log.From(ctx).ErrorContext(ctx, "janitor: XINFO CONSUMERS failed",
				slog.Any("error", err))
		}
		return
	}
	for _, con := range consumers {
		if con.Name == c.name || con.Pending != 0 || con.Idle < c.cfg.JanitorIdleThreshold {
			continue
		}
		if err := c.queue.client.XGroupDelConsumer(ctx, c.queue.stream, c.queue.group, con.Name).Err(); err != nil {
			if ctx.Err() == nil {
				log.From(ctx).WarnContext(ctx, "janitor: XGROUP DELCONSUMER failed",
					slog.String("orphan", con.Name),
					slog.Any("error", err))
			}
			continue
		}
		log.From(ctx).InfoContext(ctx, "janitor deleted orphan consumer",
			slog.String("orphan", con.Name),
			slog.Duration("idle", con.Idle))
	}
}
