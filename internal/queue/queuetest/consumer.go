package queuetest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
)

// ConsumerHandle is one spawned consumer with its individual kill switch.
type ConsumerHandle struct {
	h      *Harness
	name   string
	cancel context.CancelFunc
	done   chan error

	stopOnce sync.Once
	stopErr  error

	mu         sync.Mutex
	arms       map[armKey]bool
	killed     chan struct{}
	killedOnce sync.Once
}

// armKey identifies one armed kill: a phase, and the step it targets ("" =
// any step).
type armKey struct {
	phase queue.Phase
	step  string
}

// Spawn starts a consumer running the given handler against the harness's
// queue, returning its handle. The handler is wrapped so every invocation
// lands in the harness's Journal (a panic is recorded, then re-raised for
// the consumer's containment to handle).
//
// An empty cfg.DelayedKey defaults to the harness's isolated delayed set,
// never the global sched:delayed — a spawned consumer's promoter duty must
// not cross test boundaries. cfg.PhaseHook is owned by the handle's
// kill-at-phase machinery; a hook the caller set runs after the kill
// check. An empty name gets a fresh incarnation-unique consumer name.
func (h *Harness) Spawn(name string, handler queue.Handler, cfg queue.ConsumerConfig) *ConsumerHandle {
	h.tb.Helper()
	if handler == nil {
		h.tb.Fatal("Spawn requires a handler")
	}
	if name == "" {
		name = queue.NewConsumerName()
	}
	if cfg.DelayedKey == "" {
		cfg.DelayedKey = h.delayed.Key()
	}
	handle := &ConsumerHandle{
		h:      h,
		name:   name,
		done:   make(chan error, 1),
		arms:   make(map[armKey]bool),
		killed: make(chan struct{}),
	}
	userHook := cfg.PhaseHook
	cfg.PhaseHook = func(phase queue.Phase, d queue.Delivery) error {
		if err := handle.killCheck(phase, d); err != nil {
			return err
		}
		if userHook != nil {
			return userHook(phase, d)
		}
		return nil
	}
	c := h.queue.NewConsumer(name, h.journal.wrap(name, handler), cfg)
	ctx, cancel := context.WithCancel(context.Background())
	handle.cancel = cancel
	go func() { handle.done <- c.Run(ctx) }()
	h.tb.Cleanup(func() { handle.Kill() }) //nolint:errcheck // shutdown errors checked by tests that care
	return handle
}

// SpawnScript spawns a consumer whose handler executes the Script.
func (h *Harness) SpawnScript(name string, s *Script, cfg queue.ConsumerConfig) *ConsumerHandle {
	h.tb.Helper()
	return h.Spawn(name, s.Handle, cfg)
}

// Name returns the consumer-group member name this consumer reads as.
func (c *ConsumerHandle) Name() string { return c.name }

// KillAt arms a one-shot simulated crash: when this consumer reaches the
// given phase for an entry whose step matches (empty step = any entry),
// processing aborts un-acked and the consumer's context is canceled, so
// the loop exits exactly as if the process had died at that point — with
// PhasePreAck, after the handler's work was done. Arm before the target
// entry can be delivered. Killed() signals the trigger; Kill() (or the
// registered cleanup) joins the loop goroutine afterwards.
func (c *ConsumerHandle) KillAt(phase queue.Phase, step string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arms[armKey{phase: phase, step: step}] = true
}

// Killed is closed when an armed KillAt fires.
func (c *ConsumerHandle) Killed() <-chan struct{} { return c.killed }

// killCheck is the PhaseHook: it fires (and disarms) a matching armed
// kill, cancelling the consumer so the loop dies with the abort.
func (c *ConsumerHandle) killCheck(phase queue.Phase, d queue.Delivery) error {
	c.mu.Lock()
	stepKey := armKey{phase: phase, step: d.Envelope.StepID}
	anyKey := armKey{phase: phase, step: ""}
	armed := c.arms[stepKey] || c.arms[anyKey]
	if armed {
		delete(c.arms, stepKey)
		delete(c.arms, anyKey)
	}
	c.mu.Unlock()
	if !armed {
		return nil
	}
	c.killedOnce.Do(func() { close(c.killed) })
	c.cancel()
	return fmt.Errorf("queuetest: simulated crash of %s at %s (step %s)", c.name, phase, d.Envelope.StepID)
}

// Kill cancels the consumer and waits for Run to return, returning Run's
// error. Idempotent, and registered as test cleanup, so tests that never
// call it still join the loop goroutine. A consumer already dead from a
// fired KillAt just gets collected.
func (c *ConsumerHandle) Kill() error {
	c.stopOnce.Do(func() {
		c.cancel()
		select {
		case c.stopErr = <-c.done:
		case <-time.After(opTimeout):
			c.h.tb.Fatalf("consumer %s did not stop within %v", c.name, opTimeout)
		}
	})
	return c.stopErr
}
