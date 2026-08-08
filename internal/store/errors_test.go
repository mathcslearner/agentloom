package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
)

func TestTransitionErrorUnwrapsToErrConflict(t *testing.T) {
	t.Parallel()
	te := &store.TransitionError{
		Entity: "step", RunID: uuid.New(), StepID: "gather",
		From: store.StepStatusRunning, To: store.StepStatusSucceeded,
		Reason: store.ConflictWrongStatus,
	}
	wrapped := fmt.Errorf("store: succeed step: %w", te)

	if !errors.Is(wrapped, store.ErrConflict) {
		t.Error("TransitionError does not unwrap to ErrConflict")
	}
	var got *store.TransitionError
	if !errors.As(wrapped, &got) || got != te {
		t.Error("errors.As failed to recover the *TransitionError")
	}
	if errors.Is(wrapped, store.ErrNotFound) {
		t.Error("TransitionError must not match ErrNotFound")
	}
}

func TestTransitionErrorMessage(t *testing.T) {
	t.Parallel()
	caller, current := uuid.New(), uuid.New()

	te := &store.TransitionError{
		Entity: "step", RunID: uuid.New(), StepID: "gather",
		From: store.StepStatusRunning, To: store.StepStatusSucceeded,
		Reason:        store.ConflictClaimMismatch,
		CallerClaimID: &caller, CurrentClaimID: &current,
	}
	msg := te.Error()
	for _, want := range []string{`step "gather"`, "claim_mismatch", caller.String(), current.String()} {
		if !strings.Contains(msg, want) {
			t.Errorf("claim-mismatch message %q missing %q", msg, want)
		}
	}

	re := &store.TransitionError{
		Entity: "run", RunID: uuid.New(),
		From: store.RunStatusSucceeded, To: store.RunStatusFailed,
		Reason: store.ConflictWrongStatus,
	}
	msg = re.Error()
	for _, want := range []string{"run:", "wrong_status", `"succeeded"`, `"failed"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("run message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "claim") {
		t.Errorf("run message %q mentions claims without a claim mismatch", msg)
	}
}
