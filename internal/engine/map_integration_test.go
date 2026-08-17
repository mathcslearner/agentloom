//go:build integration

package engine_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 13.4 headline e2e (ADR-015): dynamic map fan-out. A `map` step
// instantiates one instance of its body sub-template per runtime list item plus
// a generated gather join, applied through store.ExpandRun — the planner's
// primitive with an engine-generated delta. These tests prove the (reduced)
// acceptance criteria: a list fans out to per-item llm instances whose ordered
// results the gather collects; a list over the cap is a typed permanent
// failure at expansion time; and an item failure honors fail-fast.

// mapFixture wires a store, harness, an unscripted mock (its structured/echo
// defaults let instances run offline), the map + gather + llm + echo executors,
// and a production dispatcher into one engine.
type mapFixture struct {
	s *store.Store
	h *queuetest.Harness
}

func newMapFixture(t *testing.T, rules []llm.MockRule) *mapFixture {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	mock, err := llm.NewMock(llm.MockConfig{Rules: rules})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(
		exec.MapExecutor{}, exec.GatherExecutor{},
		exec.NewLLMExecutor(providers), exec.EchoExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "map-worker", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("map-worker", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return &mapFixture{s: s, h: h}
}

func (f *mapFixture) submit(t *testing.T, defJSON string) uuid.UUID {
	t.Helper()
	res, err := f.s.CreateRun(t.Context(), store.CreateRunArgs{Definition: mustDecode(t, defJSON), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return res.Run.ID
}

// mapDef builds a source(echo list) → process(map over analyze_one) definition
// with the given items list and optional overrides on the map/expansion.
func mapDef(t *testing.T, items []string, mapExtra, expansion string) string {
	t.Helper()
	itemsJSON, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshaling items: %v", err)
	}
	return fmt.Sprintf(`{
		"schema_version": 1, "name": "map-e2e",%s
		"templates": {
			"analyze_one": {"steps": [
				{"id": "analyze", "type": "llm",
				 "config": {"model": "mock/sim-1", "prompt": "Analyze ${{ item_index }}: ${{ item }}", "max_tokens": 32, "temperature": 0}}
			]}
		},
		"steps": [
			{"id": "source", "type": "echo", "config": {"input": {"items": %s}}},
			{"id": "process", "type": "map", "config": {"items": "${{ steps.source.output.items }}", "body": "analyze_one"%s}}
		],
		"edges": [{"from": "source", "to": "process"}]
	}`, expansion, itemsJSON, mapExtra)
}

// TestMapFansOutAndGathers (criterion 1): a 20-element list fans out to 20 llm
// instances whose ordered results the generated gather collects.
func TestMapFansOutAndGathers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMapFixture(t, nil) // unscripted mock echoes each prompt

	const n = 20
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("item-%d", i)
	}
	runID := f.submit(t, mapDef(t, items, `, "max_items": 100`, ""))
	run := waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	// One committed expansion: graph_version 2, steps_total = 2 authored + 20
	// instances + 1 gather = 23.
	if run.GraphVersion != 2 {
		t.Errorf("graph_version = %d, want 2 (one expansion)", run.GraphVersion)
	}
	if run.StepsTotal != n+3 {
		t.Errorf("steps_total = %d, want %d", run.StepsTotal, n+3)
	}
	ges := graphExpandedEvents(t, f.s, runID)
	if len(ges) != 1 {
		t.Fatalf("graph_expanded events = %d, want 1", len(ges))
	}
	if ges[0].OriginStep != "process" || ges[0].OriginKind != string(dag.OriginMap) {
		t.Errorf("graph_expanded origin = %s/%s, want process/map", ges[0].OriginStep, ges[0].OriginKind)
	}

	steps := requireSteps(t, f.s, runID)
	// Every instance succeeded and carries map provenance (depth 1).
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("analyze#%d", i)
		st, ok := steps[id]
		if !ok {
			t.Fatalf("missing instance %q", id)
		}
		if st.Status != store.StepStatusSucceeded {
			t.Errorf("%s status = %s, want succeeded", id, st.Status)
		}
		if st.Depth != 1 || st.OriginStep == nil || *st.OriginStep != "process" ||
			st.OriginKind == nil || *st.OriginKind != string(dag.OriginMap) {
			t.Errorf("%s provenance = depth %d origin %v/%v, want depth 1 process/map",
				id, st.Depth, st.OriginStep, st.OriginKind)
		}
	}

	// The gather emitted the ordered array of the 20 per-instance outputs; the
	// k-th entry cites item-k (order preserved).
	gather, ok := steps[dag.MapGatherID("process")]
	if !ok {
		t.Fatalf("missing gather step %q", dag.MapGatherID("process"))
	}
	if gather.Status != store.StepStatusSucceeded {
		t.Fatalf("gather status = %s, want succeeded", gather.Status)
	}
	var results []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(gather.Output, &results); err != nil {
		t.Fatalf("gather output is not an array of outputs: %v (%s)", err, gather.Output)
	}
	if len(results) != n {
		t.Fatalf("gather collected %d results, want %d", len(results), n)
	}
	for i := 0; i < n; i++ {
		if !strings.Contains(results[i].Text, fmt.Sprintf("item-%d", i)) {
			t.Errorf("gather result[%d] = %q, want it to cite item-%d", i, results[i].Text, i)
		}
	}

	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}

// TestMapCapExceededPermanent (criterion 2): a list over the per-expansion cap
// is rejected at expansion time and the map fails permanently — the graph never
// moves, and there is no semantic retry.
func TestMapCapExceededPermanent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMapFixture(t, nil)

	// Three items but a run per-expansion cap of 2 steps: 3 instances + 1 gather
	// exceed it, so ValidateExpansion rejects with CapExceeded.
	runID := f.submit(t, mapDef(t, []string{"a", "b", "c"}, "", `"expansion": {"max_added_steps": 2},`))
	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	run, err := f.s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.GraphVersion != 1 {
		t.Errorf("graph_version = %d, want 1 (no expansion committed)", run.GraphVersion)
	}
	if got := len(graphExpandedEvents(t, f.s, runID)); got != 0 {
		t.Errorf("graph_expanded events = %d, want 0", got)
	}
	// The map dead-lettered permanently, one attempt (no semantic retry), the
	// reason carrying expansion_cap_exceeded.
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "process")
	if err != nil {
		t.Fatalf("listing map attempts: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("map attempts = %d, want 1", len(atts))
	}
	if atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomePermanent {
		t.Errorf("attempt outcome = %v, want permanent", atts[0].Outcome)
	}
	dls, err := f.s.DeadLetters().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 || !strings.Contains(string(dls[0].Error), "expansion_cap_exceeded") {
		t.Fatalf("dead letters = %+v, want one carrying expansion_cap_exceeded", dls)
	}
	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}

// TestMapMaxItemsPermanent: a list longer than the map's own max_items is a
// typed permanent failure in the executor, before any expansion.
func TestMapMaxItemsPermanent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMapFixture(t, nil)

	runID := f.submit(t, mapDef(t, []string{"a", "b", "c"}, `, "max_items": 2`, ""))
	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	run, err := f.s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.GraphVersion != 1 {
		t.Errorf("graph_version = %d, want 1 (executor rejected before expansion)", run.GraphVersion)
	}
	dls, err := f.s.DeadLetters().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 || !strings.Contains(string(dls[0].Error), "max_items") {
		t.Fatalf("dead letters = %+v, want one carrying max_items", dls)
	}
	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}

// TestMapItemFailureFailFast (criterion 3, fail-fast): an instance whose llm
// call fails permanently dead-letters, and under the default fail_fast policy
// the run fails — the gather never produces a partial result.
func TestMapItemFailureFailFast(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// The instance analyzing "boom" gets a permanent 400 from the mock.
	f := newMapFixture(t, []llm.MockRule{
		{Substring: "boom", Respond: []llm.MockOutcome{{Status: 400, Message: "scripted item failure"}}},
	})

	runID := f.submit(t, mapDef(t, []string{"ok1", "boom", "ok3"}, `, "max_items": 100`, ""))
	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	steps := requireSteps(t, f.s, runID)
	// The expansion committed (the failure is a per-item failure, not an
	// expansion rejection), so the instances exist; the boom instance
	// dead-lettered and the gather never completed.
	boom, ok := steps["analyze#1"]
	if !ok {
		t.Fatalf("missing instance analyze#1")
	}
	if boom.Status != store.StepStatusDeadLettered {
		t.Errorf("analyze#1 status = %s, want dead_lettered", boom.Status)
	}
	if g, ok := steps[dag.MapGatherID("process")]; ok && g.Status == store.StepStatusSucceeded {
		t.Errorf("gather succeeded, but fail-fast should have failed the run before it gathered")
	}
	dls, err := f.s.DeadLetters().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) == 0 {
		t.Fatal("want at least one dead letter for the failed item")
	}
	f.h.RequireHandledOncePerClaim()
}
