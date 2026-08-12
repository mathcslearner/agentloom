// Package engine is the worker's execution pipeline (ADR-002): the code
// that turns a queue delivery into durable state transitions against
// Postgres. Ticket 4.2 provides the claim path — a queue.Handler that
// attempts the guarded CAS ready → running and applies ADR-005's ACK
// discipline to every outcome. Ticket 4.3 provides the tail: the executor
// runs and its result settles in one completion transaction (output, the
// claim-fenced terminal CAS, edge resolution, readiness fan-out, outbox
// rows, run rollup), with the ACK after commit. The outbox dispatcher and
// reconciler (4.4) and fencing enforcement (4.5) build on these.
//
// The engine is transport-agnostic on purpose: it knows deliveries and
// the store, never Redis. Which entries redeliver, heartbeat, or expire
// is internal/queue's business; the engine only decides, per delivery,
// whether consuming it succeeded (ACK), is provably unnecessary (ACK and
// drop), or must be retried (no ACK).
package engine

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
)

// RetryScheduler is the engine's seam onto delayed delivery (ticket 5.2)
// — satisfied by *queue.Delayed. After a retry-routing completion commits,
// the engine schedules the re-dispatch envelope through it; a failure (or
// a crash before the call) is deliberately survivable, healed by the
// reconciler's overdue-retrying scan, which is also why tests inject a
// failing implementation to provoke that gap.
type RetryScheduler interface {
	Schedule(ctx context.Context, env queue.Envelope, fireAt time.Time) error
}

// Engine executes claimed steps for one worker process. It is safe for
// concurrent Handle calls (every field is read-only after New), though
// v0's worker runs a single serialized consumer loop.
type Engine struct {
	store    *store.Store
	registry *exec.Registry
	// workerID identifies this worker in logs (canonical worker_id field).
	// By convention it is the queue consumer name, so log lines join
	// against XINFO CONSUMERS output during an incident.
	workerID string
	// now is the injected clock stamped onto claim and completion
	// transitions.
	now func() time.Time
	// nudge, when set, is called after a completion transaction commits
	// with newly-ready steps in the outbox — the "nudge the dispatcher"
	// seam. The outbox drain loop (ticket 4.4) wires its wake channel
	// here; nil means no one to nudge (drain cadence alone dispatches).
	nudge func()
	// scheduler, when set, receives the delayed retry envelope after a
	// retry-routing completion commits (ticket 5.2). Nil means no delayed
	// scheduling — retries then re-dispatch only via the reconciler's
	// overdue-retrying heal (degraded latency, same convergence).
	scheduler RetryScheduler
	// jitterRand supplies the [0,1) draw for full-jitter backoff.
	jitterRand func() float64
	// effects is the side-effect journal (ticket 5.5), bound per claimed
	// step and handed to executors through StepContext.Effects. Built in
	// New over the same store and clock the engine uses.
	effects *effects.Journal
	// effectsStrict makes journal misuse panic (dev/test loudness) instead
	// of dead-lettering the step; see config.WorkerConfig.EffectsStrict.
	effectsStrict bool
	// cancelPollInterval is how often the in-flight cancellation watch
	// polls the run's status while an executor runs (ticket 5.6): the
	// latency bound on "in-flight executors get context cancellation".
	// Zero disables the watch — correctness is unaffected (the completion
	// transaction re-checks under the run lock); only the executor then
	// runs to its own end before the cancel settles.
	cancelPollInterval time.Duration
}

// Option customizes an Engine.
type Option func(*Engine)

// WithClock overrides the engine's clock (project invariant: time is
// injectable; tests pass a fixed clock).
func WithClock(now func() time.Time) Option {
	return func(e *Engine) { e.now = now }
}

// WithDispatchNudge sets the function called after a completion
// transaction commits having outboxed newly-ready steps. Ticket 4.4's
// outbox drain loop registers its wake signal here so fan-out latency is
// one nudge, not one poll interval. The function must be safe for
// concurrent calls and must not block.
func WithDispatchNudge(nudge func()) Option {
	return func(e *Engine) { e.nudge = nudge }
}

// WithRetryScheduler sets the delayed-delivery producer retry-routing
// completions schedule their re-dispatch through — wire the queue's
// Delayed handle here (cmd/worker does). Nil is legal: retries still
// commit durably and the reconciler re-dispatches them, just on its sweep
// cadence instead of on time.
func WithRetryScheduler(s RetryScheduler) Option {
	return func(e *Engine) { e.scheduler = s }
}

// WithJitterRand overrides the [0,1) source full-jitter backoff draws
// from (project invariant: nondeterminism is injectable; timing tests pin
// it or use jitter "none").
func WithJitterRand(r func() float64) Option {
	return func(e *Engine) { e.jitterRand = r }
}

// WithStrictEffects sets the side-effect journal's misuse behavior (ticket
// 5.5): strict panics — the loud dev/test mode, riding the consumer's
// panic path into poison dead-lettering — while non-strict dead-letters
// the step cleanly with a permanent-classified error. cmd/worker wires
// config.WorkerConfig.EffectsStrict here.
func WithStrictEffects(strict bool) Option {
	return func(e *Engine) { e.effectsStrict = strict }
}

// WithCancelPollInterval sets how often the in-flight cancellation watch
// polls the run's status during executor invocations (ticket 5.6) —
// cmd/worker wires config.WorkerConfig.CancelPollInterval here. Zero
// disables the watch (executors then run to their own end; the completion
// transaction still settles the cancel).
func WithCancelPollInterval(d time.Duration) Option {
	return func(e *Engine) { e.cancelPollInterval = d }
}

// New builds an Engine over the given store and executor registry.
// workerID may be empty (logs then carry an empty worker_id — the queue
// consumer name is the conventional value).
func New(s *store.Store, r *exec.Registry, workerID string, opts ...Option) (*Engine, error) {
	if s == nil {
		return nil, errors.New("engine: New requires a store")
	}
	if r == nil {
		return nil, errors.New("engine: New requires an executor registry")
	}
	e := &Engine{store: s, registry: r, workerID: workerID, now: time.Now, jitterRand: rand.Float64}
	for _, opt := range opts {
		opt(e)
	}
	// Built after the options so the journal shares whatever clock the
	// engine ended up with (tests inject fixed clocks through WithClock).
	j, err := effects.New(s, effects.WithClock(func() time.Time { return e.now() }), effects.WithStrict(e.effectsStrict))
	if err != nil {
		return nil, err
	}
	e.effects = j
	return e, nil
}
