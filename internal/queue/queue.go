// Package queue implements the dispatch side of ADR-005's queue and lease
// protocol on Redis Streams: task envelopes (pointers to durable state,
// never payloads) on a stream consumed through a consumer group whose
// pending-entries list doubles as the lease ledger.
//
// This package is transport, not truth: Postgres remains the source of
// truth, enqueue is at-least-once, and every duplicate is absorbed
// downstream by the claim CAS (ticket 2.6). Ticket 3.2 provides the
// primitives here — client wiring, the envelope codec, the XADD producer,
// race-safe group bootstrap, and stream/PEL introspection; the consumer
// loop (3.3), lease heartbeat/reclaim (3.4), and delayed delivery (3.5)
// build on them.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Default stream and group names per ADR-005: one `steps:ready` stream, one
// `workers` consumer group. The M19 scale lever is sharding into K streams;
// every mechanism here is per-stream, so New simply gets called K times.
const (
	DefaultStream = "steps:ready"
	DefaultGroup  = "workers"
)

// Open connects a go-redis client to addr and verifies the connection with
// a ping. The caller owns the returned client and must Close it.
func Open(ctx context.Context, addr string) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close() //nolint:errcheck,gosec // best-effort cleanup of an unusable client
		return nil, fmt.Errorf("queue: pinging Redis at %s: %w", addr, err)
	}
	return client, nil
}

// Queue is a producer and introspection handle for one stream + consumer
// group pair.
type Queue struct {
	client redis.Cmdable
	stream string
	group  string
}

// New wraps an existing client. Empty stream or group names fall back to
// the ADR-005 defaults; tests pass unique names for isolation, and M19
// sharding passes per-shard streams.
func New(client redis.Cmdable, stream, group string) *Queue {
	if stream == "" {
		stream = DefaultStream
	}
	if group == "" {
		group = DefaultGroup
	}
	return &Queue{client: client, stream: stream, group: group}
}

// Stream returns the stream key this queue produces to.
func (q *Queue) Stream() string { return q.stream }

// Group returns the consumer group name this queue introspects.
func (q *Queue) Group() string { return q.group }

// Enqueue validates and encodes the envelope and appends it to the stream,
// returning the assigned stream entry ID. Enqueue is at-least-once by
// design: callers (the outbox drainer, the reconciler, the promoter) may
// produce duplicates, which the claim CAS deduplicates downstream — no
// trimming or dedup happens here.
func (q *Queue) Enqueue(ctx context.Context, env Envelope) (string, error) {
	values, err := env.Encode()
	if err != nil {
		return "", err
	}
	id, err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream,
		ID:     "*",
		Values: values,
	}).Result()
	if err != nil {
		return "", fmt.Errorf("queue: XADD to %s: %w", q.stream, err)
	}
	return id, nil
}

// EnsureGroup idempotently creates the consumer group (and the stream, if
// absent). Every worker calls this at startup; concurrent calls are safe.
//
// The group's start ID is 0, not $: outbox drain can race worker startup,
// and entries added before the first worker boots must still be delivered.
func (q *Queue) EnsureGroup(ctx context.Context) error {
	err := q.client.XGroupCreateMkStream(ctx, q.stream, q.group, "0").Err()
	if err != nil && !isBusyGroup(err) {
		return fmt.Errorf("queue: creating group %s on %s: %w", q.group, q.stream, err)
	}
	return nil
}

// isBusyGroup reports whether err is Redis's BUSYGROUP reply — the group
// already exists, which for EnsureGroup is success (and, crucially, leaves
// the existing group's last-delivered-id untouched). go-redis surfaces
// Redis error replies as plain errors, so this is a prefix match on the
// reply code by necessity.
func isBusyGroup(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "BUSYGROUP")
}

// NewConsumerName returns a consumer name unique per process incarnation:
// `<hostname>-<pid>-<random>`. ADR-005 requires incarnation-unique names —
// a restarted worker is a new consumer, so it never replays its own
// pre-crash PEL and recovery flows uniformly through the reclaim path; the
// stranded old name is cleaned up by the janitor (3.4). The hostname/pid
// prefix exists for operators reading XINFO CONSUMERS during an incident;
// only the random suffix carries the uniqueness guarantee.
func NewConsumerName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the platform is broken beyond what a
		// queue library can mitigate.
		panic(fmt.Sprintf("queue: reading random bytes for consumer name: %v", err))
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
