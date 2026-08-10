package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// Consumer tuning defaults per ADR-005's tuning table. config.QueueConfig
// carries the deployable knobs and mirrors these values (it cannot import
// them: queue → obs/log → config would make that an import cycle).
const (
	// DefaultConsumerBatch is the XREADGROUP COUNT.
	DefaultConsumerBatch = 16
	// DefaultConsumerBlock is the XREADGROUP BLOCK timeout.
	DefaultConsumerBlock = 5 * time.Second
	// DefaultErrorBackoff spaces read retries after a transport error so a
	// Redis outage does not hot-spin the loop.
	DefaultErrorBackoff = time.Second
)

// ackTimeout bounds the detached-context XACK issued after a handler
// succeeds; see process.
const ackTimeout = 5 * time.Second

// Delivery is one delivered task message as a Handler sees it.
type Delivery struct {
	// ID is the Redis stream entry ID.
	ID string
	// Envelope is the decoded task envelope.
	Envelope Envelope
	// DeliveryCount is the number of times this entry has been delivered:
	// 1 on the fresh-read path (XREADGROUP `>` only delivers entries never
	// delivered before), higher when the entry arrives via reclaim (3.4).
	// It is the poison-message signal of ADR-005, surfaced now so the
	// handler contract does not change when the reclaimer lands.
	DeliveryCount int64
}

// Handler processes one delivery. A nil return acks the entry — the queue
// forgets it; an error leaves it in the PEL to redeliver via reclaim. A
// panic is recovered and treated as an error. Delivery is at-least-once:
// handlers must tolerate duplicates — the claim CAS (2.6) is the dedup,
// per ADR-005.
type Handler func(ctx context.Context, d Delivery) error

// ConsumerConfig tunes a consumer loop. The zero value is ready to use:
// zero fields fall back to the ADR-005 defaults.
type ConsumerConfig struct {
	// Batch is the XREADGROUP COUNT: the maximum entries fetched per read.
	Batch int
	// Block is the XREADGROUP BLOCK timeout: the upper bound on how long an
	// idle consumer waits for work before re-checking for shutdown.
	Block time.Duration
	// ErrorBackoff is the pause after a failed read before retrying. Tests
	// tune it down rather than injecting a clock — the ADR-005 convention
	// for queue timing, where Redis-side blocking already binds the loop to
	// real time.
	ErrorBackoff time.Duration
}

func (c ConsumerConfig) withDefaults() ConsumerConfig {
	if c.Batch <= 0 {
		c.Batch = DefaultConsumerBatch
	}
	if c.Block <= 0 {
		c.Block = DefaultConsumerBlock
	}
	if c.ErrorBackoff <= 0 {
		c.ErrorBackoff = DefaultErrorBackoff
	}
	return c
}

// Consumer is one member of the queue's consumer group: a blocking
// XREADGROUP loop feeding deliveries to a Handler one at a time.
// Parallelism comes from running multiple Consumers with distinct names,
// not from concurrency inside one loop.
type Consumer struct {
	queue   *Queue
	name    string
	handler Handler
	cfg     ConsumerConfig
}

// NewConsumer binds a handler to this queue under the given consumer name.
// An empty name gets a fresh incarnation-unique NewConsumerName. A nil
// handler is a programming error and panics here rather than at first
// delivery.
func (q *Queue) NewConsumer(name string, handler Handler, cfg ConsumerConfig) *Consumer {
	if handler == nil {
		panic("queue: NewConsumer requires a handler")
	}
	if name == "" {
		name = NewConsumerName()
	}
	return &Consumer{queue: q, name: name, handler: handler, cfg: cfg.withDefaults()}
}

// Name returns the consumer-group member name this consumer reads as.
func (c *Consumer) Name() string { return c.name }

// Run blocks reading fresh deliveries and feeding them to the handler
// until ctx is canceled, then returns nil once the in-flight handler (if
// any) has drained. It ensures the consumer group exists first; that is
// the only error it returns. Read errors are logged and retried after
// ErrorBackoff — a Redis outage stalls the loop, it does not kill it.
func (c *Consumer) Run(ctx context.Context) error {
	if err := c.queue.EnsureGroup(ctx); err != nil {
		return err
	}
	ctx = log.With(ctx,
		slog.String("stream", c.queue.stream),
		slog.String("group", c.queue.group),
		slog.String("consumer", c.name))
	logger := log.From(ctx)
	logger.InfoContext(ctx, "queue consumer started",
		slog.Int("batch", c.cfg.Batch),
		slog.Duration("block", c.cfg.Block))
	defer logger.InfoContext(ctx, "queue consumer stopped")

	for {
		if ctx.Err() != nil {
			return nil
		}
		// Note for 3.4: every entry in a batch becomes a PEL lease at read
		// time, but entries queue behind the sequential handler without
		// heartbeats until their turn. A reclaim of a queued entry is safe
		// (duplicates die at the claim CAS) but wasteful; batch size versus
		// lease TTL is a real tuning interaction.
		streams, err := c.queue.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.queue.group,
			Consumer: c.name,
			Streams:  []string{c.queue.stream, ">"},
			Count:    int64(c.cfg.Batch),
			Block:    c.cfg.Block,
		}).Result()
		switch {
		case err == nil:
		case errors.Is(err, redis.Nil):
			// BLOCK expired with nothing to read.
			continue
		case ctx.Err() != nil:
			// Canceled mid-block; the read error is shutdown noise.
			return nil
		default:
			logger.ErrorContext(ctx, "XREADGROUP failed; backing off",
				slog.Any("error", err),
				slog.Duration("backoff", c.cfg.ErrorBackoff))
			sleepCtx(ctx, c.cfg.ErrorBackoff)
			continue
		}
		for _, s := range streams {
			for _, msg := range s.Messages {
				// Fresh `>` deliveries are by definition first deliveries,
				// so the count is 1 without an XPENDING round-trip.
				c.process(ctx, msg, 1)
				if ctx.Err() != nil {
					return nil
				}
			}
		}
	}
}

// process runs one delivered entry through decode → handler → ACK and
// reports whether the entry was acked. It is the single per-message path:
// Run feeds it fresh reads, and ticket 3.4's reclaimer feeds it reclaimed
// entries with their real delivery counts. False means the entry stays in
// the PEL, to redeliver via reclaim or walk into the poison path as its
// delivery count rises.
func (c *Consumer) process(ctx context.Context, msg redis.XMessage, deliveryCount int64) bool {
	ctx = log.With(ctx,
		slog.String("entry_id", msg.ID),
		slog.Int64("delivery_count", deliveryCount))
	env, err := DecodeEnvelope(msg.Values)
	if err != nil {
		// No ACK, per ADR-005: the rising delivery count walks the entry
		// into the poison path, where it is dead-lettered with contents
		// preserved rather than silently dropped.
		log.From(ctx).ErrorContext(ctx, "undecodable envelope; leaving entry for poison path",
			slog.Any("error", err))
		return false
	}
	ctx = log.With(ctx,
		log.RunID(env.RunID.String()),
		log.StepID(env.StepID),
		slog.String("reason", env.Reason))
	if err := safeHandle(ctx, c.handler, Delivery{ID: msg.ID, Envelope: env, DeliveryCount: deliveryCount}); err != nil {
		log.From(ctx).WarnContext(ctx, "handler failed; entry stays pending for redelivery",
			slog.Any("error", err))
		return false
	}
	// The ACK runs on a detached context: a handler that succeeds while
	// shutdown is in progress must still ack, or its completed work would
	// redeliver for nothing.
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ackTimeout)
	defer cancel()
	if err := c.queue.client.XAck(ackCtx, c.queue.stream, c.queue.group, msg.ID).Err(); err != nil {
		log.From(ctx).ErrorContext(ctx, "XACK failed; entry will redeliver as a duplicate",
			slog.Any("error", err))
		return false
	}
	return true
}

// safeHandle invokes the handler, converting a panic into an error so one
// poisoned message cannot kill the consumer loop.
func safeHandle(ctx context.Context, h Handler, d Delivery) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("queue: handler panic: %v\n%s", r, debug.Stack())
		}
	}()
	return h(ctx, d)
}

// sleepCtx waits d or until ctx is canceled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
