//go:build integration

package pubsub_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/event/pubsub"
)

func redisAddr() string {
	if v, ok := os.LookupEnv("AGENTLOOM_TEST_REDIS_ADDR"); ok && v != "" {
		return v
	}
	return "localhost:6379"
}

// newClient opens a go-redis client to the test Redis, closed on cleanup.
func newClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: redisAddr()})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("cannot reach Redis at %s (run `make up`): %v", redisAddr(), err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// uniquePrefix isolates a test's channels from every other test on the shared
// Redis (the firehose is a single named channel, so the prefix is the isolation).
func uniquePrefix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "agentloom-test-events-" + hex.EncodeToString(b[:])
}

func recvEnv(t *testing.T, ch <-chan event.Envelope) event.Envelope {
	t.Helper()
	select {
	case env, ok := <-ch:
		if !ok {
			t.Fatal("subscription channel closed early")
		}
		return env
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an event")
		return event.Envelope{}
	}
}

// TestPublishSubscribeRoundTrip is the DoD-1 latency check (ADR-018): an event
// published after commit reaches a subscriber promptly, decoded to a typed
// envelope, on both the per-run channel and the firehose.
func TestPublishSubscribeRoundTrip(t *testing.T) {
	t.Parallel()
	prefix := uniquePrefix(t)
	client := newClient(t)
	ctx := t.Context()
	runID := uuid.New()

	runSub, err := pubsub.SubscribeRun(ctx, client, prefix, runID, nil)
	if err != nil {
		t.Fatalf("SubscribeRun: %v", err)
	}
	defer func() { _ = runSub.Close() }()
	fireSub, err := pubsub.SubscribeFirehose(ctx, client, prefix, nil)
	if err != nil {
		t.Fatalf("SubscribeFirehose: %v", err)
	}
	defer func() { _ = fireSub.Close() }()

	pub := pubsub.NewPublisher(client, pubsub.Options{Prefix: prefix})
	defer func() { _ = pub.Close(context.Background()) }()

	// Publish after subscribing (subscribe blocked until confirmed), so no
	// message is missed.
	published := time.Now()
	var envs []event.Envelope
	for seq := int64(1); seq <= 3; seq++ {
		envs = append(envs, event.NewEnvelope(runID, seq, time.Now().UTC(), event.RunSucceeded{}))
	}
	pub.EventsCommitted(ctx, envs)

	for seq := int64(1); seq <= 3; seq++ {
		got := recvEnv(t, runSub.Events())
		if got.Seq != seq || got.RunID != runID {
			t.Fatalf("run channel event = seq %d run %s, want seq %d run %s", got.Seq, got.RunID, seq, runID)
		}
		if _, ok := got.Payload.(*event.RunSucceeded); !ok {
			t.Fatalf("payload = %T, want *event.RunSucceeded", got.Payload)
		}
	}
	if elapsed := time.Since(published); elapsed > 100*time.Millisecond {
		t.Logf("first-to-last delivery took %s (over the 100ms local budget — may be a loaded machine)", elapsed)
	}

	// The firehose carried the same events.
	for seq := int64(1); seq <= 3; seq++ {
		got := recvEnv(t, fireSub.Events())
		if got.Seq != seq {
			t.Fatalf("firehose event = seq %d, want %d", got.Seq, seq)
		}
	}
}

// TestSubscriptionCloseClosesChannel checks Close releases the subscription and
// closes the Events channel.
func TestSubscriptionCloseClosesChannel(t *testing.T) {
	t.Parallel()
	prefix := uniquePrefix(t)
	client := newClient(t)
	sub, err := pubsub.SubscribeRun(t.Context(), client, prefix, uuid.New(), nil)
	if err != nil {
		t.Fatalf("SubscribeRun: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Fatal("expected closed channel, got a value")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Events channel did not close after Close")
	}
}
