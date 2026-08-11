package engine

// The reconciler (ticket 4.4): the periodic healer closing every
// Postgres→Redis dual-write gap the outbox alone cannot (ADR-002). Each
// sweep runs in one transaction under a fleet-wide advisory lock: steps
// stuck in ready past a threshold with no pending outbox row get a fresh
// row (reason reconcile_ready) — the lost-dispatch heal for ADR-005 crash
// cells P2 and R1(a) — and impossible states are flagged loudly: running
// steps staler than a threshold ≫ lease TTL (R1(c) dead-worker suspects;
// flag-only until 4.5's lease-expiry takeover makes a re-outbox useful)
// and runs still running with no live step (an invariant violation).
//
// Rate-bounding is layered: the advisory lock admits one sweep fleet-wide
// at a time, the interval is jittered so worker fleets do not align, each
// sweep is capped at Limit rows, and the anti-join in the stale-ready scan
// makes sweeps idempotent — a re-outboxed step stops matching until its
// row is drained, so a stuck step costs at most one duplicate dispatch
// per drain-plus-threshold period, never one per sweep.

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
)

// ReconcilerConfig tunes one Reconciler.
type ReconcilerConfig struct {
	// Interval is the base period between sweeps (each waits Interval
	// ±20% jitter). Must be positive.
	Interval time.Duration
	// ReadyStale is how long a step may sit in ready before its dispatch
	// is presumed lost and re-outboxed. Must comfortably exceed the drain
	// interval, or healthy steps get duplicate dispatches (safe, wasteful).
	// Must be positive.
	ReadyStale time.Duration
	// RunningStale is how long a step may sit in running before it is
	// flagged as a dead-worker suspect. Must be ≫ the lease TTL:
	// updated_at moves on transitions, not heartbeats. Must be positive.
	RunningStale time.Duration
	// Limit caps each scan's rows per sweep. Must be positive.
	Limit int
}

// Reconciler heals crash gaps from durable state. One per worker process;
// the advisory sweep lock makes any number of them fleet-safe.
type Reconciler struct {
	store *store.Store
	cfg   ReconcilerConfig
	now   func() time.Time
	// nudge, when set, wakes the dispatcher after a sweep wrote outbox
	// rows, so recovery latency is one nudge rather than one drain tick.
	nudge func()
}

// ReconcilerOption customizes a Reconciler.
type ReconcilerOption func(*Reconciler)

// WithReconcilerClock overrides the reconciler's clock (project
// invariant: time is injectable; staleness tests pass a fixed clock).
func WithReconcilerClock(now func() time.Time) ReconcilerOption {
	return func(r *Reconciler) { r.now = now }
}

// WithReconcilerNudge sets the function called after a sweep that wrote
// outbox rows — wire the dispatcher's Nudge here. Must be safe for
// concurrent calls and must not block.
func WithReconcilerNudge(nudge func()) ReconcilerOption {
	return func(r *Reconciler) { r.nudge = nudge }
}

// NewReconciler builds a Reconciler over the store.
func NewReconciler(s *store.Store, cfg ReconcilerConfig, opts ...ReconcilerOption) (*Reconciler, error) {
	if s == nil {
		return nil, errors.New("engine: NewReconciler requires a store")
	}
	if cfg.Interval <= 0 {
		return nil, errors.New("engine: NewReconciler requires a positive Interval")
	}
	if cfg.ReadyStale <= 0 {
		return nil, errors.New("engine: NewReconciler requires a positive ReadyStale")
	}
	if cfg.RunningStale <= 0 {
		return nil, errors.New("engine: NewReconciler requires a positive RunningStale")
	}
	if cfg.Limit <= 0 {
		return nil, errors.New("engine: NewReconciler requires a positive Limit")
	}
	r := &Reconciler{store: s, cfg: cfg, now: time.Now}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// ReconcileResult is what one sweep found and did.
type ReconcileResult struct {
	// Skipped: another worker held the sweep lock; nothing was scanned.
	Skipped bool
	// Requeued lists the stuck-ready steps this sweep re-outboxed.
	Requeued []store.StepRef
	// StaleRunning lists dead-worker suspects (flagged, not healed).
	StaleRunning []store.StaleRunningStep
	// StalledRuns lists runs in an impossible state (flagged, untouched).
	StalledRuns []uuid.UUID
	// LimitHit: some scan returned exactly Limit rows — there may be more;
	// the next sweep continues (no silent cap).
	LimitHit bool
}

// Run sweeps until ctx is canceled. Errors are logged and retried on the
// next jittered tick.
func (r *Reconciler) Run(ctx context.Context) {
	timer := time.NewTimer(r.jittered())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := r.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			log.From(ctx).WarnContext(ctx, "reconciler sweep failed; retrying next tick",
				slog.Any("error", err))
		}
		timer.Reset(r.jittered())
	}
}

// jittered spreads sweeps Interval ±20% so worker fleets do not align
// their sweeps (the lock makes alignment safe, but spread keeps worst-case
// heal latency near Interval instead of stacking skipped sweeps).
func (r *Reconciler) jittered() time.Duration {
	return time.Duration(float64(r.cfg.Interval) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // jitter, not cryptography
}

// ReconcileOnce runs one sweep transaction and, post-commit, logs what it
// found and nudges the dispatcher if it wrote outbox rows.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult
	now := r.now()
	limit := int32(r.cfg.Limit) //nolint:gosec // Limit is a small validated positive
	err := r.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		locked, err := store.TryReconcileLock(ctx, q)
		if err != nil {
			return err
		}
		if !locked {
			res.Skipped = true
			return nil
		}
		stale, err := store.ListStaleReadySteps(ctx, q, now.Add(-r.cfg.ReadyStale), limit)
		if err != nil {
			return err
		}
		for _, ref := range stale {
			if _, err := q.Outbox().Create(ctx, ref.RunID, ref.StepID, store.OutboxReasonReconcileReady); err != nil {
				return err
			}
		}
		res.Requeued = stale
		res.StaleRunning, err = store.ListStaleRunningSteps(ctx, q, now.Add(-r.cfg.RunningStale), limit)
		if err != nil {
			return err
		}
		res.StalledRuns, err = store.ListStalledRuns(ctx, q, limit)
		if err != nil {
			return err
		}
		res.LimitHit = len(res.Requeued) == int(limit) ||
			len(res.StaleRunning) == int(limit) || len(res.StalledRuns) == int(limit)
		return nil
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	if res.Skipped {
		return res, nil
	}

	logger := log.From(ctx)
	for _, ref := range res.Requeued {
		logger.WarnContext(ctx, "reconciler re-outboxed a stuck-ready step (lost dispatch)",
			log.RunID(ref.RunID.String()), log.StepID(ref.StepID),
			slog.Time("stuck_since", ref.UpdatedAt))
	}
	for _, st := range res.StaleRunning {
		logger.WarnContext(ctx, "step running past staleness threshold — dead-worker suspect (takeover lands in ticket 4.5)",
			log.RunID(st.RunID.String()), log.StepID(st.StepID),
			slog.Time("running_since", st.UpdatedAt),
			slog.String("claim_id", claimIDRefString(st.ClaimID)))
	}
	for _, runID := range res.StalledRuns {
		logger.ErrorContext(ctx, "run running with no live step — impossible state, investigate",
			log.RunID(runID.String()))
	}
	if res.LimitHit {
		logger.WarnContext(ctx, "reconciler sweep hit its row limit; more may remain for the next sweep",
			slog.Int("limit", r.cfg.Limit))
	}
	if len(res.Requeued) > 0 || len(res.StaleRunning) > 0 || len(res.StalledRuns) > 0 {
		logger.InfoContext(ctx, "reconciler sweep complete",
			slog.Int("requeued", len(res.Requeued)),
			slog.Int("stale_running", len(res.StaleRunning)),
			slog.Int("stalled_runs", len(res.StalledRuns)))
	}
	if len(res.Requeued) > 0 && r.nudge != nil {
		r.nudge()
	}
	return res, nil
}

// claimIDRefString renders a nullable claim ID for logs.
func claimIDRefString(id *uuid.UUID) string {
	if id == nil {
		return "<none>"
	}
	return id.String()
}
