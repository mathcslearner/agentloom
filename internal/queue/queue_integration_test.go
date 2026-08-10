//go:build integration

package queue_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/queue"
)

const opTimeout = 30 * time.Second

// redisAddr mirrors the storetest convention: an AGENTLOOM_TEST_* override
// with the compose-stack default, deliberately separate from
// AGENTLOOM_REDIS_ADDR so re-pointing dev tools never silently redirects
// tests.
func redisAddr() string {
	if v, ok := os.LookupEnv("AGENTLOOM_TEST_REDIS_ADDR"); ok && v != "" {
		return v
	}
	return "localhost:6379"
}

// newTestQueue opens a client and returns a Queue on a unique stream key so
// parallel tests — including tests in other packages, which go test runs as
// separate processes — never see each other's entries. The stream key
// (which owns its groups) is deleted on cleanup. Ticket 3.6's queuetest
// harness will absorb this helper.
func newTestQueue(tb testing.TB) *queue.Queue {
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
		tb.Fatalf("generating stream name: %v", err)
	}
	stream := "agentloom-test:" + hex.EncodeToString(b[:]) + ":steps:ready"
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if err := client.Del(ctx, stream).Err(); err != nil {
			tb.Errorf("deleting test stream %s: %v", stream, err)
		}
	})
	return queue.New(client, stream, queue.DefaultGroup)
}

// rawClient reaches the go-redis client behind a test Queue for direct
// stream commands (XREADGROUP, XADD of malformed entries, XINFO).
func rawClient(tb testing.TB) *redis.Client {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	client, err := queue.Open(ctx, redisAddr())
	if err != nil {
		tb.Fatalf("cannot reach Redis (is the dev stack running? try `make up`): %v", err)
	}
	tb.Cleanup(func() { client.Close() }) //nolint:errcheck // best-effort cleanup in tests
	return client
}

// readOne delivers the next fresh entry to the given consumer.
func readOne(ctx context.Context, tb testing.TB, client *redis.Client, q *queue.Queue, consumer string) redis.XMessage {
	tb.Helper()
	streams, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.Group(),
		Consumer: consumer,
		Streams:  []string{q.Stream(), ">"},
		Count:    1,
		Block:    5 * time.Second,
	}).Result()
	if err != nil {
		tb.Fatalf("XREADGROUP as %s: %v", consumer, err)
	}
	if len(streams) != 1 || len(streams[0].Messages) != 1 {
		tb.Fatalf("XREADGROUP as %s: got %+v, want exactly one message", consumer, streams)
	}
	return streams[0].Messages[0]
}

func TestProduceConsumeRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	want := fullEnvelope()
	id, err := q.Enqueue(ctx, want)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("Enqueue returned an empty stream ID")
	}

	msg := readOne(ctx, t, client, q, queue.NewConsumerName())
	if msg.ID != id {
		t.Errorf("delivered ID = %s, want %s", msg.ID, id)
	}
	got, err := queue.DecodeEnvelope(msg.Values)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if got != want {
		t.Errorf("round-trip through Redis = %+v, want %+v", got, want)
	}
}

// TestGroupSeesEntriesEnqueuedBeforeCreation pins EnsureGroup's start ID of
// 0: outbox drain can race worker startup, and entries added before the
// first worker boots must still be delivered.
func TestGroupSeesEntriesEnqueuedBeforeCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	client := rawClient(t)

	want := minimalEnvelope()
	if _, err := q.Enqueue(ctx, want); err != nil {
		t.Fatalf("Enqueue before group creation: %v", err)
	}
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	msg := readOne(ctx, t, client, q, queue.NewConsumerName())
	got, err := queue.DecodeEnvelope(msg.Values)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if got != want {
		t.Errorf("pre-creation entry = %+v, want %+v", got, want)
	}
}

func TestEnsureGroupConcurrent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	client := rawClient(t)

	const racers = 16
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = q.EnsureGroup(ctx)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: EnsureGroup: %v", i, err)
		}
	}

	groups, err := client.XInfoGroups(ctx, q.Stream()).Result()
	if err != nil {
		t.Fatalf("XINFO GROUPS: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != q.Group() {
		t.Errorf("groups = %+v, want exactly one named %s", groups, q.Group())
	}
}

// TestEnsureGroupDoesNotResetExistingGroup pins the BUSYGROUP-is-success
// path: re-ensuring an existing group must leave its last-delivered-id
// untouched, or a worker restart would redeliver the whole stream.
func TestEnsureGroupDoesNotResetExistingGroup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := q.Enqueue(ctx, minimalEnvelope()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	msg := readOne(ctx, t, client, q, queue.NewConsumerName())

	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("re-EnsureGroup: %v", err)
	}
	groups, err := client.XInfoGroups(ctx, q.Stream()).Result()
	if err != nil {
		t.Fatalf("XINFO GROUPS: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want exactly one", groups)
	}
	if groups[0].LastDeliveredID != msg.ID {
		t.Errorf("last-delivered-id = %s after re-ensure, want %s (unchanged)", groups[0].LastDeliveredID, msg.ID)
	}
}

func TestStats(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for range 3 {
		if _, err := q.Enqueue(ctx, minimalEnvelope()); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	stats, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Length != 3 || stats.Pending != 0 || len(stats.Consumers) != 0 {
		t.Errorf("before reads: stats = %+v, want Length 3, Pending 0, no consumers", stats)
	}

	// Deliver two entries to consumer "a" and one to "b" without acking:
	// all three become PEL entries (leases).
	readOne(ctx, t, client, q, "a")
	readOne(ctx, t, client, q, "a")
	readOne(ctx, t, client, q, "b")

	stats, err = q.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats after reads: %v", err)
	}
	if stats.Length != 3 || stats.Pending != 3 {
		t.Errorf("after reads: stats = %+v, want Length 3, Pending 3", stats)
	}
	want := []queue.ConsumerPending{{Name: "a", Pending: 2}, {Name: "b", Pending: 1}}
	if len(stats.Consumers) != len(want) || stats.Consumers[0] != want[0] || stats.Consumers[1] != want[1] {
		t.Errorf("consumer breakdown = %+v, want %+v", stats.Consumers, want)
	}
}

func TestStatsWithoutGroup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	if _, err := q.Enqueue(ctx, minimalEnvelope()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Stats(ctx); !errors.Is(err, queue.ErrNoGroup) {
		t.Errorf("Stats without group: err = %v, want ErrNoGroup", err)
	}
}

// TestDecodeMalformedOnTheWire proves the codec against real go-redis value
// types: raw XADDs that this codec did not write must come back as the
// typed decode errors, which per ADR-005 the consumer answers with no-ACK
// so the delivery count walks the entry into the poison path.
func TestDecodeMalformedOnTheWire(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	client := rawClient(t)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	cases := []struct {
		name   string
		values map[string]any
		check  func(error) bool
	}{
		{
			"unknown version",
			map[string]any{"v": "99", "run_id": minimalEnvelope().RunID.String(), "step_id": "fetch", "reason": "step_ready"},
			func(err error) bool { var e *queue.UnknownVersionError; return errors.As(err, &e) },
		},
		{
			"missing version",
			map[string]any{"run_id": minimalEnvelope().RunID.String(), "step_id": "fetch", "reason": "step_ready"},
			func(err error) bool { var e *queue.MalformedEnvelopeError; return errors.As(err, &e) },
		},
	}
	for _, tc := range cases {
		if err := client.XAdd(ctx, &redis.XAddArgs{Stream: q.Stream(), ID: "*", Values: tc.values}).Err(); err != nil {
			t.Fatalf("%s: raw XADD: %v", tc.name, err)
		}
		msg := readOne(ctx, t, client, q, queue.NewConsumerName())
		_, err := queue.DecodeEnvelope(msg.Values)
		if !errors.Is(err, queue.ErrBadEnvelope) {
			t.Errorf("%s: errors.Is(err, ErrBadEnvelope) = false for %v", tc.name, err)
		}
		if !tc.check(err) {
			t.Errorf("%s: wrong error type: %v", tc.name, err)
		}
	}
}
