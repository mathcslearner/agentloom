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
// returns: fmt-wrapped, reachable via errors.As.
func transitionErr(reason store.ConflictReason, from string) error {
	return fmt.Errorf("store: claim step: %w", &store.TransitionError{
		Entity: "step",
		RunID:  uuid.New(),
		StepID: "s",
		From:   from,
		To:     store.StepStatusRunning,
		Reason: reason,
	})
}

func TestClassifyClaimFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		err           error
		deliveryCount int64
		wantAck       bool
		wantLevel     slog.Level
		wantInReason  string
	}{
		{
			name:          "terminal succeeded is duplicate of finished work",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusSucceeded),
			deliveryCount: 1,
			wantAck:       true,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "already terminal",
		},
		{
			name:          "terminal failed is duplicate of finished work",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusFailed),
			deliveryCount: 2,
			wantAck:       true,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "already terminal",
		},
		{
			name:          "terminal skipped is duplicate of finished work",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusSkipped),
			deliveryCount: 1,
			wantAck:       true,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "already terminal",
		},
		{
			name:          "running on fresh delivery is concurrent duplicate",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusRunning),
			deliveryCount: 1,
			wantAck:       true,
			wantLevel:     slog.LevelInfo,
			wantInReason:  "concurrent duplicate",
		},
		{
			name:          "running on reclaimed delivery awaits takeover (4.5)",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusRunning),
			deliveryCount: 2,
			wantAck:       false,
			wantLevel:     slog.LevelWarn,
			wantInReason:  "takeover",
		},
		{
			name:          "pending step keeps the entry pending",
			err:           transitionErr(store.ConflictWrongStatus, store.StepStatusPending),
			deliveryCount: 1,
			wantAck:       false,
			wantLevel:     slog.LevelError,
			wantInReason:  "unexpected status",
		},
		{
			name:          "unexpected conflict reason keeps the entry pending",
			err:           transitionErr(store.ConflictGuardFailed, store.StepStatusReady),
			deliveryCount: 1,
			wantAck:       false,
			wantLevel:     slog.LevelError,
			wantInReason:  "unexpected conflict reason",
		},
		{
			name:          "missing run or step is a dangling reference",
			err:           fmt.Errorf("store: claim step: lock run: %w", store.ErrNotFound),
			deliveryCount: 1,
			wantAck:       true,
			wantLevel:     slog.LevelWarn,
			wantInReason:  "dangling reference",
		},
		{
			name:          "transport failure redelivers",
			err:           errors.New("connection refused"),
			deliveryCount: 1,
			wantAck:       false,
			wantLevel:     slog.LevelError,
			wantInReason:  "claim transaction failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := classifyClaimFailure(tc.err, tc.deliveryCount)
			if dec.ack != tc.wantAck {
				t.Errorf("ack = %t, want %t", dec.ack, tc.wantAck)
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
