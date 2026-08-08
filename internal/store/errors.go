package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is returned by single-row reads that match nothing and by
// deletes that remove nothing. Test with errors.Is.
var ErrNotFound = errors.New("not found")

// ErrNestedTx is returned when WithTx is called from inside a WithTx
// callback. The store has no savepoint semantics: transaction-composing
// code takes the Querier the outer callback received instead of opening
// its own transaction.
var ErrNestedTx = errors.New("nested WithTx call")

// ConflictError reports a violated uniqueness or referential constraint —
// duplicate (name, version), duplicate idempotency token, delete blocked
// by ON DELETE RESTRICT, and the like. Test with errors.As; Constraint
// names the violated database constraint.
type ConflictError struct {
	// Constraint is the database constraint that was violated.
	Constraint string
	cause      error
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict on constraint %q", e.Constraint)
}

func (e *ConflictError) Unwrap() error { return e.cause }

// errNoRowsDeleted is what delete methods feed wrapErr when no row matched;
// callers observe it as ErrNotFound.
var errNoRowsDeleted = ErrNotFound

// wrapErr maps driver errors onto the store's typed errors and stamps the
// failed operation. Every repository return path goes through it.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgerrcode.UniqueViolation, pgerrcode.ForeignKeyViolation:
			return fmt.Errorf("store: %s: %w", op, &ConflictError{Constraint: pgErr.ConstraintName, cause: err})
		}
	}
	return fmt.Errorf("store: %s: %w", op, err)
}
