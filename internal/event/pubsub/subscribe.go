package pubsub

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/event"
)

// redisSubscriber is the subscribe surface a Subscription needs; *redis.Client
// satisfies it.
type redisSubscriber interface {
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

// Subscription is a live tail of one pub/sub channel, decoded to typed
// envelopes (ticket 16.2, ADR-018). A message that fails to parse (unknown type,
// bad JSON, unsupported envelope version) is logged and dropped — a gap the
// consumer heals via DB backfill, never a fatal error. Close releases the
// underlying Redis subscription and closes the Events channel.
type Subscription struct {
	ps      *redis.PubSub
	out     chan event.Envelope
	log     *slog.Logger
	channel string
}

// SubscribeRun subscribes to one run's event channel. It blocks until the
// SUBSCRIBE is confirmed by Redis, so a caller can subscribe, then read a
// snapshot + backfill from the DB with no window in which a live event is
// missed between the two (the 16.3 protocol ordering). The caller must Close the
// returned Subscription.
func SubscribeRun(ctx context.Context, client redisSubscriber, prefix string, runID uuid.UUID, log *slog.Logger) (*Subscription, error) {
	return subscribe(ctx, client, RunChannel(prefix, runID), log)
}

// SubscribeFirehose subscribes to the all-runs firehose channel (16.4).
func SubscribeFirehose(ctx context.Context, client redisSubscriber, prefix string, log *slog.Logger) (*Subscription, error) {
	return subscribe(ctx, client, FirehoseChannel(prefix), log)
}

func subscribe(ctx context.Context, client redisSubscriber, channel string, log *slog.Logger) (*Subscription, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ps := client.Subscribe(ctx, channel)
	// Wait for the subscribe confirmation so no live message can slip between
	// "subscribed" and the caller's snapshot read.
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, err
	}
	s := &Subscription{
		ps:      ps,
		out:     make(chan event.Envelope, 256),
		log:     log,
		channel: channel,
	}
	go s.pump(ctx)
	return s, nil
}

// pump reads raw messages, parses them to typed envelopes, and forwards them.
// A parse error is dropped (the consumer heals via backfill). It exits when the
// Redis message channel closes (Close was called) or ctx is done.
func (s *Subscription) pump(ctx context.Context) {
	defer close(s.out)
	msgs := s.ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			env, err := event.ParseEnvelope([]byte(m.Payload))
			if err != nil {
				s.log.Warn("pubsub: dropping unparseable message (consumer heals via backfill)",
					slog.String("channel", s.channel), slog.Any("error", err))
				continue
			}
			select {
			case s.out <- env:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Events is the channel of decoded envelopes. It closes when the Subscription
// is Closed or its context is done. Ordering is Redis pub/sub arrival order; a
// consumer that needs per-run seq order feeds these through a Tailer.
func (s *Subscription) Events() <-chan event.Envelope { return s.out }

// Close unsubscribes and releases the Redis connection. The Events channel
// closes once the pump goroutine observes the closed message channel.
func (s *Subscription) Close() error { return s.ps.Close() }
