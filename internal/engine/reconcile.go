package engine

// The reconciler (tickets 4.4 + 4.5): the periodic healer closing every
// Postgres→Redis dual-write gap the outbox alone cannot (ADR-002). Each
// sweep runs in one transaction under a fleet-wide advisory lock: steps
// stuck in ready past a threshold with no pending outbox row get a fresh
// row (reason reconcile_ready) — the lost-dispatch heal for ADR-005 crash
// cells P2 and R1(a); running steps staler than a threshold ≫ lease TTL
// (R1(c) dead-worker suspects, e.g. after Redis lost the PEL so no lease
// will ever expire) get the lease-expiry takeover — store.TakeoverStep,
// fenced on the observed claim, running → ready — plus a re-outbox
// (reason reconcile_running); runs still running with no live step (an
// invariant violation) are flagged loudly and left untouched.
//
// Rate-bounding is layered: the advisory lock admits one sweep fleet-wide
// at a time, the interval is jittered so worker fleets do not align, each
// sweep is capped at Limit rows, and both heals are idempotent per sweep —
// a re-outboxed ready step stops matching the anti-join until its row is
// drained, and a taken-over step is ready with a fresh updated_at and a
// pending outbox row, so it matches neither scan again.
//
// Deadlock note: takeovers make the sweep take run locks (TakeoverStep
// locks the run first, like every transition) for several runs in one
// transaction, in scan order. That cannot deadlock against claim and
// completion transactions — they hold locks on a single run — and sweeps
// cannot overlap each other (the advisory lock).

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
	// RunningStale is how long a step may sit in running before its holder
	// is presumed dead and the step is taken over + re-outboxed. Must be
	// ≫ the lease TTL: updated_at moves on transitions, not heartbeats, so
	// a healthy long-running step looks stale on any tighter bound. In
	// effect this is a hard cap on a step's wall-clock execution time: a
	// live holder still executing past it gets taken over, and while its
	// eventual completion is fenced (state stays correct), the step's side
	// effects run twice. Until M5.3 gives steps real timeouts, size this
	// above the longest executor you expect to run. Must be positive.
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
	// TakenOver lists the stale-running steps this sweep healed with the
	// lease-expiry takeover + re-outbox (ADR-005 R1(c), ticket 4.5).
	TakenOver []store.StaleRunningStep
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
		if err := recoverPass(func() error {
			_, err := r.ReconcileOnce(ctx)
			return err
		}); err != nil && ctx.Err() == nil {
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
	// Stale-running steps the takeover could not heal, logged post-commit:
	// lostRace moved on between the scan snapshot and the fenced CAS
	// (benign — the guard did its job); corrupt carried no claim to fence
	// on (impossible state, running always has one).
	var lostRace, corrupt []store.StaleRunningStep
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
		staleRunning, err := store.ListStaleRunningSteps(ctx, q, now.Add(-r.cfg.RunningStale), limit)
		if err != nil {
			return err
		}
		for _, st := range staleRunning {
			if st.ClaimID == nil {
				corrupt = append(corrupt, st)
				continue
			}
			if _, err := store.TakeoverStep(ctx, q, store.TakeoverStepArgs{
				RunID: st.RunID, StepID: st.StepID, ClaimID: *st.ClaimID, Now: now,
			}); err != nil {
				// A typed conflict means the step moved on (completed, or
				// taken over and re-claimed) between the scan snapshot and
				// the row lock — drop this heal, never the sweep. A
				// rejected transition writes nothing, so the transaction
				// stays clean.
				var te *store.TransitionError
				if errors.As(err, &te) {
					lostRace = append(lostRace, st)
					continue
				}
				return err
			}
			// Skip the re-outbox when an undrained dispatch row already
			// exists (P1 sustained past the threshold): the takeover above
			// healed the dangling claim, and the pending row carries the
			// dispatch — a second row would just double it.
			if !st.HasPendingOutbox {
				if _, err := q.Outbox().Create(ctx, st.RunID, st.StepID, store.OutboxReasonReconcileRunning); err != nil {
					return err
				}
			}
			res.TakenOver = append(res.TakenOver, st)
		}
		res.StalledRuns, err = store.ListStalledRuns(ctx, q, limit)
		if err != nil {
			return err
		}
		res.LimitHit = len(res.Requeued) == int(limit) ||
			len(staleRunning) == int(limit) || len(res.StalledRuns) == int(limit)
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
	for _, st := range res.TakenOver {
		logger.WarnContext(ctx, "reconciler took over a stale running step — holder presumed dead, re-outboxed",
			log.RunID(st.RunID.String()), log.StepID(st.StepID),
			slog.Time("running_since", st.UpdatedAt),
			slog.String("displaced_claim_id", claimIDRefString(st.ClaimID)))
	}
	for _, st := range lostRace {
		logger.InfoContext(ctx, "stale running step moved on before takeover — heal dropped",
			log.RunID(st.RunID.String()), log.StepID(st.StepID))
	}
	for _, st := range corrupt {
		logger.ErrorContext(ctx, "running step has no claim_id — corrupt state, cannot take over, investigate",
			log.RunID(st.RunID.String()), log.StepID(st.StepID),
			slog.Time("running_since", st.UpdatedAt))
	}
	for _, runID := range res.StalledRuns {
		logger.ErrorContext(ctx, "run running with no live step — impossible state, investigate",
			log.RunID(runID.String()))
	}
	if res.LimitHit {
		logger.WarnContext(ctx, "reconciler sweep hit its row limit; more may remain for the next sweep",
			slog.Int("limit", r.cfg.Limit))
	}
	if len(res.Requeued) > 0 || len(res.TakenOver) > 0 || len(res.StalledRuns) > 0 {
		logger.InfoContext(ctx, "reconciler sweep complete",
			slog.Int("requeued", len(res.Requeued)),
			slog.Int("taken_over", len(res.TakenOver)),
			slog.Int("stalled_runs", len(res.StalledRuns)))
	}
	if (len(res.Requeued) > 0 || len(res.TakenOver) > 0) && r.nudge != nil {
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
