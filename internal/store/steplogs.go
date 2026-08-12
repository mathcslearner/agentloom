package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Step-log level vocabulary (ticket 7.4). Capture canonicalizes slog
// levels onto exactly these four values (the schema CHECK enforces it),
// so a stored level is always comparable by severity order below.
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

// logLevelOrder is the severity order, least severe first.
var logLevelOrder = []string{LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError}

// ValidLogLevel reports whether s is in the step-log level vocabulary.
func ValidLogLevel(s string) bool {
	for _, l := range logLevelOrder {
		if s == l {
			return true
		}
	}
	return false
}

// LogLevelsAtOrAbove returns the severity-order suffix starting at floor
// — the level set a minimum-level filter matches. An unknown floor
// returns every level (callers validate first; misuse stays harmless).
func LogLevelsAtOrAbove(floor string) []string {
	for i, l := range logLevelOrder {
		if l == floor {
			return logLevelOrder[i:]
		}
	}
	return logLevelOrder
}

// StepLogRepo stores step_logs rows (ticket 7.4): the per-attempt,
// size-capped ring of captured executor log lines. Written only by the
// async buffered writer (internal/exec/steplog); read by the logs API.
// There are no update methods — lines are immutable, and the only delete
// is the ring-cap trim.
type StepLogRepo interface {
	// CreateBatch inserts lines in one COPY. Per-attempt seq uniqueness is
	// the caller's contract (one writer per attempt); a violation surfaces
	// as a *ConflictError.
	CreateBatch(ctx context.Context, args []gen.CreateStepLogsParams) (int64, error)
	// Trim enforces the ring cap: deletes the attempt's rows with
	// seq <= maxDropSeq, returning how many went. Callers pass
	// max(seq written) - cap.
	Trim(ctx context.Context, runID uuid.UUID, stepID string, attempt int32, maxDropSeq int64) (int64, error)
	// ListPage returns one ascending-seq keyset page of an attempt's
	// lines, restricted to the given level set (see LogLevelsAtOrAbove).
	ListPage(ctx context.Context, arg gen.ListStepLogsPageParams) ([]gen.StepLog, error)
	// Stats returns the attempt's stored-row count and max seq; dropped
	// lines = MaxSeq - Stored (the logs API's derived truncation marker).
	Stats(ctx context.Context, runID uuid.UUID, stepID string, attempt int32) (gen.StepLogStatsRow, error)
}

type stepLogRepo struct{ q *gen.Queries }

func (r stepLogRepo) CreateBatch(ctx context.Context, args []gen.CreateStepLogsParams) (int64, error) {
	n, err := r.q.CreateStepLogs(ctx, args)
	return n, wrapErr("create step logs", err)
}

func (r stepLogRepo) Trim(ctx context.Context, runID uuid.UUID, stepID string, attempt int32, maxDropSeq int64) (int64, error) {
	n, err := r.q.TrimStepLogs(ctx, gen.TrimStepLogsParams{
		RunID: runID, StepID: stepID, Attempt: attempt, Seq: maxDropSeq,
	})
	return n, wrapErr("trim step logs", err)
}

func (r stepLogRepo) ListPage(ctx context.Context, arg gen.ListStepLogsPageParams) ([]gen.StepLog, error) {
	rows, err := r.q.ListStepLogsPage(ctx, arg)
	return rows, wrapErr("list step logs page", err)
}

func (r stepLogRepo) Stats(ctx context.Context, runID uuid.UUID, stepID string, attempt int32) (gen.StepLogStatsRow, error) {
	row, err := r.q.StepLogStats(ctx, gen.StepLogStatsParams{RunID: runID, StepID: stepID, Attempt: attempt})
	return row, wrapErr("step log stats", err)
}
