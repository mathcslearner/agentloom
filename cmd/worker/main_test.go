package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/config"
	"github.com/mathcslearner/agentloom/internal/queue"
)

// TestConsumerConfigMapping pins the config → consumer field mapping: a
// QueueConfig with every field set to a distinct value must land on the
// matching ConsumerConfig fields, with nothing dropped or swapped.
func TestConsumerConfigMapping(t *testing.T) {
	t.Parallel()

	in := config.QueueConfig{
		DelayedKey:           "sched:delayed:mapped",
		ConsumerBatch:        7,
		ConsumerBlock:        1 * time.Second,
		LeaseTTL:             2 * time.Second,
		HeartbeatInterval:    3 * time.Second,
		ReclaimInterval:      4 * time.Second,
		PoisonThreshold:      9,
		JanitorInterval:      5 * time.Second,
		JanitorIdleThreshold: 6 * time.Second,
		PromoterTick:         7 * time.Second,
		TrimInterval:         8 * time.Second,
	}
	want := queue.ConsumerConfig{
		DelayedKey:           "sched:delayed:mapped",
		Batch:                7,
		Block:                1 * time.Second,
		LeaseTTL:             2 * time.Second,
		HeartbeatInterval:    3 * time.Second,
		ReclaimInterval:      4 * time.Second,
		PoisonThreshold:      9,
		JanitorInterval:      5 * time.Second,
		JanitorIdleThreshold: 6 * time.Second,
		PromoterTick:         7 * time.Second,
		TrimInterval:         8 * time.Second,
	}
	poison := func(context.Context, queue.PoisonMessage) error { return nil }
	got := consumerConfig(in, poison)
	// ConsumerConfig carries two non-comparable fields (PoisonHandler,
	// PhaseHook): the poison handler must pass through (ticket 5.4 wires
	// the engine's dead-lettering here), the phase hook must stay nil.
	if got.PoisonHandler == nil {
		t.Error("PoisonHandler is nil; the mapping must pass the engine's dead-lettering handler through")
	}
	if got.PhaseHook != nil {
		t.Error("PhaseHook is set; it is test instrumentation, never production config")
	}
	got.PoisonHandler = nil
	got.PhaseHook = nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("consumerConfig(%+v)\n got %+v\nwant %+v", in, got, want)
	}
}
