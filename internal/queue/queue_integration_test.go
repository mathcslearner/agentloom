//go:build integration

package queue_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
)

const opTimeout = 30 * time.Second

func TestProduceConsumeRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	want := fullEnvelope()
	id := h.Enqueue(ctx, want)
	if id == "" {
		t.Fatal("Enqueue returned an empty stream ID")
	}

	msg := h.ReadOne(ctx, queue.NewConsumerName())
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

	h := queuetest.New(t)

	want := minimalEnvelope()
	h.Enqueue(ctx, want)
	h.EnsureGroup(ctx)

	msg := h.ReadOne(ctx, queue.NewConsumerName())
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

	h := queuetest.New(t)
	q := h.Queue()

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

	groups, err := h.Client().XInfoGroups(ctx, q.Stream()).Result()
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

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	h.Enqueue(ctx, minimalEnvelope())
	msg := h.ReadOne(ctx, queue.NewConsumerName())

	h.EnsureGroup(ctx)
	groups, err := h.Client().XInfoGroups(ctx, h.Queue().Stream()).Result()
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

	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	for range 3 {
		h.Enqueue(ctx, minimalEnvelope())
	}

	stats := h.Stats(ctx)
	if stats.Length != 3 || stats.Pending != 0 || len(stats.Consumers) != 0 {
		t.Errorf("before reads: stats = %+v, want Length 3, Pending 0, no consumers", stats)
	}

	// Deliver two entries to consumer "a" and one to "b" without acking:
	// all three become PEL entries (leases).
	h.ReadOne(ctx, "a")
	h.ReadOne(ctx, "a")
	h.ReadOne(ctx, "b")

	stats = h.Stats(ctx)
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

	h := queuetest.New(t)
	h.Enqueue(ctx, minimalEnvelope())
	if _, err := h.Queue().Stats(ctx); !errors.Is(err, queue.ErrNoGroup) {
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

	h := queuetest.New(t)
	h.EnsureGroup(ctx)

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
		h.EnqueueRaw(ctx, tc.values)
		msg := h.ReadOne(ctx, queue.NewConsumerName())
		_, err := queue.DecodeEnvelope(msg.Values)
		if !errors.Is(err, queue.ErrBadEnvelope) {
			t.Errorf("%s: errors.Is(err, ErrBadEnvelope) = false for %v", tc.name, err)
		}
		if !tc.check(err) {
			t.Errorf("%s: wrong error type: %v", tc.name, err)
		}
	}
}
