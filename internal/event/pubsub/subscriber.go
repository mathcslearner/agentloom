package pubsub

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// Subscriber is a small factory over a Redis client and channel prefix that
// opens per-run (16.3) and firehose (16.4) subscriptions. It carries the two
// things every subscription needs — the client and the channel namespace — so a
// consumer (the WS server) needs only a run id. Keeping it here lets the api
// package hold a subscriber behind a narrow interface without importing go-redis
// (the same shape the Publisher gives the write side).
type Subscriber struct {
	client redisSubscriber
	prefix string
	log    *slog.Logger
}

// NewSubscriber builds a Subscriber. prefix defaults to DefaultPrefix; log
// defaults to a discard logger. The client is the shared coordination Redis
// client the queue/cache/publisher use.
func NewSubscriber(client redisSubscriber, prefix string, log *slog.Logger) *Subscriber {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Subscriber{client: client, prefix: prefix, log: log}
}

// SubscribeRun opens a live subscription to one run's event channel. It blocks
// until the SUBSCRIBE is confirmed, so a caller can subscribe, then read a DB
// snapshot + backfill with no window in which a live event is missed (the 16.3
// protocol ordering). The caller must Close the returned Subscription.
func (s *Subscriber) SubscribeRun(ctx context.Context, runID uuid.UUID) (*Subscription, error) {
	return SubscribeRun(ctx, s.client, s.prefix, runID, s.log)
}

// SubscribeFirehose opens a live subscription to the all-runs firehose channel
// (16.4). The caller must Close the returned Subscription.
func (s *Subscriber) SubscribeFirehose(ctx context.Context) (*Subscription, error) {
	return SubscribeFirehose(ctx, s.client, s.prefix, s.log)
}
