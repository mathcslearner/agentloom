package engine

// Failure classification (ticket 5.2, ADR-006 "Classification"): the pure
// function that assigns every executor error its class, worker-side at
// completion time. Call sites that *know* the class from context — the
// force-classified permanent set of taxonomy rows 6–7 (CEL edge failures,
// corrupt stored content) — pass it to completeFailure explicitly instead
// of routing through here.

import (
	"errors"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
)

// classifyFailure maps an executor error onto its ADR-006 class:
//
//   - A ClassifiedError declaring transient or permanent is honored as
//     declared (taxonomy row 2).
//   - exec.ErrUnknownType and exec.ErrInvalidConfig are force-classified
//     permanent (rows 4–5): deterministic functions of stored state, so
//     re-execution is provably futile.
//   - Everything else defaults to transient (row 1): a generic error is
//     far more often a flaky dependency than a deterministic bug, and the
//     cost of misclassifying is bounded by the retry budget.
//
// A ClassifiedError declaring any other class (timeout and cancelled are
// engine-assigned in M5.3/5.6; validation_failed is reserved until M11)
// is executor misuse and falls back to the unclassified default,
// transient — the caller logs it.
func classifyFailure(err error) dag.ErrorClass {
	var ce *exec.ClassifiedError
	if errors.As(err, &ce) {
		switch ce.Class {
		case dag.ClassTransient, dag.ClassPermanent:
			return ce.Class
		}
		return dag.ClassTransient
	}
	if errors.Is(err, exec.ErrUnknownType) || errors.Is(err, exec.ErrInvalidConfig) {
		return dag.ClassPermanent
	}
	return dag.ClassTransient
}

// misdeclaredClass reports whether err carries a ClassifiedError whose
// declared class executors may not use — the case classifyFailure fell
// back to transient on, which the caller should log loudly.
func misdeclaredClass(err error) (dag.ErrorClass, bool) {
	var ce *exec.ClassifiedError
	if errors.As(err, &ce) {
		switch ce.Class {
		case dag.ClassTransient, dag.ClassPermanent:
			return "", false
		}
		return ce.Class, true
	}
	return "", false
}
