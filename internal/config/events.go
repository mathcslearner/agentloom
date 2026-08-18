package config

import "time"

// Environment variables read by EventsConfig.
const (
	EnvEventsPubSubEnabled  = "AGENTLOOM_EVENTS_PUBSUB_ENABLED"
	EnvEventsChannelPrefix  = "AGENTLOOM_EVENTS_CHANNEL_PREFIX"
	EnvEventsPublishBuffer  = "AGENTLOOM_EVENTS_PUBLISH_BUFFER"
	EnvEventsPublishTimeout = "AGENTLOOM_EVENTS_PUBLISH_TIMEOUT"
)

// Events defaults (ticket 16.2, ADR-018).
const (
	// DefaultEventsChannelPrefix is the Redis pub/sub channel namespace; a
	// test-isolation and multi-deploy knob mirroring the cache key prefix.
	DefaultEventsChannelPrefix = "events"
	// DefaultEventsPublishBuffer is the number of committed batches the
	// publisher queues before dropping (the events stay durable in Postgres and
	// reach consumers via backfill).
	DefaultEventsPublishBuffer = 1024
	// DefaultEventsPublishTimeout bounds one batch's Redis PUBLISH calls so a
	// hung connection cannot wedge the publisher's drain goroutine.
	DefaultEventsPublishTimeout = 2 * time.Second
)

// EventsConfig configures the live event pub/sub publish path (ticket 16.2,
// internal/event/pubsub). The worker and API build a publisher over the same
// Redis client the queue/cache use (the shared coordination Redis, ADR-002) and
// wire it into the store as the after-commit event sink. Publishing is a
// best-effort latency hint on top of the durable Postgres event log: a publish
// failure or a full buffer never affects the engine transaction, and consumers
// heal any miss via a DB backfill. Enabled by default — the worker already
// requires Redis, and default-on pub/sub is what gives the dashboard low-latency
// tails with zero configuration; set AGENTLOOM_EVENTS_PUBSUB_ENABLED=false to run
// with only the durable log (WS clients then rely on backfill polling).
type EventsConfig struct {
	// PubSubEnabled turns the Redis pub/sub publish path on. Default true.
	PubSubEnabled bool
	// ChannelPrefix is the Redis channel namespace (run:{id} and firehose live
	// under it). Defaults to DefaultEventsChannelPrefix.
	ChannelPrefix string
	// PublishBuffer is the queued-batch capacity before a drop. Must be positive.
	PublishBuffer int
	// PublishTimeout bounds one batch's publishes. Must be positive.
	PublishTimeout time.Duration
}

func defaultEventsConfig() EventsConfig {
	return EventsConfig{
		PubSubEnabled:  true,
		ChannelPrefix:  DefaultEventsChannelPrefix,
		PublishBuffer:  DefaultEventsPublishBuffer,
		PublishTimeout: DefaultEventsPublishTimeout,
	}
}

// applyEnv overrides fields from the environment, reporting one error per
// invalid variable.
func (c *EventsConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	errs = applyBool(errs, fn, EnvEventsPubSubEnabled, &c.PubSubEnabled)
	applyString(fn, EnvEventsChannelPrefix, &c.ChannelPrefix)
	errs = applyPositiveInt(errs, fn, EnvEventsPublishBuffer, &c.PublishBuffer)
	errs = applyPositiveDuration(errs, fn, EnvEventsPublishTimeout, &c.PublishTimeout)
	return errs
}
