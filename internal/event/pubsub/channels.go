// Package pubsub is the Redis pub/sub fan-out for agentloom's event feed
// (ticket 16.2, ADR-018): the low-latency delivery hint that rides alongside
// the durable per-run event log in Postgres.
//
// It is deliberately a leaf beside internal/event: it imports internal/event
// (for the Envelope it marshals and parses) and go-redis, but never
// internal/store — the durable-truth layer plugs the Publisher into the store
// via the store.EventSink seam, so persistence never depends on the transport.
// This mirrors how internal/cache/redisstore and internal/retrieval/pgfts sit
// beside their leaf contracts.
//
// The contract (ADR-018): Postgres is the truth; publishing is best-effort. A
// Publisher fan-outs each committed envelope to a per-run channel and a firehose
// channel after the transaction commits, and a Subscriber turns received
// messages back into typed envelopes. Delivery is at-least-once — a consumer
// dedupes and orders by (run_id, seq) and heals any missed or out-of-order
// message via a DB backfill (the Tailer). A publish failure is logged and
// metered, never surfaced to the engine.
package pubsub

import (
	"github.com/google/uuid"
)

// DefaultPrefix is the Redis channel namespace when the operator sets none.
// Channels are "<prefix>:run:<uuid>" (per run) and "<prefix>:firehose" (all
// runs) — the prefix is a test-isolation and multi-deploy knob.
const DefaultPrefix = "events"

// RunChannel is the per-run pub/sub channel for a run's event feed. A run
// WebSocket (16.3) subscribes to exactly this channel for its run.
func RunChannel(prefix string, runID uuid.UUID) string {
	return prefix + ":run:" + runID.String()
}

// FirehoseChannel is the single channel carrying every run's events, for the
// multi-run dashboard firehose (16.4). Every published envelope goes to both
// its run channel and this one.
func FirehoseChannel(prefix string) string {
	return prefix + ":firehose"
}
