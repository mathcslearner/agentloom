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
	// DefaultConsumerBatch is the XREADGROUP COUNT, and also the per-tick
	// XAUTOCLAIM batch bound.
	DefaultConsumerBatch = 16
	// DefaultConsumerBlock is the XREADGROUP BLOCK timeout.
	DefaultConsumerBlock = 5 * time.Second
	// DefaultErrorBackoff spaces read retries after a transport error so a
	// Redis outage does not hot-spin the loop.
	DefaultErrorBackoff = time.Second
	// DefaultLeaseTTL is the XAUTOCLAIM min-idle threshold: a PEL entry
	// idle longer than this is an expired lease.
	DefaultLeaseTTL = 30 * time.Second
	// DefaultPoisonThreshold is the delivery count above which a reclaimed
	// entry is diverted to the poison callback instead of the handler.
	DefaultPoisonThreshold = 5
	// DefaultJanitorInterval is the period between orphan-consumer cleanup
	// passes.
	DefaultJanitorInterval = 10 * time.Minute
	// DefaultJanitorIdleThreshold is how long a consumer with an empty PEL
	// must be idle before the janitor deletes it.
	DefaultJanitorIdleThreshold = time.Hour
	// DefaultTrimInterval is the period between stream retention passes
	// (TrimAcked). Retention is a memory concern, not correctness; a
	// minute bounds acked-entry buildup to a minute of throughput.
	DefaultTrimInterval = time.Minute
)

// detachedOpTimeout bounds Redis commands issued on a detached context —
// the post-success XACK and lease heartbeats, both of which must outlive a
// canceled loop context during shutdown drain.
const detachedOpTimeout = 5 * time.Second

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

// Phase identifies the instrumented points in the per-message path where
// the chaos harness (internal/queue/queuetest, ticket 3.6) injects
// simulated crashes. The hook exists because one crash cell cannot be
// provoked from outside this package: die-after-work-before-ACK (crash
// matrix W4) — after a successful handler the ack is unconditional and
// runs on a detached context, so no amount of context cancellation can
// suppress it.
type Phase int

const (
	// PhasePreHandle is after the envelope decodes, before the
	// heartbeater starts and the handler runs (crash cell W2: die after
	// claim, before execution).
	PhasePreHandle Phase = iota + 1
	// PhasePreAck is after the handler returns nil (and the heartbeater
	// has stopped), before XACK (crash cell W4: die after work, before
	// ack).
	PhasePreAck
)

// String names the phase for logs and test failures.
func (p Phase) String() string {
	switch p {
	case PhasePreHandle:
		return "pre-handle"
	case PhasePreAck:
		return "pre-ack"
	default:
		return fmt.Sprintf("phase(%d)", int(p))
	}
}

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
	// LeaseTTL is the XAUTOCLAIM min-idle threshold: how long a PEL entry
	// may sit idle before any consumer may reclaim it. Idle time is
	// tracked by the Redis server against its own clock, so expiry is
	// immune to worker clock skew.
	LeaseTTL time.Duration
	// HeartbeatInterval is the base period between XCLAIM JUSTID
	// heartbeats for the in-flight entry; each beat applies ±20% jitter so
	// a fleet never aligns. Zero derives LeaseTTL/3, keeping ADR-005's
	// two-missed-beats-still-precede-expiry margin when only the TTL is
	// tuned.
	HeartbeatInterval time.Duration
	// ReclaimInterval is the period between XAUTOCLAIM passes. Zero
	// derives LeaseTTL/2, bounding worst-case reclaim latency to
	// TTL + TTL/2 per ADR-005.
	ReclaimInterval time.Duration
	// PoisonThreshold is the delivery count above which a reclaimed entry
	// is diverted to the PoisonHandler instead of the Handler.
	PoisonThreshold int
	// PoisonHandler receives entries whose delivery count exceeded
	// PoisonThreshold. Nil return acks the entry (M5.4 wires dead-lettering
	// here); an error leaves it pending for the next reclaim pass. A nil
	// PoisonHandler logs and leaves the entry pending — visible spin beats
	// a silent drop, per ADR-005.
	PoisonHandler PoisonHandler
	// JanitorInterval is the period between orphan-consumer cleanup passes.
	JanitorInterval time.Duration
	// JanitorIdleThreshold is how long a consumer with an empty PEL must be
	// idle before the janitor deletes it. Generous by design: cleanup is
	// cosmetic-plus-memory, and a merely quiet consumer still heartbeats
	// its entries, keeping them visibly pending and itself undeletable.
	JanitorIdleThreshold time.Duration
	// TrimInterval is the period between stream retention passes: each
	// pass XTRIMs entries the group has delivered and acked (ADR-005,
	// stream retention). Pending and undelivered entries are never
	// trimmed.
	TrimInterval time.Duration
	// PromoterTick is the period between delayed-delivery promotion passes
	// (3.5). Each pass moves up to Batch due entries from the delayed set
	// onto the ready stream.
	PromoterTick time.Duration
	// DrainTimeout is the graceful-shutdown grace period (ticket 5.7).
	// When positive, canceling Run's context stops fresh reads and the
	// periodic duties but NOT the work: the in-flight handler and every
	// already-delivered entry keep processing — heartbeating and acking as
	// usual, under a context that survives the cancellation — until they
	// finish or this budget elapses, at which point the remaining work is
	// abandoned un-acked and its leases expire naturally into reclaim. A
	// consumer that drains fully clean also deletes its own consumer-group
	// record on exit, so graceful restarts leave no orphan for the janitor.
	//
	// Zero (the default — withDefaults deliberately does not touch it)
	// keeps the pre-5.7 semantics: handlers see the cancellation
	// immediately and delivered-but-unhandled entries are abandoned. That
	// mode is how the queuetest chaos harness simulates crashes, so it
	// must stay a faithful sudden death: no drain, no deregistration.
	DrainTimeout time.Duration
	// DelayedKey is the delayed-delivery sorted-set key this consumer
	// promotes from. Empty falls back to DefaultDelayedKey; tests pass
	// unique keys for isolation.
	DelayedKey string
	// PhaseHook, when non-nil, is invoked at each Phase of the
	// per-message path — fresh and reclaimed deliveries alike, since
	// process is the single path. A non-nil error aborts the message at
	// that point: no ack, the entry stays in the PEL, exactly as if the
	// process had died there. Test instrumentation for the queuetest
	// chaos harness (ticket 3.6); production configs leave it nil.
	PhaseHook func(phase Phase, d Delivery) error
	// Metrics receives duty observations (ticket 7.2): reclaims, poison
	// diversions, delayed promotions. Nil means the no-op recorder —
	// cmd/worker wires the Prometheus implementation.
	Metrics ConsumerMetrics
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
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = DefaultLeaseTTL
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = c.LeaseTTL / 3
	}
	if c.ReclaimInterval <= 0 {
		c.ReclaimInterval = c.LeaseTTL / 2
	}
	if c.PoisonThreshold <= 0 {
		c.PoisonThreshold = DefaultPoisonThreshold
	}
	if c.JanitorInterval <= 0 {
		c.JanitorInterval = DefaultJanitorInterval
	}
	if c.JanitorIdleThreshold <= 0 {
		c.JanitorIdleThreshold = DefaultJanitorIdleThreshold
	}
	if c.TrimInterval <= 0 {
		c.TrimInterval = DefaultTrimInterval
	}
	if c.PromoterTick <= 0 {
		c.PromoterTick = DefaultPromoterTick
	}
	if c.Metrics == nil {
		c.Metrics = nopConsumerMetrics{}
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
	delayed *Delayed
	// reclaimCursor is the XAUTOCLAIM scan position, persisted across
	// ticks so a bounded per-tick batch still sweeps the whole PEL over
	// successive passes. Only touched from Run's goroutine.
	reclaimCursor string
	// work is the handler-side context of the current Run (ticket 5.7):
	// with a drain budget it survives Run's ctx cancellation until the
	// drain deadline; without one it is Run's ctx itself. Set at the top
	// of Run; only touched from Run's goroutine.
	work context.Context
	// drain accumulates the per-entry shutdown dispositions finishShutdown
	// reports. Only touched from Run's goroutine.
	drain drainStats
}

// drainStats counts what became of the entries a shutting-down consumer
// still had in hand (ticket 5.7): drained (handled to completion and
// acked), redeliver (handler failed — the entry stays pending and
// redelivers via reclaim, as in normal operation), abandoned (not handled,
// or aborted mid-handler, because the drain deadline passed — the lease
// expires naturally into reclaim).
type drainStats struct {
	drained   int
	redeliver int
	abandoned int
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
	cfg = cfg.withDefaults()
	return &Consumer{
		queue:         q,
		name:          name,
		handler:       handler,
		cfg:           cfg,
		delayed:       q.NewDelayed(cfg.DelayedKey),
		reclaimCursor: "0-0",
	}
}

// Name returns the consumer-group member name this consumer reads as.
func (c *Consumer) Name() string { return c.name }

// Run blocks reading fresh deliveries and feeding them to the handler
// until ctx is canceled, then drains and returns nil. What draining means
// depends on DrainTimeout (ticket 5.7): with a budget, the in-flight
// handler and every already-delivered entry finish processing — under the
// work context, which survives the cancellation — before Run returns,
// unless the budget elapses first and the remainder is abandoned to
// natural lease expiry; without one (the zero default), the in-flight
// handler sees the cancellation immediately and only its own return is
// awaited. Either way Run logs a per-entry disposition and a summary, and
// a fully clean drain deregisters the consumer from the group. Run
// ensures the consumer group exists first; that is the only error it
// returns. Read errors are logged and retried after ErrorBackoff — a
// Redis outage stalls the loop, it does not kill it.
//
// Between reads, Run also performs the consumer's periodic duties: the
// reclaim pass (XAUTOCLAIM of expired leases, every ReclaimInterval), the
// delayed-delivery promoter (every PromoterTick), the orphan-consumer
// janitor (every JanitorInterval), and the stream trimmer (every
// TrimInterval). Duties share the
// loop rather than running as goroutines so handler invocations stay
// strictly serialized — one Consumer, one handler at a time — which is
// what the drain-on-shutdown contract and 3.3's tests assume; a consumer
// busy in a long handler simply doesn't reclaim, and any idle consumer in
// the fleet does instead (recovery is leaderless per ADR-005).
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
		slog.Duration("block", c.cfg.Block),
		slog.Duration("lease_ttl", c.cfg.LeaseTTL),
		slog.Duration("heartbeat_interval", c.cfg.HeartbeatInterval),
		slog.Duration("reclaim_interval", c.cfg.ReclaimInterval),
		slog.Int("poison_threshold", c.cfg.PoisonThreshold),
		slog.Duration("promoter_tick", c.cfg.PromoterTick),
		slog.Duration("trim_interval", c.cfg.TrimInterval),
		slog.Duration("drain_timeout", c.cfg.DrainTimeout),
		slog.String("delayed_key", c.delayed.Key()))
	defer logger.InfoContext(ctx, "queue consumer stopped")

	// The work context is what handlers run under. With a drain budget it
	// survives ctx's cancellation — shutdown stops the reads, not the
	// work — until the watchdog cancels it at the drain deadline. Without
	// one it IS ctx: cancellation reaches the handler immediately, the
	// sudden-death mode the chaos harness relies on.
	c.work = ctx
	c.drain = drainStats{}
	if c.cfg.DrainTimeout > 0 {
		work, hardCancel := context.WithCancel(context.WithoutCancel(ctx))
		defer hardCancel()
		stopWatchdog := c.startDrainWatchdog(ctx, hardCancel)
		defer stopWatchdog()
		c.work = work
	}

	nextReclaim := time.Now().Add(c.cfg.ReclaimInterval)
	nextJanitor := time.Now().Add(c.cfg.JanitorInterval)
	nextPromote := time.Now().Add(c.cfg.PromoterTick)
	nextTrim := time.Now().Add(c.cfg.TrimInterval)
	for {
		if ctx.Err() != nil {
			c.finishShutdown(ctx)
			return nil
		}
		if !time.Now().Before(nextReclaim) {
			c.reclaimTick(ctx)
			nextReclaim = time.Now().Add(c.cfg.ReclaimInterval)
		}
		if !time.Now().Before(nextJanitor) {
			c.janitorTick(ctx)
			nextJanitor = time.Now().Add(c.cfg.JanitorInterval)
		}
		if !time.Now().Before(nextPromote) {
			c.promoteTick(ctx)
			nextPromote = time.Now().Add(c.cfg.PromoterTick)
		}
		if !time.Now().Before(nextTrim) {
			c.trimTick(ctx)
			nextTrim = time.Now().Add(c.cfg.TrimInterval)
		}
		if ctx.Err() != nil {
			continue // loop top runs the shutdown path
		}
		// Cap the blocking read at the next due duty so an idle consumer's
		// reclaim and promotion cadences are governed by their intervals,
		// not by Block.
		block := c.cfg.Block
		nextDuty := nextReclaim
		if nextJanitor.Before(nextDuty) {
			nextDuty = nextJanitor
		}
		if nextPromote.Before(nextDuty) {
			nextDuty = nextPromote
		}
		if nextTrim.Before(nextDuty) {
			nextDuty = nextTrim
		}
		if until := time.Until(nextDuty); until < block {
			block = max(until, time.Millisecond)
		}
		// A full batch becomes PEL leases at read time, but entries queue
		// behind the sequential handler without heartbeats until their
		// turn. A reclaim of a queued entry is safe (duplicates die at the
		// claim CAS) but wasteful; batch size versus lease TTL is a real
		// tuning interaction.
		streams, err := c.queue.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.queue.group,
			Consumer: c.name,
			Streams:  []string{c.queue.stream, ">"},
			Count:    int64(c.cfg.Batch),
			Block:    block,
		}).Result()
		switch {
		case err == nil:
		case errors.Is(err, redis.Nil):
			// BLOCK expired with nothing to read.
			continue
		case ctx.Err() != nil:
			// Canceled mid-block; the read error is shutdown noise, and
			// the loop top runs the shutdown path.
			continue
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
				// so the count is 1 without an XPENDING round-trip. No
				// early return on cancellation: every read entry is a PEL
				// lease this consumer owes a disposition — deliver drains
				// or abandons it, and the loop top runs the shutdown path
				// once the batch is settled.
				c.deliver(ctx, msg, 1)
			}
		}
	}
}

// deliver feeds one delivered entry through the shared process path under
// the work context and, once shutdown has begun, records and logs its
// disposition (ticket 5.7). Fresh reads and reclaimed entries alike come
// through here: both are leases in this consumer's PEL, and a graceful
// shutdown owes both the same treatment — finish and ack, or abandon
// visibly.
func (c *Consumer) deliver(ctx context.Context, msg redis.XMessage, deliveryCount int64) {
	if c.work.Err() != nil {
		// The drain deadline has passed (or there is no drain budget):
		// leave the entry untouched — its lease expires into reclaim.
		c.drain.abandoned++
		log.From(ctx).WarnContext(ctx, "shutdown: abandoning delivered entry; lease expires into reclaim",
			slog.String("entry_id", msg.ID),
			slog.String("step_id", envelopeStep(msg)))
		return
	}
	acked := c.process(c.work, msg, deliveryCount)
	if ctx.Err() == nil {
		return // normal operation: process said everything there is to say
	}
	switch {
	case acked:
		c.drain.drained++
		log.From(ctx).InfoContext(ctx, "shutdown drain: entry completed and acked",
			slog.String("entry_id", msg.ID),
			slog.String("step_id", envelopeStep(msg)))
	case c.work.Err() != nil:
		c.drain.abandoned++
		log.From(ctx).WarnContext(ctx, "shutdown drain: entry aborted at the drain deadline; lease expires into reclaim",
			slog.String("entry_id", msg.ID),
			slog.String("step_id", envelopeStep(msg)))
	default:
		c.drain.redeliver++
		log.From(ctx).WarnContext(ctx, "shutdown drain: handler failed; entry stays pending to redeliver",
			slog.String("entry_id", msg.ID),
			slog.String("step_id", envelopeStep(msg)))
	}
}

// envelopeStep best-effort decodes an entry's step ID for the shutdown
// disposition logs ("" when the envelope does not decode); the process
// path decodes properly on its own.
func envelopeStep(msg redis.XMessage) string {
	env, err := DecodeEnvelope(msg.Values)
	if err != nil {
		return ""
	}
	return env.StepID
}

// startDrainWatchdog arms the drain deadline: when ctx is canceled —
// shutdown — it logs the drain start and cancels the work context
// DrainTimeout later, abandoning whatever is still in flight. The
// returned stop signals and joins the goroutine, so Run never leaves it
// behind.
func (c *Consumer) startDrainWatchdog(ctx context.Context, hardCancel context.CancelFunc) (stop func()) {
	stopped := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-stopped:
			return
		case <-ctx.Done():
		}
		log.From(ctx).InfoContext(ctx, "shutdown: draining in-flight deliveries",
			slog.Duration("drain_timeout", c.cfg.DrainTimeout))
		t := time.NewTimer(c.cfg.DrainTimeout)
		defer t.Stop()
		select {
		case <-stopped:
		case <-t.C:
			log.From(ctx).WarnContext(ctx, "shutdown: drain deadline exceeded; abandoning remaining deliveries")
			hardCancel()
		}
	}()
	return func() {
		close(stopped)
		<-done
	}
}

// finishShutdown logs the shutdown summary and — in drain mode only, when
// this consumer's PEL is empty — deletes its own consumer-group record,
// so graceful restarts leave no orphan for the janitor to collect an hour
// later. The emptiness check is stable by construction: reads and
// reclaims have stopped, so nothing can be assigned to this consumer
// anymore. An entry left pending keeps the consumer registered — XGROUP
// DELCONSUMER would drop its PEL state — and the zero-drain mode never
// deregisters at all: it simulates crashes, and a crashed process gets no
// exit-time hygiene. Best effort on a detached context.
func (c *Consumer) finishShutdown(ctx context.Context) {
	logger := log.From(ctx)
	logger.InfoContext(ctx, "shutdown drain complete",
		slog.Int("drained", c.drain.drained),
		slog.Int("redeliver", c.drain.redeliver),
		slog.Int("abandoned", c.drain.abandoned))
	if c.cfg.DrainTimeout <= 0 {
		return
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedOpTimeout)
	defer cancel()
	pending, err := c.queue.client.XPendingExt(opCtx, &redis.XPendingExtArgs{
		Stream: c.queue.stream, Group: c.queue.group, Consumer: c.name,
		Start: "-", End: "+", Count: 1,
	}).Result()
	if err != nil {
		logger.WarnContext(ctx, "shutdown: could not inspect own PEL; leaving consumer registered",
			slog.Any("error", err))
		return
	}
	if len(pending) != 0 {
		logger.InfoContext(ctx, "shutdown: entries still pending; consumer stays registered until they resolve")
		return
	}
	if err := c.queue.client.XGroupDelConsumer(opCtx, c.queue.stream, c.queue.group, c.name).Err(); err != nil {
		logger.WarnContext(ctx, "shutdown: XGROUP DELCONSUMER failed; the janitor will collect this consumer",
			slog.Any("error", err))
		return
	}
	logger.InfoContext(ctx, "shutdown: consumer deregistered from group")
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
	d := Delivery{ID: msg.ID, Envelope: env, DeliveryCount: deliveryCount}
	if err := c.phaseHook(PhasePreHandle, d); err != nil {
		log.From(ctx).WarnContext(ctx, "phase hook aborted before handling; entry stays pending",
			slog.Any("error", err))
		return false
	}
	// The heartbeater keeps this entry's lease alive for the duration of
	// the handler; it stops (and is fully joined) before the ack decision.
	stopHeartbeat := c.startHeartbeat(ctx, msg.ID)
	err = safeHandle(ctx, c.handler, d)
	stopHeartbeat()
	if err != nil {
		log.From(ctx).WarnContext(ctx, "handler failed; entry stays pending for redelivery",
			slog.Any("error", err))
		return false
	}
	if err := c.phaseHook(PhasePreAck, d); err != nil {
		log.From(ctx).WarnContext(ctx, "phase hook aborted before ack; entry stays pending",
			slog.Any("error", err))
		return false
	}
	return c.ack(ctx, msg.ID)
}

// phaseHook runs the configured PhaseHook, if any.
func (c *Consumer) phaseHook(phase Phase, d Delivery) error {
	if c.cfg.PhaseHook == nil {
		return nil
	}
	return c.cfg.PhaseHook(phase, d)
}

// ack removes one entry from the PEL. It runs on a detached context: a
// handler that succeeds while shutdown is in progress must still ack, or
// its completed work would redeliver for nothing.
func (c *Consumer) ack(ctx context.Context, entryID string) bool {
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedOpTimeout)
	defer cancel()
	if err := c.queue.client.XAck(ackCtx, c.queue.stream, c.queue.group, entryID).Err(); err != nil {
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
