package engine

import (
	"errors"
	"time"

	"github.com/mathcslearner/agentloom/internal/store"
)

// Control is the run-lifecycle control surface — Cancel, Park, Unpark,
// Requeue (tickets 5.4/5.6, exposed via the API by 6.5). The ops are pure
// store transactions plus an optional post-commit dispatcher nudge, so
// Control needs neither an executor registry nor a queue: the API server
// builds one with NewControl (no nudge — its outbox rows are drained on the
// worker fleet's dispatch cadence, per ADR-002), while every worker's
// Engine embeds one wired to its own dispatcher.
type Control struct {
	store *store.Store
	// now is the injected clock stamped onto the control transitions.
	now func() time.Time
	// nudge, when set, is called after Unpark/Requeue commit outbox rows —
	// the dispatcher wake-up seam. Nil means dispatch waits for the drain
	// cadence (correct, just slower).
	nudge func()
}

// ControlOption customizes a Control built by NewControl.
type ControlOption func(*Control)

// WithControlClock overrides the control's clock (project invariant: time
// is injectable).
func WithControlClock(now func() time.Time) ControlOption {
	return func(c *Control) { c.now = now }
}

// NewControl builds a standalone run-lifecycle control surface over the
// store — the API server's constructor. Workers get theirs embedded in
// Engine (New wires the engine's clock and nudge through).
func NewControl(s *store.Store, opts ...ControlOption) (*Control, error) {
	if s == nil {
		return nil, errors.New("engine: NewControl requires a store")
	}
	c := &Control{store: s, now: time.Now}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// ErrRunNotRequeueable is the Requeue refusal for cancelling/cancelled
// runs: a cancel is terminal by operator intent (ticket 5.6), so requeueing
// a dead-lettered step of one would strand it — the claim path refuses the
// run — or silently undo the cancel. Test with errors.Is.
var ErrRunNotRequeueable = errors.New("cancelled runs are not requeueable")
