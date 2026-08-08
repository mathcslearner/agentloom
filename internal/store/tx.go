package store

import (
	"context"
	"errors"
	"fmt"
)

// txMarker marks a context as already inside a WithTx callback so nested
// calls can be rejected (savepoints are complexity nothing here needs).
type txMarker struct{}

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

	txCtx := context.WithValue(ctx, txMarker{}, struct{}{})
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
	return nil
}
