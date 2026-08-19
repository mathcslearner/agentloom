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

// IdempotencyMismatchError reports a CreateRun whose idempotency token was
// seen before but whose payload — definition snapshot, params, definition
// ref — differs from the original submission's (ticket 6.5, post-M4 audit):
// the replay is refused instead of silently returning the original run.
// Unwraps to ErrConflict; RunID is the original (conflicting) run.
type IdempotencyMismatchError struct {
	Token string
	RunID uuid.UUID
}

func (e *IdempotencyMismatchError) Error() string {
	return fmt.Sprintf("idempotency token replayed with a different payload (original run %s)", e.RunID)
}

func (e *IdempotencyMismatchError) Unwrap() error { return ErrConflict }

// VersionConflictError reports a CreateDefinitionVersion whose caller-supplied
// expected base version (the `If-Match` precondition, ticket 17.6) no longer
// matches the name's latest stored version — someone appended a version in
// between (a stale save). The append is refused so the client can reconcile.
// Latest is the current newest version; Expected is what the caller asserted.
// Unwraps to ErrConflict.
type VersionConflictError struct {
	Name     string
	Expected int32
	Latest   int32
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("definition %q version precondition failed: expected latest %d, but latest is %d",
		e.Name, e.Expected, e.Latest)
}

func (e *VersionConflictError) Unwrap() error { return ErrConflict }

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
	// ConflictRunNotRunning: a step claim was refused because its run is
	// not running (ticket 5.2, ADR-006 — the claim path refuses steps of
	// terminal runs; 5.6's park/cancel reuses the same guard). On this
	// reason alone, From carries the *run's* status, not the step's.
	ConflictRunNotRunning ConflictReason = "run_not_running"
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
	// CallerClaimID is the fencing token the caller presented; set only
	// when the rejected transition was claim-guarded (completion,
	// takeover). CurrentClaimID is the claim on the row at rejection time
	// (nil = none) and is always set for step conflicts: 4.5's zombie
	// rejections log both — whatever the rejection reason, since a fenced
	// worker most often sees wrong_status (the new holder already
	// completed) — and a rejected claim on a running step reads it as the
	// observed holder to fence the takeover CAS on.
	CallerClaimID  *uuid.UUID
	CurrentClaimID *uuid.UUID
}

func (e *TransitionError) Error() string {
	subject := e.Entity
	if e.StepID != "" {
		subject = fmt.Sprintf("%s %q", e.Entity, e.StepID)
	}
	msg := fmt.Sprintf("%s: %s transition to %q rejected (status %q)", subject, e.Reason, e.To, e.From)
	if e.CallerClaimID != nil {
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
