package queue_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/queue"
)

func TestConsumerConfigDefaults(t *testing.T) {
	t.Parallel()

	got := queue.ConsumerConfig{}.WithDefaults()
	want := queue.ConsumerConfig{
		Batch:                queue.DefaultConsumerBatch,
		Block:                queue.DefaultConsumerBlock,
		ErrorBackoff:         queue.DefaultErrorBackoff,
		LeaseTTL:             queue.DefaultLeaseTTL,
		HeartbeatInterval:    queue.DefaultLeaseTTL / 3,
		ReclaimInterval:      queue.DefaultLeaseTTL / 2,
		PoisonThreshold:      queue.DefaultPoisonThreshold,
		JanitorInterval:      queue.DefaultJanitorInterval,
		JanitorIdleThreshold: queue.DefaultJanitorIdleThreshold,
		PromoterTick:         queue.DefaultPromoterTick,
		TrimInterval:         queue.DefaultTrimInterval,
	}
	// PoisonHandler makes the struct non-comparable; DeepEqual treats the
	// nil func fields on both sides as equal.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("zero config with defaults = %+v, want %+v", got, want)
	}

	explicit := queue.ConsumerConfig{
		Batch:                4,
		Block:                100 * time.Millisecond,
		ErrorBackoff:         10 * time.Millisecond,
		LeaseTTL:             time.Minute,
		HeartbeatInterval:    5 * time.Second,
		ReclaimInterval:      7 * time.Second,
		PoisonThreshold:      3,
		JanitorInterval:      time.Minute,
		JanitorIdleThreshold: 2 * time.Hour,
		PromoterTick:         100 * time.Millisecond,
		TrimInterval:         30 * time.Second,
		DelayedKey:           "custom:delayed",
	}
	if got := explicit.WithDefaults(); !reflect.DeepEqual(got, explicit) {
		t.Errorf("explicit config with defaults = %+v, want unchanged %+v", got, explicit)
	}
}

// TestConsumerConfigDerivedIntervals pins the ADR-005 ratios: heartbeat and
// reclaim intervals derive from LeaseTTL (TTL/3 and TTL/2) when unset, so a
// TTL-only override keeps two missed beats preceding expiry.
func TestConsumerConfigDerivedIntervals(t *testing.T) {
	t.Parallel()

	got := queue.ConsumerConfig{LeaseTTL: 900 * time.Millisecond}.WithDefaults()
	if got.HeartbeatInterval != 300*time.Millisecond {
		t.Errorf("HeartbeatInterval = %v, want LeaseTTL/3 = 300ms", got.HeartbeatInterval)
	}
	if got.ReclaimInterval != 450*time.Millisecond {
		t.Errorf("ReclaimInterval = %v, want LeaseTTL/2 = 450ms", got.ReclaimInterval)
	}
}

// TestHeartbeatJitterBounds pins the ±20% jitter contract from the ADR-005
// tuning table.
func TestHeartbeatJitterBounds(t *testing.T) {
	t.Parallel()

	const interval = time.Second
	lo, hi := 800*time.Millisecond, 1200*time.Millisecond
	for range 1000 {
		if d := queue.HeartbeatJitter(interval); d < lo || d > hi {
			t.Fatalf("jitter produced %v, want within [%v, %v]", d, lo, hi)
		}
	}
}

func TestSafeHandlePassesThrough(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("handler says no")
	var got queue.Delivery
	h := func(_ context.Context, d queue.Delivery) error {
		got = d
		return sentinel
	}
	d := queue.Delivery{ID: "1-0", Envelope: minimalEnvelope(), DeliveryCount: 3}
	if err := queue.SafeHandle(context.Background(), h, d); !errors.Is(err, sentinel) {
		t.Errorf("SafeHandle error = %v, want %v", err, sentinel)
	}
	if got != d {
		t.Errorf("handler received %+v, want %+v", got, d)
	}

	ok := func(context.Context, queue.Delivery) error { return nil }
	if err := queue.SafeHandle(context.Background(), ok, d); err != nil {
		t.Errorf("SafeHandle with succeeding handler: unexpected error: %v", err)
	}
}

func TestSafeHandleContainsPanic(t *testing.T) {
	t.Parallel()

	h := func(context.Context, queue.Delivery) error { panic("kaboom") }
	err := queue.SafeHandle(context.Background(), h, queue.Delivery{})
	if err == nil {
		t.Fatal("SafeHandle with panicking handler: want error, got nil")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("panic error %q does not carry the panic value", err)
	}
	if !strings.Contains(err.Error(), "consumer_test.go") {
		t.Errorf("panic error %q does not carry a stack trace", err)
	}
}

func TestNewConsumerGeneratesUniqueNames(t *testing.T) {
	t.Parallel()

	q := queue.New(nil, "", "")
	h := func(context.Context, queue.Delivery) error { return nil }
	a, b := q.NewConsumer("", h, queue.ConsumerConfig{}), q.NewConsumer("", h, queue.ConsumerConfig{})
	if a.Name() == "" || a.Name() == b.Name() {
		t.Errorf("generated names %q and %q, want non-empty and distinct", a.Name(), b.Name())
	}
	if c := q.NewConsumer("keep-me", h, queue.ConsumerConfig{}); c.Name() != "keep-me" {
		t.Errorf("explicit name = %q, want %q", c.Name(), "keep-me")
	}
}

func TestNewConsumerNilHandlerPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("NewConsumer with nil handler: want panic, got none")
		}
	}()
	queue.New(nil, "", "").NewConsumer("", nil, queue.ConsumerConfig{})
}

// TestPhaseHookPreHandleAborts pins the hook contract for the chaos
// harness: an erroring PhaseHook at pre-handle aborts the message before
// the handler (and before any Redis command — provable here with a nil
// client), leaving the entry un-acked. The pre-ack sibling needs a real
// PEL and lives in the queuetest integration self-tests.
func TestPhaseHookPreHandleAborts(t *testing.T) {
	t.Parallel()

	values, err := minimalEnvelope().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	handled := false
	var gotPhase queue.Phase
	cfg := queue.ConsumerConfig{
		PhaseHook: func(phase queue.Phase, d queue.Delivery) error {
			gotPhase = phase
			if d.Envelope != minimalEnvelope() {
				t.Errorf("hook delivery envelope = %+v, want %+v", d.Envelope, minimalEnvelope())
			}
			return errors.New("simulated crash")
		},
	}
	c := queue.New(nil, "", "").NewConsumer("", func(context.Context, queue.Delivery) error {
		handled = true
		return nil
	}, cfg)
	if acked := c.Process(context.Background(), redis.XMessage{ID: "1-1", Values: values}, 1); acked {
		t.Error("Process reported an ack despite the pre-handle abort")
	}
	if handled {
		t.Error("handler ran despite the pre-handle abort")
	}
	if gotPhase != queue.PhasePreHandle {
		t.Errorf("hook phase = %v, want %v", gotPhase, queue.PhasePreHandle)
	}
}

func TestPhaseString(t *testing.T) {
	t.Parallel()

	cases := map[queue.Phase]string{
		queue.PhasePreHandle: "pre-handle",
		queue.PhasePreAck:    "pre-ack",
		queue.Phase(42):      "phase(42)",
	}
	for phase, want := range cases {
		if got := phase.String(); got != want {
			t.Errorf("Phase(%d).String() = %q, want %q", int(phase), got, want)
		}
	}
}
