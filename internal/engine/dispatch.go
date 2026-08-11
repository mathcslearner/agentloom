package engine

// The outbox dispatcher (ticket 4.4): the drain half of ADR-002's
// "every worker participates in dispatch". A Dispatcher moves task_outbox
// rows onto the Redis stream — SELECT … FOR UPDATE SKIP LOCKED batch,
// XADD each row's envelope, DELETE what was XADDed, commit. SKIP LOCKED
// partitions concurrent drainers onto disjoint rows, so under normal
// operation each row is dispatched exactly once; a crash or rollback
// after an XADD re-drains the row (ADR-005 P1) and the duplicate entry
// is ACK-dropped by the claim CAS. Enqueue stays at-least-once by design.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Enqueuer is the dispatcher's producer seam — satisfied by *queue.Queue.
// An interface so failure-injection tests can wrap the real producer with
// lost or half-committed XADDs (ADR-005's P1/P2 crash cells).
type Enqueuer interface {
	Enqueue(ctx context.Context, env queue.Envelope) (string, error)
}

// DispatcherConfig tunes one Dispatcher.
type DispatcherConfig struct {
	// Interval is the polling cadence — the latency backstop when no nudge
	// arrives (another worker's completion wrote the outbox rows, or a
	// nudge was dropped). Must be positive.
	Interval time.Duration
	// Batch is the maximum rows locked and dispatched per drain
	// transaction. Must be positive.
	Batch int
}

// Dispatcher drains the transactional outbox to the stream. One per
// worker process; safe alongside any number of concurrent dispatchers on
// other workers.
type Dispatcher struct {
	store *store.Store
	enq   Enqueuer
	cfg   DispatcherConfig
	now   func() time.Time
	// wake coalesces nudges: capacity 1, non-blocking send. A nudge while
	// a drain is running is not lost — it stays buffered and triggers one
	// more pass.
	wake chan struct{}
}

// DispatcherOption customizes a Dispatcher.
type DispatcherOption func(*Dispatcher)

// WithDispatcherClock overrides the dispatcher's clock (project
// invariant: time is injectable). The clock only stamps envelope
// enqueued_at_ms — informational, never an input to logic.
func WithDispatcherClock(now func() time.Time) DispatcherOption {
	return func(d *Dispatcher) { d.now = now }
}

// NewDispatcher builds a Dispatcher over the store and producer.
func NewDispatcher(s *store.Store, enq Enqueuer, cfg DispatcherConfig, opts ...DispatcherOption) (*Dispatcher, error) {
	if s == nil {
		return nil, errors.New("engine: NewDispatcher requires a store")
	}
	if enq == nil {
		return nil, errors.New("engine: NewDispatcher requires an enqueuer")
	}
	if cfg.Interval <= 0 {
		return nil, errors.New("engine: NewDispatcher requires a positive Interval")
	}
	if cfg.Batch <= 0 {
		return nil, errors.New("engine: NewDispatcher requires a positive Batch")
	}
	d := &Dispatcher{store: s, enq: enq, cfg: cfg, now: time.Now, wake: make(chan struct{}, 1)}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

// Nudge wakes the drain loop. Non-blocking and safe for concurrent calls
// — the WithDispatchNudge contract; excess nudges coalesce into the one
// buffered wake-up.
func (d *Dispatcher) Nudge() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run drains until ctx is canceled: on every tick or nudge it drains the
// outbox to empty. Errors are logged and retried next wake — a Redis or
// Postgres blip must never kill the worker; undrained rows are durable
// and the reconciler backstops nothing here, because the rows themselves
// are the pending work.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.wake:
		}
		if err := recoverPass(func() error {
			_, err := d.drainAll(ctx)
			return err
		}); err != nil && ctx.Err() == nil {
			log.From(ctx).WarnContext(ctx, "outbox drain failed; retrying next tick",
				slog.Any("error", err))
		}
	}
}

// drainAll repeats DrainOnce until a batch comes back short (the outbox
// is empty or nearly so), an error stops the pass, or ctx ends.
func (d *Dispatcher) drainAll(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := d.DrainOnce(ctx)
		total += n
		if err != nil || n < d.cfg.Batch {
			return total, err
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

// recoverPass runs one background-duty pass, converting a panic into an
// error so the loop's "must never kill the worker" contract holds even
// against a bug in the pass itself (post-M4 audit). The consumer's
// handler path has its own containment (queue.safeHandle); this covers
// the dispatch and reconcile loops.
func recoverPass(pass func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("engine: background pass panic: %v\n%s", rec, debug.Stack())
		}
	}()
	return pass()
}

// DrainOnce runs one drain transaction — ADR-005's happy-path producer
// sequence: lock a batch (SKIP LOCKED), XADD each row's envelope, delete
// the XADDed rows, commit. It returns how many rows were dispatched and
// deleted.
//
// An Enqueue failure mid-batch stops the pass but still commits the rows
// that did reach the stream — their deletes must land, or a retried pass
// would re-XADD them all. The failed row itself is NOT deleted: whether
// its XADD landed is unknown (the P1 window), so it stays pending and the
// next pass re-dispatches it — at-least-once, deduplicated at claim.
func (d *Dispatcher) DrainOnce(ctx context.Context) (int, error) {
	var (
		drained int
		enqErr  error
	)
	txErr := d.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		tasks, err := q.Outbox().ListForDrain(ctx, int32(d.cfg.Batch)) //nolint:gosec // Batch is a small validated positive
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			return nil
		}
		ids := make([]int64, 0, len(tasks))
		for _, task := range tasks {
			if _, err := d.enq.Enqueue(ctx, queue.Envelope{
				RunID:      task.RunID,
				StepID:     task.StepID,
				Reason:     task.Reason,
				EnqueuedAt: d.now(),
			}); err != nil {
				enqErr = err
				break
			}
			ids = append(ids, task.ID)
		}
		if len(ids) > 0 {
			if _, err := q.Outbox().Delete(ctx, ids); err != nil {
				return err
			}
		}
		drained = len(ids)
		return nil
	})
	if txErr != nil {
		// Rolled back: any XADDs that happened this pass are now duplicates
		// of rows still in the outbox — the P1 shape, absorbed at claim.
		return 0, txErr
	}
	if drained > 0 {
		log.From(ctx).DebugContext(ctx, "outbox drained",
			slog.Int("dispatched", drained))
	}
	return drained, enqErr
}
