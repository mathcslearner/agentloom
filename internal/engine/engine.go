// Package engine is the worker's execution pipeline (ADR-002): the code
// that turns a queue delivery into durable state transitions against
// Postgres. Ticket 4.2 provides the claim path — a queue.Handler that
// attempts the guarded CAS ready → running and applies ADR-005's ACK
// discipline to every outcome; the execute-and-complete transaction
// (4.3), the outbox dispatcher and reconciler (4.4), and fencing
// enforcement on completion (4.5) build on it.
//
// The engine is transport-agnostic on purpose: it knows deliveries and
// the store, never Redis. Which entries redeliver, heartbeat, or expire
// is internal/queue's business; the engine only decides, per delivery,
// whether consuming it succeeded (ACK), is provably unnecessary (ACK and
// drop), or must be retried (no ACK).
package engine

import (
	"errors"
	"time"

	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store"
)

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
	// now is the injected clock stamped onto claim transitions.
	now func() time.Time
}

// Option customizes an Engine.
type Option func(*Engine)

// WithClock overrides the engine's clock (project invariant: time is
// injectable; tests pass a fixed clock).
func WithClock(now func() time.Time) Option {
	return func(e *Engine) { e.now = now }
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
	e := &Engine{store: s, registry: r, workerID: workerID, now: time.Now}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}
