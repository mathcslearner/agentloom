package api

// Contract goldens for the ops views (ticket 18.6): the cross-run dead-letter
// list and the queue-health system stats. Like the run-detail / approval-list
// fixtures (18.3/18.5) these build the wire views over fixed data — no
// database, deterministic timestamps — and pin the shape against committed JSON
// the frontend ops-view tests read as ground truth. Regenerate with
// UPDATE_GOLDEN=1.
//
//   TestDeadLetterListFixtureGolden -> testdata/dead_letter_list_fixture.json
//   TestSystemStatsFixtureGolden    -> testdata/system_stats_fixture.json

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

func TestDeadLetterListFixtureGolden(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	runID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	defID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	sptr := func(s string) *string { return &s }

	// The join rows the store returns, covering every triage case in one page:
	//   - an OPEN step (still dead_lettered) from retries_exhausted, on a
	//     running run with a definition — the requeueable working set;
	//   - a permanent poison death whose step is now succeeded (a later
	//     requeue fixed it) — CLOSED, class absent, no definition id (inline run);
	//   - a permanent death whose run failed — CLOSED.
	rows := []gen.ListDeadLettersPageRow{
		{
			RunID: runID, StepID: "flaky", Seq: 1, Source: "retries_exhausted",
			Class:           sptr("transient"),
			Error:           json.RawMessage(`{"class":"transient","message":"upstream 503"}`),
			AttemptsAtDeath: 3,
			CreatedAt:       t0.Add(2 * time.Second),
			StepStatus:      "dead_lettered", StepType: "llm",
			RunStatus: "running", DefinitionID: &defID,
		},
		{
			RunID: runID, StepID: "fetch", Seq: 2, Source: "poison",
			Error:           json.RawMessage(`{"message":"bad envelope"}`),
			AttemptsAtDeath: 6,
			CreatedAt:       t0.Add(1 * time.Second),
			StepStatus:      "succeeded", StepType: "tool",
			RunStatus: "running", DefinitionID: nil,
		},
		{
			RunID: runID, StepID: "publish", Seq: 1, Source: "permanent",
			Class:           sptr("permanent"),
			Error:           json.RawMessage(`{"class":"permanent","message":"host not allowed"}`),
			AttemptsAtDeath: 1,
			CreatedAt:       t0,
			StepStatus:      "dead_lettered", StepType: "tool",
			RunStatus: "failed", DefinitionID: &defID,
		},
	}

	resp := DeadLetterListResponse{DeadLetters: []DeadLetterListItem{}}
	for _, row := range rows {
		resp.DeadLetters = append(resp.DeadLetters, buildDeadLetterListItem(row))
	}
	last := rows[len(rows)-1]
	resp.NextCursor = encodeDeadLetterCursor(store.DeadLetterCursor{
		CreatedAt: last.CreatedAt, RunID: last.RunID, StepID: last.StepID, Seq: last.Seq,
	})

	assertGolden(t, "dead_letter_list_fixture.json", resp)
}

func TestSystemStatsFixtureGolden(t *testing.T) {
	t.Parallel()

	observed := time.Date(2026, 8, 19, 12, 0, 5, 0, time.UTC)
	oldest := int64(1200)

	resp := SystemStatsResponse{
		ObservedAt: observed,
		Queue: &QueueStatsView{
			Stream:        "steps:ready",
			Group:         "workers",
			ReadyDepth:    42,
			Pending:       7,
			Delayed:       3,
			Length:        49,
			LagKnown:      true,
			WorkersActive: 2,
			Workers: []ConsumerView{
				{ID: "host-a-1234-ab12", IdleMs: 40, Pending: 4, Active: true},
				{ID: "host-b-5678-cd34", IdleMs: 120, Pending: 3, Active: true},
				{ID: "host-c-9012-ef56", IdleMs: 60000, Pending: 0, Active: false},
			},
		},
		Outbox:      OutboxStatsView{Backlog: 5, OldestAgeMs: &oldest},
		DeadLetters: DeadLetterStatsView{Open: 2},
		Runs:        RunStatsView{Active: 11},
	}

	assertGolden(t, "system_stats_fixture.json", resp)
}
