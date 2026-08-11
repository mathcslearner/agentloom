// Package queuetest is the chaos harness for code built on internal/queue
// (ticket 3.6): it spawns real consumers with individual kill switches and
// kill-at-phase hooks, scripts handler behaviors (succeed/fail/hang/panic),
// journals every handler invocation, and provides the quiescence assertions
// (stream drained, PEL empty, delayed set empty) that the M4/M5 chaos
// tickets build on.
//
// Each Harness owns uniquely named keys (stream, delayed set, quarantine
// list), so parallel tests — including tests in other packages, which
// `go test` runs as separate processes — never see each other's entries;
// the keys are deleted on cleanup. The connection comes from
// AGENTLOOM_TEST_REDIS_ADDR and defaults to the local compose stack
// (`make up`); like storetest, an unreachable Redis is a failure, not a
// skip — the `integration` build tag is the explicit opt-in.
//
// Time handling follows ADR-005's convention: delayed promotion takes an
// injected now (PromoteDue), while lease expiry rides the Redis server's
// idle clock — chaos tests shrink TTLs (LeaseConfig) rather than faking
// that clock.
package queuetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/queue"
)

// EnvTestRedisAddr overrides the Redis address the harness connects to.
// Deliberately separate from AGENTLOOM_REDIS_ADDR so re-pointing dev tools
// never silently redirects tests.
const EnvTestRedisAddr = "AGENTLOOM_TEST_REDIS_ADDR"

const opTimeout = 30 * time.Second

// Harness is one test's isolated queue world: a client, a uniquely named
// stream + group pair, its delayed-delivery set, and the shared invocation
// Journal that every spawned consumer records into.
type Harness struct {
	tb      testing.TB
	client  *redis.Client
	queue   *queue.Queue
	delayed *queue.Delayed
	journal *Journal
}

// New connects to the test Redis and builds an isolated Harness. Keys are
// prefixed `agentloom-test:<random>:` and deleted on cleanup.
func New(tb testing.TB) *Harness {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	client, err := queue.Open(ctx, redisAddr())
	if err != nil {
		tb.Fatalf("cannot reach Redis (is the dev stack running? try `make up`): %v", err)
	}
	tb.Cleanup(func() { client.Close() }) //nolint:errcheck // best-effort cleanup in tests

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		tb.Fatalf("generating key prefix: %v", err)
	}
	prefix := "agentloom-test:" + hex.EncodeToString(b[:]) + ":"
	q := queue.New(client, prefix+"steps:ready", queue.DefaultGroup)
	d := q.NewDelayed(prefix + "sched:delayed")
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if err := client.Del(ctx, q.Stream(), d.Key(), d.MalformedKey()).Err(); err != nil {
			tb.Errorf("deleting test keys: %v", err)
		}
	})
	return &Harness{tb: tb, client: client, queue: q, delayed: d, journal: &Journal{}}
}

func redisAddr() string {
	if v, ok := os.LookupEnv(EnvTestRedisAddr); ok && v != "" {
		return v
	}
	return "localhost:6379"
}

// Client returns the raw go-redis client for direct stream commands
// (XPENDING, XINFO, raw XADD/ZADD of malformed entries).
func (h *Harness) Client() *redis.Client { return h.client }

// Queue returns the harness's uniquely named stream + group pair.
func (h *Harness) Queue() *queue.Queue { return h.queue }

// Delayed returns the harness's isolated delayed-delivery set. Spawned
// consumers promote from this set by default.
func (h *Harness) Delayed() *queue.Delayed { return h.delayed }

// Journal returns the shared invocation journal every spawned consumer
// records into.
func (h *Harness) Journal() *Journal { return h.journal }

// TestRunID is the fixed run ID Envelope stamps, so envelope equality
// assertions stay trivial.
var TestRunID = uuid.MustParse("7d44e2a8-1f3c-4b9a-8e5d-2c6f0a9b3d71")

// Envelope returns a valid minimal envelope distinguished by step ID —
// the standard fixture for harness-driven tests.
func Envelope(step string) queue.Envelope {
	return queue.Envelope{RunID: TestRunID, StepID: step, Reason: queue.ReasonStepReady}
}

// FastConfig is a consumer config with a short XREADGROUP block, keeping
// shutdown — which is observed between blocking reads and therefore
// bounded by Block — fast in tests. Everything else stays at the ADR-005
// defaults.
func FastConfig() queue.ConsumerConfig {
	return queue.ConsumerConfig{Block: 500 * time.Millisecond}
}

// LeaseConfig tunes the lease machinery to test speeds: the given TTL
// with derived heartbeat (TTL/3) and reclaim (TTL/2) intervals, and a
// short block so duty ticks are observed promptly. ADR-005's convention:
// chaos tests shrink the TTL rather than faking the Redis server clock,
// which is the one clock the protocol consumes without injecting.
func LeaseConfig(ttl time.Duration) queue.ConsumerConfig {
	return queue.ConsumerConfig{Block: 100 * time.Millisecond, LeaseTTL: ttl}
}

// EnsureGroup creates the consumer group, failing the test on error.
func (h *Harness) EnsureGroup(ctx context.Context) {
	h.tb.Helper()
	if err := h.queue.EnsureGroup(ctx); err != nil {
		h.tb.Fatalf("EnsureGroup: %v", err)
	}
}

// Enqueue appends the envelope to the stream, failing the test on error,
// and returns the assigned stream entry ID.
func (h *Harness) Enqueue(ctx context.Context, env queue.Envelope) string {
	h.tb.Helper()
	id, err := h.queue.Enqueue(ctx, env)
	if err != nil {
		h.tb.Fatalf("Enqueue: %v", err)
	}
	return id
}

// EnqueueStep enqueues Envelope(step).
func (h *Harness) EnqueueStep(ctx context.Context, step string) string {
	h.tb.Helper()
	return h.Enqueue(ctx, Envelope(step))
}

// EnqueueRaw XADDs raw field–value pairs the envelope codec did not
// write — the way tests plant malformed entries on the wire.
func (h *Harness) EnqueueRaw(ctx context.Context, values map[string]any) string {
	h.tb.Helper()
	id, err := h.client.XAdd(ctx, &redis.XAddArgs{Stream: h.queue.Stream(), ID: "*", Values: values}).Result()
	if err != nil {
		h.tb.Fatalf("raw XADD: %v", err)
	}
	return id
}

// ReadOne delivers the next fresh entry to the given consumer name via a
// manual XREADGROUP — for tests that drive the group protocol by hand.
func (h *Harness) ReadOne(ctx context.Context, consumer string) redis.XMessage {
	h.tb.Helper()
	streams, err := h.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    h.queue.Group(),
		Consumer: consumer,
		Streams:  []string{h.queue.Stream(), ">"},
		Count:    1,
		Block:    5 * time.Second,
	}).Result()
	if err != nil {
		h.tb.Fatalf("XREADGROUP as %s: %v", consumer, err)
	}
	if len(streams) != 1 || len(streams[0].Messages) != 1 {
		h.tb.Fatalf("XREADGROUP as %s: got %+v, want exactly one message", consumer, streams)
	}
	return streams[0].Messages[0]
}

// Stats returns the stream stats, failing the test on error.
func (h *Harness) Stats(ctx context.Context) queue.StreamStats {
	h.tb.Helper()
	stats, err := h.queue.Stats(ctx)
	if err != nil {
		h.tb.Fatalf("Stats: %v", err)
	}
	return stats
}

// WaitStats polls Stats until cond is satisfied or the deadline passes,
// returning the last snapshot. Redis-side effects (ACKs, PEL changes)
// have no completion signal beyond the commands themselves, so
// observation polls. ErrNoGroup is treated as not-yet-ready, not fatal:
// a spawned consumer creates the group asynchronously inside Run, so a
// poll racing that bootstrap must keep waiting (post-M4 audit — this
// race flaked TestConsumerTrimsAckedEntries under parallel-package CI
// load).
func (h *Harness) WaitStats(ctx context.Context, cond func(queue.StreamStats) bool) queue.StreamStats {
	h.tb.Helper()
	var stats queue.StreamStats
	deadline := time.Now().Add(opTimeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		s, err := h.queue.Stats(ctx)
		if err != nil && !errors.Is(err, queue.ErrNoGroup) {
			h.tb.Fatalf("Stats: %v", err)
		}
		if err == nil {
			stats = s
			if cond(stats) {
				return stats
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.tb.Fatalf("condition never met; last stats %+v", stats)
	return stats
}

// PELSnapshot returns the full PEL for the harness's group.
func (h *Harness) PELSnapshot(ctx context.Context) []redis.XPendingExt {
	h.tb.Helper()
	pending, err := h.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: h.queue.Stream(), Group: h.queue.Group(), Start: "-", End: "+", Count: 100,
	}).Result()
	if err != nil {
		h.tb.Fatalf("XPENDING: %v", err)
	}
	return pending
}

// StreamEnvelopes decodes every entry currently on the stream, in order.
func (h *Harness) StreamEnvelopes(ctx context.Context) []queue.Envelope {
	h.tb.Helper()
	msgs, err := h.client.XRange(ctx, h.queue.Stream(), "-", "+").Result()
	if err != nil {
		h.tb.Fatalf("XRANGE %s: %v", h.queue.Stream(), err)
	}
	envs := make([]queue.Envelope, 0, len(msgs))
	for _, msg := range msgs {
		env, err := queue.DecodeEnvelope(msg.Values)
		if err != nil {
			h.tb.Fatalf("stream entry %s does not decode: %v", msg.ID, err)
		}
		envs = append(envs, env)
	}
	return envs
}

// DelayedLen returns the delayed set's size, failing the test on error.
func (h *Harness) DelayedLen(ctx context.Context) int64 {
	h.tb.Helper()
	n, err := h.delayed.Len(ctx)
	if err != nil {
		h.tb.Fatalf("Delayed.Len: %v", err)
	}
	return n
}

// MalformedMembers returns the quarantine list's raw contents. Quarantine
// is asserted separately from quiescence: a chaos test may deliberately
// plant undecodable members.
func (h *Harness) MalformedMembers(ctx context.Context) []string {
	h.tb.Helper()
	members, err := h.client.LRange(ctx, h.delayed.MalformedKey(), 0, -1).Result()
	if err != nil {
		h.tb.Fatalf("LRANGE %s: %v", h.delayed.MalformedKey(), err)
	}
	return members
}

// PromoteDue runs one promotion pass with the caller's injected now,
// failing the test on error.
func (h *Harness) PromoteDue(ctx context.Context, now time.Time, limit int) queue.PromoteResult {
	h.tb.Helper()
	res, err := h.delayed.PromoteDue(ctx, now, limit)
	if err != nil {
		h.tb.Fatalf("PromoteDue: %v", err)
	}
	return res
}
