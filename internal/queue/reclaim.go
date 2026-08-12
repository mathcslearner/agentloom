package queue

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// PoisonMessage is an entry whose delivery count exceeded the poison
// threshold: it has crashed or errored its way through that many handlers.
// The raw Values are always carried so dead-lettering (M5.4) preserves the
// contents even when the envelope itself is the poison — an undecodable
// entry can never be dead-lettered from its Envelope, because it has none.
type PoisonMessage struct {
	// ID is the Redis stream entry ID.
	ID string
	// Values is the raw field–value map exactly as stored on the stream.
	Values map[string]any
	// Envelope is the decoded envelope, or nil when Values do not decode
	// (a malformed or unknown-version entry — itself a poison source, per
	// ADR-005's no-silent-drop rule).
	Envelope *Envelope
	// DeliveryCount is the delivery count that crossed the threshold.
	DeliveryCount int64
}

// PoisonHandler receives poison messages diverted from the handler path.
// Nil return acks the entry — M5.4 wires dead-lettering (step →
// dead_lettered, full context recorded) before that ack; an error leaves
// the entry pending for the next reclaim pass. A panic is contained and
// treated as an error.
type PoisonHandler func(ctx context.Context, p PoisonMessage) error

// reclaimTick runs one bounded XAUTOCLAIM pass: it takes ownership of up
// to Batch entries idle beyond LeaseTTL, starting at the stored cursor,
// and feeds each through the shared process path with its real delivery
// count — or through the poison path when the count exceeds the
// threshold. XAUTOCLAIM (without JUSTID) increments the delivery counter,
// correctly: a reclaim is a redelivery.
func (c *Consumer) reclaimTick(ctx context.Context) {
	msgs, next, err := c.queue.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   c.queue.stream,
		Group:    c.queue.group,
		Consumer: c.name,
		MinIdle:  c.cfg.LeaseTTL,
		Start:    c.reclaimCursor,
		Count:    int64(c.cfg.Batch),
	}).Result()
	if err != nil {
		if ctx.Err() == nil {
			log.From(ctx).ErrorContext(ctx, "XAUTOCLAIM failed; retrying from the same cursor next tick",
				slog.Any("error", err))
		}
		return
	}
	c.reclaimCursor = next
	if len(msgs) == 0 {
		return
	}
	counts, err := c.deliveryCounts(ctx, msgs)
	if err != nil {
		// The entries are now leased to this consumer; if we bail here
		// they redeliver — to us or anyone — after another TTL. Bounded
		// delay, no loss.
		if ctx.Err() == nil {
			log.From(ctx).ErrorContext(ctx, "delivery-count lookup after XAUTOCLAIM failed; entries redeliver after TTL",
				slog.Any("error", err))
		}
		return
	}
	log.From(ctx).InfoContext(ctx, "reclaimed expired leases",
		slog.Int("count", len(msgs)),
		slog.String("cursor", next))
	for _, msg := range msgs {
		count, ok := counts[msg.ID]
		if !ok {
			// The entry left the PEL between the claim and the count
			// lookup: its previous holder's completion acked it (XACK
			// needs no ownership). The work is done; nothing to do here.
			continue
		}
		if count > int64(c.cfg.PoisonThreshold) {
			if c.work.Err() != nil {
				// Shutdown past the drain deadline: leave the poison entry
				// pending — the next reclaimer diverts it.
				log.From(ctx).WarnContext(ctx, "shutdown: leaving reclaimed poison entry pending",
					slog.String("entry_id", msg.ID))
				continue
			}
			c.divertPoison(c.work, msg, count)
			continue
		}
		// The XAUTOCLAIM above put these entries in this consumer's PEL,
		// so a shutdown mid-pass owes them the same drain-or-abandon
		// treatment as fresh reads — deliver handles both (ticket 5.7).
		c.deliver(ctx, msg, count)
	}
}

// deliveryCounts fetches the post-reclaim delivery count for each claimed
// entry. XAUTOCLAIM does not return counts, so this is one pipelined
// per-entry XPENDING — exact regardless of what other entries this
// consumer holds, unlike a single range query, which interleaved entries
// could truncate.
func (c *Consumer) deliveryCounts(ctx context.Context, msgs []redis.XMessage) (map[string]int64, error) {
	pipe := c.queue.client.Pipeline()
	cmds := make([]*redis.XPendingExtCmd, len(msgs))
	for i, msg := range msgs {
		cmds[i] = pipe.XPendingExt(ctx, &redis.XPendingExtArgs{
			Stream: c.queue.stream,
			Group:  c.queue.group,
			Start:  msg.ID,
			End:    msg.ID,
			Count:  1,
		})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("queue: XPENDING for reclaimed entries: %w", err)
	}
	counts := make(map[string]int64, len(msgs))
	for i, cmd := range cmds {
		for _, p := range cmd.Val() {
			if p.ID == msgs[i].ID {
				counts[p.ID] = p.RetryCount
			}
		}
	}
	return counts, nil
}

// divertPoison hands one over-threshold entry to the poison callback
// instead of the handler. Without a callback the entry is logged and left
// pending — it will surface on every reclaim pass until M5.4's
// dead-lettering (or an operator) resolves it, which is the designed
// behavior: visible spin, never a silent drop.
func (c *Consumer) divertPoison(ctx context.Context, msg redis.XMessage, deliveryCount int64) {
	ctx = log.With(ctx,
		slog.String("entry_id", msg.ID),
		slog.Int64("delivery_count", deliveryCount))
	p := PoisonMessage{ID: msg.ID, Values: msg.Values, DeliveryCount: deliveryCount}
	if env, err := DecodeEnvelope(msg.Values); err == nil {
		p.Envelope = &env
		ctx = log.With(ctx,
			log.RunID(env.RunID.String()),
			log.StepID(env.StepID),
			slog.String("reason", env.Reason))
	}
	logger := log.From(ctx)
	if c.cfg.PoisonHandler == nil {
		logger.ErrorContext(ctx, "poison message with no poison handler; entry stays pending")
		return
	}
	if err := safePoison(ctx, c.cfg.PoisonHandler, p); err != nil {
		logger.ErrorContext(ctx, "poison handler failed; entry stays pending",
			slog.Any("error", err))
		return
	}
	if c.ack(ctx, msg.ID) {
		logger.WarnContext(ctx, "poison message diverted and acked")
	}
}

// safePoison invokes the poison callback, converting a panic into an error
// so a faulty callback cannot kill the consumer loop.
func safePoison(ctx context.Context, h PoisonHandler, p PoisonMessage) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("queue: poison handler panic: %v\n%s", r, debug.Stack())
		}
	}()
	return h(ctx, p)
}
