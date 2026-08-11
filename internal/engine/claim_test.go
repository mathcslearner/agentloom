package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store"
)

// transitionErr builds the wrapped *TransitionError shape ClaimStep
// returns: fmt-wrapped, reachable via errors.As. currentClaim mirrors
// stepConflict, which always reports the row's claim at rejection time.
func transitionErr(reason store.ConflictReason, from string, currentClaim *uuid.UUID) error {
	return fmt.Errorf("store: claim step: %w", &store.TransitionError{
		Entity:         "step",
		RunID:          uuid.New(),
		StepID:         "s",
		From:           from,
		To:             store.StepStatusRunning,
		Reason:         reason,
		CurrentClaimID: currentClaim,
	})
}

// takeoverErr is transitionErr for the takeover CAS (To = ready).
func takeoverErr(reason store.ConflictReason, from string) error {
	caller := uuid.New()
	return fmt.Errorf("store: takeover step: %w", &store.TransitionError{
		Entity:        "step",
		RunID:         uuid.New(),
		StepID:        "s",
		From:          from,
		To:            store.StepStatusReady,
		Reason:        reason,
		CallerClaimID: &caller,
	})
}

func TestClassifyClaimFailure(t *testing.T) {
	t.Parallel()

	holder := uuid.New()
	cases := []struct {
		name            string
		err             error
		deliveryCount   int64
		wantAction      claimAction
		wantLevel       slog.Level
		wantInReason    string
		wantHolderClaim *uuid.UUID
	}{
		{
			name:          "terminal succeeded is duplicate of finished work",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusSucceeded, &holder),
			deliveryCount: 1,
			wantAction:    claimAckDrop,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "already terminal",
		},
		{
			name:          "terminal failed is duplicate of finished work",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusFailed, &holder),
			deliveryCount: 2,
			wantAction:    claimAckDrop,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "already terminal",
		},
		{
			name:          "terminal skipped is duplicate of finished work",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusSkipped, nil),
			deliveryCount: 1,
			wantAction:    claimAckDrop,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "already terminal",
		},
		{
			name:          "retrying with backoff pending is dropped (5.2)",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusRetrying, nil),
			deliveryCount: 1,
			wantAction:    claimAckDrop,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "backoff pending",
		},
		{
			name:          "retrying on reclaimed delivery is dropped too (5.2)",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusRetrying, nil),
			deliveryCount: 3,
			wantAction:    claimAckDrop,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "backoff pending",
		},
		{
			name:          "run not running drops the delivery (5.2)",
			err:           transitionErr(store.ConflictRunNotRunning, store.RunStatusFailed, &holder),
			deliveryCount: 1,
			wantAction:    claimAckDrop,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "run not running",
		},
		{
			name:          "running on fresh delivery is concurrent duplicate",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusRunning, &holder),
			deliveryCount: 1,
			wantAction:    claimAckDrop,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "concurrent duplicate",
		},
		{
			name:            "running on reclaimed delivery takes over (4.5)",
			err:             transitionErr(store.ConflictWrongStatus, store.StepStatusRunning, &holder),
			deliveryCount:   2,
			wantAction:      claimTakeover,
			wantLevel:       slog.LevelWarn,
			wantInReason:    "taking over",
			wantHolderClaim: &holder,
		},
		{
			name:          "running with no claim is corrupt state — redeliver",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusRunning, nil),
			deliveryCount: 2,
			wantAction:    claimRedeliver,
			wantLevel:     slog.LevelError,
			wantInReason:  "no claim_id",
		},
		{
			name:          "pending step keeps the entry pending",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusPending, nil),
			deliveryCount: 1,
			wantAction:    claimRedeliver,
			wantLevel:     slog.LevelError,
			wantInReason:  "unexpected status",
		},
		{
			name:          "unexpected conflict reason keeps the entry pending",
			err:           transitionErr(store.ConflictGuardFailed, store.StepStatusReady, nil),
			deliveryCount: 1,
			wantAction:    claimRedeliver,
			wantLevel:     slog.LevelError,
			wantInReason:  "unexpected conflict reason",
		},
		{
			name:          "missing run or step is a dangling reference",
			err:           fmt.Errorf("store: claim step: lock run: %w", store.ErrNotFound),
			deliveryCount: 1,
			wantAction:    claimAckDrop,
			wantLevel:     slog.LevelWarn,
			wantInReason:  "dangling reference",
		},
		{
			name:          "transport failure redelivers",
			err:           errors.New("connection refused"),
			deliveryCount: 1,
			wantAction:    claimRedeliver,
			wantLevel:     slog.LevelError,
			wantInReason:  "claim transaction failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := classifyClaimFailure(tc.err, tc.deliveryCount)
			if dec.action != tc.wantAction {
				t.Errorf("action = %v, want %v", dec.action, tc.wantAction)
			}
			if dec.level != tc.wantLevel {
				t.Errorf("level = %v, want %v", dec.level, tc.wantLevel)
			}
			if !strings.Contains(dec.reason, tc.wantInReason) {
				t.Errorf("reason %q does not mention %q", dec.reason, tc.wantInReason)
			}
			switch {
			case tc.wantHolderClaim == nil:
				if dec.holderClaim != nil {
					t.Errorf("holderClaim = %v, want nil", dec.holderClaim)
				}
			case dec.holderClaim == nil || *dec.holderClaim != *tc.wantHolderClaim:
				t.Errorf("holderClaim = %v, want %v", dec.holderClaim, tc.wantHolderClaim)
			}
		})
	}
}

func TestClassifyTakeoverFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		err          error
		wantAction   claimAction
		wantLevel    slog.Level
		wantInReason string
	}{
		{
			name:         "claim mismatch means a newer live holder — drop",
			err:          takeoverErr(store.ConflictClaimMismatch, store.StepStatusRunning),
			wantAction:   claimAckDrop,
			wantLevel:    slog.LevelWarn,
			wantInReason: "newer holder",
		},
		{
			name:         "completed between reclaim and takeover — drop",
			err:          takeoverErr(store.ConflictWrongStatus, store.StepStatusSucceeded),
			wantAction:   claimAckDrop,
			wantLevel:    slog.LevelInfo,
			wantInReason: "duplicate of finished work",
		},
		{
			name:         "failed between reclaim and takeover — drop",
			err:          takeoverErr(store.ConflictWrongStatus, store.StepStatusFailed),
			wantAction:   claimAckDrop,
			wantLevel:    slog.LevelInfo,
			wantInReason: "duplicate of finished work",
		},
		{
			name:         "retry-routed between reclaim and takeover — drop (5.2)",
			err:          takeoverErr(store.ConflictWrongStatus, store.StepStatusRetrying),
			wantAction:   claimAckDrop,
			wantLevel:    slog.LevelInfo,
			wantInReason: "duplicate of finished work",
		},
		{
			name:         "run not running drops the reclaimed delivery (5.2)",
			err:          takeoverErr(store.ConflictRunNotRunning, store.RunStatusFailed),
			wantAction:   claimAckDrop,
			wantLevel:    slog.LevelInfo,
			wantInReason: "run not running",
		},
		{
			name:         "unexpected takeover conflict redelivers",
			err:          takeoverErr(store.ConflictWrongStatus, store.StepStatusPending),
			wantAction:   claimRedeliver,
			wantLevel:    slog.LevelError,
			wantInReason: "unexpected takeover conflict",
		},
		{
			name: "conflict on the post-takeover claim redelivers",
			err: fmt.Errorf("store: claim step: %w", &store.TransitionError{
				Entity: "step", RunID: uuid.New(), StepID: "s",
				From: store.StepStatusRunning, To: store.StepStatusRunning,
				Reason: store.ConflictWrongStatus,
			}),
			wantAction:   claimRedeliver,
			wantLevel:    slog.LevelError,
			wantInReason: "post-takeover claim",
		},
		{
			name:         "missing run or step is a dangling reference",
			err:          fmt.Errorf("store: takeover step: lock run: %w", store.ErrNotFound),
			wantAction:   claimAckDrop,
			wantLevel:    slog.LevelWarn,
			wantInReason: "dangling reference",
		},
		{
			name:         "transport failure redelivers",
			err:          errors.New("connection refused"),
			wantAction:   claimRedeliver,
			wantLevel:    slog.LevelError,
			wantInReason: "takeover transaction failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := classifyTakeoverFailure(tc.err)
			if dec.action != tc.wantAction {
				t.Errorf("action = %v, want %v", dec.action, tc.wantAction)
			}
			if dec.level != tc.wantLevel {
				t.Errorf("level = %v, want %v", dec.level, tc.wantLevel)
			}
			if !strings.Contains(dec.reason, tc.wantInReason) {
				t.Errorf("reason %q does not mention %q", dec.reason, tc.wantInReason)
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, exec.Builtins(), "w"); err == nil {
		t.Error("New with nil store: want error, got nil")
	}
	if _, err := New(&store.Store{}, nil, "w"); err == nil {
		t.Error("New with nil registry: want error, got nil")
	}
}
