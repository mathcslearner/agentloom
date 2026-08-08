package store

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is returned by single-row reads that match nothing and by
// deletes that remove nothing. Test with errors.Is.
var ErrNotFound = errors.New("not found")

// ErrConflict is the sentinel every lost or illegal state transition
// unwraps to: the conditional UPDATE matched no row because the entity was
// not in the expected state. Test with errors.Is; errors.As a
// *TransitionError for the diagnosis.
var ErrConflict = errors.New("transition conflict")

// ErrNoTx is returned when a transition function is called outside a
// WithTx callback or with a Querier other than the one the callback
// received — transitions are multi-statement and only atomic inside the
// transaction WithTx owns.
var ErrNoTx = errors.New("transition requires the Querier a WithTx callback receives")

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

// ConflictReason classifies why a transition was rejected.
type ConflictReason string

// Conflict reasons.
const (
	// ConflictWrongStatus: the row's status is not the transition's
	// required from-status — an illegal transition, or a race lost to
	// whoever moved the row first.
	ConflictWrongStatus ConflictReason = "wrong_status"
	// ConflictClaimMismatch: the status matched but the presented claim_id
	// is not the row's — a fenced zombie write (ADR-004 / M4).
	ConflictClaimMismatch ConflictReason = "claim_mismatch"
	// ConflictGuardFailed: status (and claim, where relevant) matched but
	// the transition's guard predicate did not — e.g. readying a pending
	// step with unresolved dependencies, or a premature run rollup.
	ConflictGuardFailed ConflictReason = "guard_failed"
)

// TransitionError reports a rejected state transition (ticket 2.6). It
// unwraps to ErrConflict.
type TransitionError struct {
	// Entity is "run" or "step".
	Entity string
	RunID  uuid.UUID
	// StepID is empty for run transitions.
	StepID string
	// From is the row's actual status at rejection time; To the attempted
	// target status.
	From, To string
	Reason   ConflictReason
	// CallerClaimID and CurrentClaimID are set on claim_mismatch: the
	// fencing token the caller presented vs the one on the row (nil =
	// none) — M4.5 logs both on zombie rejections.
	CallerClaimID  *uuid.UUID
	CurrentClaimID *uuid.UUID
}

func (e *TransitionError) Error() string {
	subject := e.Entity
	if e.StepID != "" {
		subject = fmt.Sprintf("%s %q", e.Entity, e.StepID)
	}
	msg := fmt.Sprintf("%s: %s transition to %q rejected (status %q)", subject, e.Reason, e.To, e.From)
	if e.Reason == ConflictClaimMismatch {
		msg += fmt.Sprintf(" [caller claim %s, current claim %s]",
			uuidOrNone(e.CallerClaimID), uuidOrNone(e.CurrentClaimID))
	}
	return msg
}

func (e *TransitionError) Unwrap() error { return ErrConflict }

func uuidOrNone(id *uuid.UUID) string {
	if id == nil {
		return "<none>"
	}
	return id.String()
}

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
