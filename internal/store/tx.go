package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/event"
)

// txMarker marks a context as already inside a WithTx callback so nested
// calls can be rejected (savepoints are complexity nothing here needs).
type txMarker struct{}

// txState travels on the WithTx context (keyed by txMarker) so the two
// sanctioned event-append helpers can record each committed event's projected
// envelope for the after-commit sink (ticket 16.2, ADR-018). It is never shared
// across transactions — WithTx rejects nesting — so its slice needs no locking:
// the transition statements that append events run sequentially within one tx.
type txState struct {
	events []event.Envelope
}

// recordEnvelope appends one committed event's envelope to the current
// transaction's buffer, if a WithTx is in progress. The append helpers call it
// after a successful AppendEvent with the payload they already hold, so the
// envelope is built without re-decoding the stored row. A call outside WithTx
// (no txState on the context) is a no-op — the after-commit sink only fires for
// events written inside a transaction, which is all of them.
func recordEnvelope(ctx context.Context, env event.Envelope) {
	if st, ok := ctx.Value(txMarker{}).(*txState); ok {
		st.events = append(st.events, env)
	}
}

// WithTx runs fn inside a single transaction: every repository call made
// through the Querier it receives executes on that transaction, which
// commits iff fn returns nil.
//
// On a non-nil error from fn the transaction is rolled back and the error
// returned wrapped (errors.Is/As reach the original). A panic in fn also
// rolls back, then propagates. Calling WithTx from inside fn — detectable
// via the context WithTx hands it — fails with ErrNestedTx: composing code
// shares the outer Querier instead.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, q Querier) error) error {
	if ctx.Value(txMarker{}) != nil {
		return fmt.Errorf("store: %w", ErrNestedTx)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	// Safety net for panics in fn and failed commits. WithoutCancel so a
	// context cancelled mid-fn cannot also doom the rollback; after a
	// clean commit or rollback this is a no-op (ErrTxClosed).
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	st := &txState{}
	txCtx := context.WithValue(ctx, txMarker{}, st)
	if err := fn(txCtx, repos{q: s.q.WithTx(tx)}); err != nil {
		err = fmt.Errorf("store: tx rolled back: %w", err)
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil {
			return errors.Join(err, fmt.Errorf("store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit tx: %w", err)
	}
	// After-commit event fan-out (ticket 16.2, ADR-018): the durable truth is
	// already committed above, so publishing is a best-effort latency hint. The
	// sink contract is non-blocking and never-panics; it fires only for a
	// committed tx that appended at least one event, and never on rollback. Use
	// a cancel-detached context so a caller context cancelled right after commit
	// cannot drop the fan-out. Consumers dedupe/order by (run_id, seq) and heal
	// any miss via a DB backfill, so a lost publish is never a correctness bug.
	if s.eventSink != nil && len(st.events) > 0 {
		s.eventSink.EventsCommitted(context.WithoutCancel(ctx), st.events)
	}
	return nil
}
