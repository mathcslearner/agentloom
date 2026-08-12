//go:build integration

package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 7.4's store-level suite: the step_logs repo the async writer
// (internal/exec/steplog) and the logs API compose — batch insert, the
// ring-cap trim, keyset paging with the minimum-level filter, and the
// stats read behind the derived truncation marker.

// logLine builds one CreateStepLogsParams row with defaults the tests
// override as needed.
func logLine(runID uuid.UUID, stepID string, attempt int32, seq int64, level, msg string) gen.CreateStepLogsParams {
	return gen.CreateStepLogsParams{
		RunID: runID, StepID: stepID, Attempt: attempt, Seq: seq,
		Level: level, Message: msg, LoggedAt: testNow,
	}
}

func TestStepLogsBatchTrimAndStats(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	const capLines, written = 5, 12
	lines := make([]gen.CreateStepLogsParams, 0, written)
	for seq := int64(1); seq <= written; seq++ {
		lines = append(lines, logLine(run.ID, "a", 1, seq, store.LogLevelInfo, fmt.Sprintf("line %d", seq)))
	}
	if n, err := s.StepLogs().CreateBatch(ctx, lines); err != nil || n != written {
		t.Fatalf("CreateBatch = (%d, %v), want (%d, nil)", n, err, written)
	}

	// Ring enforcement: drop everything at or below max(seq) - cap.
	if n, err := s.StepLogs().Trim(ctx, run.ID, "a", 1, written-capLines); err != nil || n != written-capLines {
		t.Fatalf("Trim = (%d, %v), want (%d, nil)", n, err, written-capLines)
	}

	stats, err := s.StepLogs().Stats(ctx, run.ID, "a", 1)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Stored != capLines || stats.MaxSeq != written {
		t.Errorf("Stats = %+v, want stored %d, max_seq %d", stats, capLines, written)
	}
	// dropped = max_seq - stored is the logs API's truncation marker.
	if dropped := stats.MaxSeq - stats.Stored; dropped != written-capLines {
		t.Errorf("derived dropped = %d, want %d", dropped, written-capLines)
	}

	// The retained window is the newest cap lines, in seq order.
	page, err := s.StepLogs().ListPage(ctx, gen.ListStepLogsPageParams{
		RunID: run.ID, StepID: "a", Attempt: 1,
		Levels: store.LogLevelsAtOrAbove(store.LogLevelDebug), Limit: written,
	})
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if len(page) != capLines || page[0].Seq != written-capLines+1 || page[len(page)-1].Seq != written {
		t.Errorf("retained window = %d rows [%d..%d], want %d rows [%d..%d]",
			len(page), page[0].Seq, page[len(page)-1].Seq, capLines, written-capLines+1, written)
	}

	// A duplicate seq is a loud conflict, not silence: the single-writer-
	// per-attempt protocol makes it a bug worth surfacing.
	var conflict *store.ConflictError
	if _, err := s.StepLogs().CreateBatch(ctx, []gen.CreateStepLogsParams{
		logLine(run.ID, "a", 1, written, store.LogLevelInfo, "dup"),
	}); !errors.As(err, &conflict) {
		t.Errorf("duplicate seq CreateBatch: %v, want *ConflictError", err)
	}
}

func TestStepLogsKeysetPageAndLevelFilter(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))
	ctx := t.Context()
	run := instantiate(t, s, decodeDef(t, twoEntrySteps))

	levels := []string{store.LogLevelDebug, store.LogLevelInfo, store.LogLevelWarn, store.LogLevelError}
	lines := make([]gen.CreateStepLogsParams, 0, 20)
	for seq := int64(1); seq <= 20; seq++ {
		lines = append(lines, logLine(run.ID, "a", 1, seq, levels[(seq-1)%4], fmt.Sprintf("line %d", seq)))
	}
	if _, err := s.StepLogs().CreateBatch(ctx, lines); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	// Keyset walk at page size 3 visits every row exactly once, in order.
	var got []int64
	after := int64(0)
	for {
		page, err := s.StepLogs().ListPage(ctx, gen.ListStepLogsPageParams{
			RunID: run.ID, StepID: "a", Attempt: 1, Seq: after,
			Levels: store.LogLevelsAtOrAbove(store.LogLevelDebug), Limit: 3,
		})
		if err != nil {
			t.Fatalf("ListPage(after=%d): %v", after, err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			got = append(got, row.Seq)
		}
		after = page[len(page)-1].Seq
	}
	if len(got) != 20 {
		t.Fatalf("keyset walk visited %d rows, want 20: %v", len(got), got)
	}
	for i, seq := range got {
		if seq != int64(i+1) {
			t.Fatalf("keyset walk out of order at %d: %v", i, got)
		}
	}

	// Minimum-level filter: warn+ matches exactly the warn and error rows.
	page, err := s.StepLogs().ListPage(ctx, gen.ListStepLogsPageParams{
		RunID: run.ID, StepID: "a", Attempt: 1,
		Levels: store.LogLevelsAtOrAbove(store.LogLevelWarn), Limit: 20,
	})
	if err != nil {
		t.Fatalf("ListPage(warn+): %v", err)
	}
	if len(page) != 10 {
		t.Errorf("warn+ filter returned %d rows, want 10", len(page))
	}
	for _, row := range page {
		if row.Level != store.LogLevelWarn && row.Level != store.LogLevelError {
			t.Errorf("warn+ filter returned level %q", row.Level)
		}
	}

	// Attempts are separate rings: attempt 2 rows never leak into 1.
	if _, err := s.StepLogs().CreateBatch(ctx, []gen.CreateStepLogsParams{
		logLine(run.ID, "a", 2, 1, store.LogLevelInfo, "second attempt"),
	}); err != nil {
		t.Fatalf("CreateBatch attempt 2: %v", err)
	}
	stats, err := s.StepLogs().Stats(ctx, run.ID, "a", 1)
	if err != nil || stats.Stored != 20 {
		t.Errorf("attempt 1 Stats = (%+v, %v), want 20 stored", stats, err)
	}
	stats2, err := s.StepLogs().Stats(ctx, run.ID, "a", 2)
	if err != nil || stats2.Stored != 1 || stats2.MaxSeq != 1 {
		t.Errorf("attempt 2 Stats = (%+v, %v), want 1 stored, max_seq 1", stats2, err)
	}
}

func TestStepLogsRequireExistingStep(t *testing.T) {
	t.Parallel()
	s := store.NewFromPool(storetest.NewDB(t))

	// The FK to run_steps keeps orphan diagnostics out (and CASCADE cleans
	// the ring with its run); the async writer treats this as log-and-drop.
	var conflict *store.ConflictError
	if _, err := s.StepLogs().CreateBatch(t.Context(), []gen.CreateStepLogsParams{
		logLine(uuid.New(), "ghost", 1, 1, store.LogLevelInfo, "orphan"),
	}); !errors.As(err, &conflict) {
		t.Errorf("orphan CreateBatch: %v, want *ConflictError (FK)", err)
	}
}
