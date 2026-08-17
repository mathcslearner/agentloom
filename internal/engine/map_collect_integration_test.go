//go:build integration

package engine_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 13.4b (ADR-015): the map collect_errors policy. A map instance that
// fails terminally is tolerated — settled `collected` with an error marker, its
// edge to the gather fired — so the gather fires on all-terminal and the run
// still succeeds with an error slot in the ordered result array.

// collectMarker is the shape of the engine-synthesized error slot a collected
// instance contributes to the gathered array.
type collectMarker struct {
	MapItemFailed bool   `json:"map_item_failed"`
	Class         string `json:"class"`
}

// TestMapCollectErrorsSucceeds (criteria 1+2): one item fails, the gather still
// fires on all-terminal and emits the ordered array with an error marker in the
// failed slot; the run succeeds.
func TestMapCollectErrorsSucceeds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// The instance analyzing "boom" gets a permanent 400 from the mock.
	f := newMapFixture(t, []llm.MockRule{
		{Substring: "boom", Respond: []llm.MockOutcome{{Status: 400, Message: "scripted item failure"}}},
	})

	runID := f.submit(t, mapDef(t, []string{"ok1", "boom", "ok3"}, `, "max_items": 100, "on_item_failure": "collect_errors"`, ""))
	run := waitRun(t, f.s, runID, store.RunStatusSucceeded) // the run SUCCEEDS despite the failed item
	f.h.WaitQuiescent(ctx)

	// One committed expansion; the failed item counts as collected, not failed.
	if run.GraphVersion != 2 {
		t.Errorf("graph_version = %d, want 2", run.GraphVersion)
	}
	if run.StepsCollected != 1 {
		t.Errorf("steps_collected = %d, want 1", run.StepsCollected)
	}
	if run.StepsFailed != 0 {
		t.Errorf("steps_failed = %d, want 0 (collect_errors tolerates the item failure)", run.StepsFailed)
	}

	steps := requireSteps(t, f.s, runID)
	if got := steps["analyze#1"].Status; got != store.StepStatusCollected {
		t.Errorf("analyze#1 status = %s, want collected", got)
	}
	for _, id := range []string{"analyze#0", "analyze#2"} {
		if got := steps[id].Status; got != store.StepStatusSucceeded {
			t.Errorf("%s status = %s, want succeeded", id, got)
		}
	}

	// The gather emitted the ordered array: ok1's output, the error marker,
	// ok3's output — in list order.
	gather := steps[dag.MapGatherID("process")]
	if gather.Status != store.StepStatusSucceeded {
		t.Fatalf("gather status = %s, want succeeded", gather.Status)
	}
	var results []json.RawMessage
	if err := json.Unmarshal(gather.Output, &results); err != nil {
		t.Fatalf("gather output is not an array: %v (%s)", err, gather.Output)
	}
	if len(results) != 3 {
		t.Fatalf("gather collected %d results, want 3", len(results))
	}
	// Slot 1 is the error marker.
	var marker collectMarker
	if err := json.Unmarshal(results[1], &marker); err != nil || !marker.MapItemFailed {
		t.Errorf("gather result[1] = %s, want a map_item_failed marker (err %v)", results[1], err)
	}
	if marker.Class != string(dag.ClassPermanent) {
		t.Errorf("marker class = %q, want permanent", marker.Class)
	}
	// Slots 0 and 2 are real analyses citing their items.
	for slot, item := range map[int]string{0: "ok1", 2: "ok3"} {
		var out struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(results[slot], &out); err != nil {
			t.Fatalf("gather result[%d] not an output: %v", slot, err)
		}
		if !strings.Contains(out.Text, item) {
			t.Errorf("gather result[%d] = %q, want it to cite %q", slot, out.Text, item)
		}
	}

	// The tolerated item still recorded its real failure class on the attempt.
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "analyze#1")
	if err != nil {
		t.Fatalf("listing analyze#1 attempts: %v", err)
	}
	if len(atts) == 0 || atts[len(atts)-1].Outcome == nil || *atts[len(atts)-1].Outcome != store.AttemptOutcomePermanent {
		t.Errorf("analyze#1 last attempt outcome = %v, want permanent (the real failure, recorded)", atts)
	}
	// A collected item is tolerated, not dead-lettered — no DLQ row.
	dls, err := f.s.DeadLetters().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 0 {
		t.Errorf("dead letters = %d, want 0 (a collected item is not dead-lettered)", len(dls))
	}

	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}

// TestMapCollectAllFail: every item fails; all instances collect, the gather
// emits an all-error array, and the run still succeeds (the tolerate-everything
// corner).
func TestMapCollectAllFail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMapFixture(t, []llm.MockRule{
		{Substring: "Analyze", Respond: []llm.MockOutcome{{Status: 400, Message: "all fail"}}},
	})

	const n = 4
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("x%d", i)
	}
	runID := f.submit(t, mapDef(t, items, `, "max_items": 100, "on_item_failure": "collect_errors"`, ""))
	run := waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	if run.StepsCollected != n || run.StepsFailed != 0 {
		t.Errorf("steps_collected/steps_failed = %d/%d, want %d/0", run.StepsCollected, run.StepsFailed, n)
	}
	steps := requireSteps(t, f.s, runID)
	gather := steps[dag.MapGatherID("process")]
	var results []collectMarker
	if err := json.Unmarshal(gather.Output, &results); err != nil {
		t.Fatalf("gather output: %v (%s)", err, gather.Output)
	}
	if len(results) != n {
		t.Fatalf("gather collected %d results, want %d", len(results), n)
	}
	for i, r := range results {
		if !r.MapItemFailed {
			t.Errorf("result[%d] = %+v, want an error marker", i, r)
		}
	}
	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}
