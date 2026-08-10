package queuetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
)

// Action is one scripted handler behavior.
type Action struct {
	kind     actionKind
	err      error
	panicMsg string
}

type actionKind int

const (
	actionSucceed actionKind = iota
	actionFail
	actionPanic
	actionHang
)

// Succeed returns nil from the handler: the entry acks.
func Succeed() Action { return Action{kind: actionSucceed} }

// Fail returns err from the handler (a default error when nil): the entry
// stays pending for reclaim.
func Fail(err error) Action {
	if err == nil {
		err = errors.New("queuetest: scripted failure")
	}
	return Action{kind: actionFail, err: err}
}

// PanicWith panics in the handler — the consumer's containment turns it
// into an error, so the entry stays pending.
func PanicWith(msg string) Action { return Action{kind: actionPanic, panicMsg: msg} }

// Hang blocks until the step is Released (then succeeds) or the handler
// context is canceled (then returns ctx.Err(), a failure). The building
// block for kill-mid-handle scenarios and long-running-task simulations.
func Hang() Action { return Action{kind: actionHang} }

// Script maps step IDs to behavior sequences; its Handle method is a
// queue.Handler. Each invocation of a step consumes one action from its
// sequence; when the sequence is exhausted the last action repeats, so
// OnStep("x", Fail(nil)) fails forever and OnStep("x", Fail(nil),
// Succeed()) walks a fail-then-succeed ladder. Steps without a sequence
// run the Default action (Succeed unless overridden). Safe for concurrent
// use by many consumers.
type Script struct {
	mu       sync.Mutex
	steps    map[string]*sequence
	fallback Action
	releases map[string]chan struct{}
}

type sequence struct {
	actions []Action
	next    int
}

// NewScript returns a Script whose default action is Succeed.
func NewScript() *Script {
	return &Script{
		steps:    make(map[string]*sequence),
		fallback: Succeed(),
		releases: make(map[string]chan struct{}),
	}
}

// OnStep scripts the step's behavior sequence. Chainable.
func (s *Script) OnStep(step string, actions ...Action) *Script {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps[step] = &sequence{actions: actions}
	return s
}

// Default sets the action for steps without a scripted sequence.
// Chainable.
func (s *Script) Default(a Action) *Script {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallback = a
	return s
}

// Release unblocks current and future Hang actions for the step.
func (s *Script) Release(step string) {
	ch := s.releaseCh(step)
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// releaseCh get-or-creates the step's release channel. Release closes it,
// which is why a Release before the Hang even starts also unblocks it.
func (s *Script) releaseCh(step string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.releases[step]
	if !ok {
		ch = make(chan struct{})
		s.releases[step] = ch
	}
	return ch
}

// take consumes the step's next action.
func (s *Script) take(step string) Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.steps[step]
	if !ok || len(seq.actions) == 0 {
		return s.fallback
	}
	a := seq.actions[seq.next]
	if seq.next < len(seq.actions)-1 {
		seq.next++
	}
	return a
}

// Handle is a queue.Handler executing the scripted action for the
// delivery's step.
func (s *Script) Handle(ctx context.Context, d queue.Delivery) error {
	a := s.take(d.Envelope.StepID)
	switch a.kind {
	case actionSucceed:
		return nil
	case actionFail:
		return a.err
	case actionPanic:
		panic(a.panicMsg)
	case actionHang:
		select {
		case <-s.releaseCh(d.Envelope.StepID):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	default:
		panic(fmt.Sprintf("queuetest: unknown action kind %d", a.kind))
	}
}

// Invocation is one handler invocation as recorded by the Journal. End is
// zero while the handler is still running; Err is the handler's result
// once it returns (nil = success, about to ack).
type Invocation struct {
	Consumer      string
	EntryID       string
	Step          string
	DeliveryCount int64
	Start         time.Time
	End           time.Time
	Err           error
	Panicked      bool
}

// Journal is a thread-safe record of every handler invocation across all
// consumers spawned from one Harness — the raw material for the
// delivered-exactly-once-per-claim assertion and for chaos post-mortems.
type Journal struct {
	mu   sync.Mutex
	invs []*Invocation
}

// Invocations returns a snapshot copy, in invocation-start order.
func (j *Journal) Invocations() []Invocation {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Invocation, len(j.invs))
	for i, inv := range j.invs {
		out[i] = *inv
	}
	return out
}

// wrap returns a handler that records every invocation of next, including
// panics (recorded, then re-raised so the consumer's containment stays
// the behavior under test).
func (j *Journal) wrap(consumer string, next queue.Handler) queue.Handler {
	return func(ctx context.Context, d queue.Delivery) (err error) {
		inv := &Invocation{
			Consumer:      consumer,
			EntryID:       d.ID,
			Step:          d.Envelope.StepID,
			DeliveryCount: d.DeliveryCount,
			Start:         time.Now(),
		}
		j.mu.Lock()
		j.invs = append(j.invs, inv)
		j.mu.Unlock()
		defer func() {
			r := recover()
			j.mu.Lock()
			inv.End = time.Now()
			inv.Err = err
			if r != nil {
				inv.Panicked = true
				inv.Err = fmt.Errorf("queuetest: handler panic: %v", r)
			}
			j.mu.Unlock()
			if r != nil {
				panic(r)
			}
		}()
		return next(ctx, d)
	}
}

// duplicateClaims reports entries handled more than once within a single
// claim — two invocations sharing (entry ID, delivery count). Each
// delivery count is one claim, so re-handling across claims is
// at-least-once working as designed; a duplicate within a claim would
// mean a consumer double-ran one lease.
func (j *Journal) duplicateClaims() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	type claimKey struct {
		entryID string
		count   int64
	}
	seen := make(map[claimKey]*Invocation, len(j.invs))
	var dups []string
	for _, inv := range j.invs {
		k := claimKey{entryID: inv.EntryID, count: inv.DeliveryCount}
		if prev, ok := seen[k]; ok {
			dups = append(dups, fmt.Sprintf("entry %s delivery %d handled by both %s and %s",
				inv.EntryID, inv.DeliveryCount, prev.Consumer, inv.Consumer))
			continue
		}
		seen[k] = inv
	}
	return dups
}
