package pubsub

import "time"

// Metrics is the observability seam for the publisher (ticket 16.2, ADR-008
// subsystem "events"). The worker and API pass a concrete recorder (their
// obs/metrics set); tests and the default pass NopMetrics. Every method must be
// cheap and non-blocking — they run on the publisher's drain goroutine.
type Metrics interface {
	// EventPublished records one successful PUBLISH to a channel. channel is
	// "run" or "firehose" — the bounded {channel} label; never the concrete run
	// channel name (that would be unbounded cardinality).
	EventPublished(channel string)
	// PublishFailed records one failed PUBLISH (Redis error or timeout).
	PublishFailed()
	// PublishDropped records n envelopes dropped because the publish buffer was
	// full (a slow or stalled Redis) — the events are still durable in Postgres
	// and reach consumers via backfill.
	PublishDropped(n int)
	// PublishLatency records the commit-to-published latency of one envelope.
	PublishLatency(d time.Duration)
}

// Channel label values for EventPublished (bounded, ADR-008).
const (
	ChannelRun      = "run"
	ChannelFirehose = "firehose"
)

// NopMetrics is the no-op Metrics used when none is wired.
type NopMetrics struct{}

func (NopMetrics) EventPublished(string)        {}
func (NopMetrics) PublishFailed()               {}
func (NopMetrics) PublishDropped(int)           {}
func (NopMetrics) PublishLatency(time.Duration) {}
