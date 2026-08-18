package pubsub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/event"
)

// Default publisher tuning.
const (
	// DefaultBuffer is the number of committed batches the publisher queues
	// before it drops (metering the loss). A drop is safe — the events are
	// durable in Postgres and reach consumers via backfill — so a bounded buffer
	// keeps a stalled Redis from growing memory without bound.
	DefaultBuffer = 1024
	// DefaultPublishTimeout bounds one batch's PUBLISH calls, so a hung Redis
	// connection cannot wedge the drain goroutine forever.
	DefaultPublishTimeout = 2 * time.Second
)

// redisPublisher is the minimal publish surface the Publisher needs; *redis.Client
// satisfies it. Narrowing it to this one method keeps the publisher testable
// without a live Redis and documents that the publisher only ever PUBLISHes.
type redisPublisher interface {
	Publish(ctx context.Context, channel string, message any) *redis.IntCmd
}

// Options configure a Publisher. The zero value of each field falls back to its
// documented default, so an empty Options is valid.
type Options struct {
	// Prefix is the Redis channel namespace (DefaultPrefix if empty).
	Prefix string
	// Buffer is the queued-batch capacity before drop (DefaultBuffer if 0).
	Buffer int
	// PublishTimeout bounds one batch's publishes (DefaultPublishTimeout if 0).
	PublishTimeout time.Duration
	// Logger receives best-effort warnings (a nop logger if nil).
	Logger *slog.Logger
	// Metrics records publish/failure/drop/latency (NopMetrics if nil).
	Metrics Metrics
	// Clock supplies the current time for latency (time.Now if nil). Injectable
	// so latency assertions run on a controlled clock.
	Clock func() time.Time
}

// batch is one committed transaction's envelopes plus when they were handed to
// the publisher (the commit-adjacent moment latency is measured from).
type batch struct {
	envs      []event.Envelope
	committed time.Time
}

// Publisher fans committed event envelopes out to Redis pub/sub, after the
// transaction commits, best-effort (ticket 16.2, ADR-018). It satisfies
// store.EventSink structurally (EventsCommitted), so the store calls it directly
// after a commit. Publishing is async: EventsCommitted enqueues and returns
// immediately; a single drain goroutine PUBLISHes in commit order. A full buffer
// drops (metered); a publish error is logged and metered. Neither ever affects
// the caller — the transaction has already committed.
type Publisher struct {
	client   redisPublisher
	prefix   string
	firehose string
	timeout  time.Duration
	log      *slog.Logger
	metrics  Metrics
	now      func() time.Time

	ch        chan batch
	wg        sync.WaitGroup
	closeOnce sync.Once

	// publishFn does one channel PUBLISH; a seam so tests can simulate a hung or
	// failing publish without a live Redis. Defaults to publishing via client.
	publishFn func(ctx context.Context, channel string, msg []byte) error
}

// NewPublisher builds a Publisher over the given Redis client and starts its
// drain goroutine. The caller must Close it (after the components that write
// events have stopped) to drain and release the goroutine.
func NewPublisher(client redisPublisher, opts Options) *Publisher {
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	timeout := opts.PublishTimeout
	if timeout <= 0 {
		timeout = DefaultPublishTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := opts.Metrics
	if m == nil {
		m = NopMetrics{}
	}
	now := opts.Clock
	if now == nil {
		now = time.Now
	}
	p := &Publisher{
		client:   client,
		prefix:   prefix,
		firehose: FirehoseChannel(prefix),
		timeout:  timeout,
		log:      log,
		metrics:  m,
		now:      now,
		ch:       make(chan batch, buffer),
	}
	p.publishFn = p.publish
	p.wg.Add(1)
	go p.drain()
	return p
}

// EventsCommitted implements store.EventSink: it enqueues a committed
// transaction's envelopes for best-effort fan-out and returns immediately. A
// full buffer drops the batch (metered) rather than blocking the caller — the
// events are durable in Postgres and a consumer heals the gap via backfill. The
// ctx is unused: enqueue is non-blocking and publishing rides the drain
// goroutine's own bounded context.
func (p *Publisher) EventsCommitted(_ context.Context, envs []event.Envelope) {
	if len(envs) == 0 {
		return
	}
	// Copy: the store may reuse the slice after this returns.
	cp := make([]event.Envelope, len(envs))
	copy(cp, envs)
	b := batch{envs: cp, committed: p.now()}
	select {
	case p.ch <- b:
	default:
		p.metrics.PublishDropped(len(envs))
		p.log.Warn("pubsub: publish buffer full, dropping batch (events remain durable in Postgres)",
			slog.Int("dropped", len(envs)))
	}
}

// drain publishes queued batches until the channel is closed and empty.
func (p *Publisher) drain() {
	defer p.wg.Done()
	for b := range p.ch {
		p.publishBatch(b)
	}
}

// publishBatch publishes every envelope in a batch to its run channel and the
// firehose, under a single bounded context.
func (p *Publisher) publishBatch(b batch) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	for _, env := range b.envs {
		msg, err := json.Marshal(env)
		if err != nil {
			// A projected envelope always marshals; guard defensively.
			p.metrics.PublishFailed()
			p.log.Warn("pubsub: marshaling envelope failed",
				slog.String("type", string(env.Type)), slog.Any("error", err))
			continue
		}
		p.publishOne(ctx, RunChannel(p.prefix, env.RunID), ChannelRun, msg)
		p.publishOne(ctx, p.firehose, ChannelFirehose, msg)
		p.metrics.PublishLatency(p.now().Sub(b.committed))
	}
}

func (p *Publisher) publishOne(ctx context.Context, channel, label string, msg []byte) {
	if err := p.publishFn(ctx, channel, msg); err != nil {
		p.metrics.PublishFailed()
		p.log.Warn("pubsub: publish failed (event remains durable in Postgres; consumers heal via backfill)",
			slog.String("channel_kind", label), slog.Any("error", err))
		return
	}
	p.metrics.EventPublished(label)
}

// publish is the default publishFn: a real Redis PUBLISH.
func (p *Publisher) publish(ctx context.Context, channel string, msg []byte) error {
	return p.client.Publish(ctx, channel, msg).Err()
}

// Close stops accepting new batches and drains the queued ones, bounded by ctx.
// It is idempotent. After Close returns the drain goroutine has exited (on a
// clean drain) or ctx expired first (queued batches may be undelivered — they
// remain durable in Postgres). Call it after the event-writing components have
// stopped, so no EventsCommitted races the channel close.
func (p *Publisher) Close(ctx context.Context) error {
	p.closeOnce.Do(func() { close(p.ch) })
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
