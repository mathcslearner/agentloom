package api

// Contract and consistency tests for the run-graph projection (ticket 13.6).
// These drive the pure buildRunGraphResponse over fixture rows, so they need
// no database and stay deterministic (fixed timestamps in the inputs).
//
// TestRunGraphContract asserts the wire shape against a committed golden
// (testdata/run_graph_fixture.json) — the exported fixture the M17/M18
// frontend tests consume. Regenerate it with UPDATE_GOLDEN=1.
//
// TestRunGraphVersionConsistency asserts the graph_expanded events and the API
// versions agree: the version count matches, every row's introducing version
// is real, the row-derived and event-derived deltas coincide, and the delta
// feed is linear.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

func sptr(s string) *string { return &s }

// graphFixtureRun is one planner expansion: an authored planner "plan" and
// join "join" (version 1), with "plan" injecting an llm step "work#1" plus the
// edges wiring it into the join (version 2). "join" is widened, "work#1" is
// readied. It exercises every provenance case: an authored node, an injected
// node, an authored edge, injected edges, and one expansion delta.
func graphFixtureRun() (gen.Run, []gen.RunStep, []gen.RunEdge, []gen.Event) {
	runID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	t0 := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 8, 16, 12, 0, 5, 0, time.UTC)

	run := gen.Run{
		ID:           runID,
		Status:       "succeeded",
		GraphVersion: 2,
		StepsTotal:   3,
		CreatedAt:    t0,
	}
	steps := []gen.RunStep{
		{RunID: runID, StepID: "plan", StepType: "planner", Status: "succeeded", Depth: 0, GraphVersion: 1},
		{RunID: runID, StepID: "join", StepType: "join", Status: "succeeded", Depth: 0, GraphVersion: 1},
		{RunID: runID, StepID: "work#1", StepType: "llm", Status: "succeeded", Depth: 1, GraphVersion: 2, OriginStep: sptr("plan"), OriginKind: sptr("planner")},
	}
	edges := []gen.RunEdge{
		{RunID: runID, Ordinal: 0, FromStep: "plan", ToStep: "join", EdgeType: "normal", Resolution: "fired", GraphVersion: 1},
		{RunID: runID, Ordinal: 1, FromStep: "plan", ToStep: "work#1", EdgeType: "normal", Resolution: "fired", GraphVersion: 2, OriginStep: sptr("plan"), OriginKind: sptr("planner")},
		{RunID: runID, Ordinal: 2, FromStep: "work#1", ToStep: "join", EdgeType: "normal", Resolution: "fired", GraphVersion: 2, OriginStep: sptr("plan"), OriginKind: sptr("planner")},
	}
	delta := dag.PlanOutput{
		SchemaVersion: 1,
		Steps:         []dag.Step{{ID: "work#1", Type: dag.StepLLM}},
		Edges: []dag.Edge{
			{From: "plan", To: "work#1"},
			{From: "work#1", To: "join"},
		},
	}
	payload, _ := json.Marshal(store.GraphExpandedEvent{
		OriginStep:  "plan",
		OriginKind:  "planner",
		FromVersion: 1,
		ToVersion:   2,
		Depth:       1,
		Delta:       delta,
		Readied:     []string{"work#1"},
		Widened:     []string{"join"},
	})
	events := []gen.Event{
		{RunID: runID, Seq: 7, Type: store.EventGraphExpanded, Payload: payload, CreatedAt: t1},
	}
	return run, steps, edges, events
}

func TestRunGraphContract(t *testing.T) {
	t.Parallel()
	resp, err := buildRunGraphResponse(graphFixtureRun())
	if err != nil {
		t.Fatalf("buildRunGraphResponse: %v", err)
	}
	got, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	golden := filepath.Join("testdata", "run_graph_fixture.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading golden (run with UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("graph response does not match golden %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}

func TestRunGraphVersionConsistency(t *testing.T) {
	t.Parallel()
	run, steps, edges, events := graphFixtureRun()
	resp, err := buildRunGraphResponse(run, steps, edges, events)
	if err != nil {
		t.Fatalf("buildRunGraphResponse: %v", err)
	}

	// The current version equals expansion count + 1, and the expansion feed
	// has exactly one entry per graph_expanded event.
	if resp.GraphVersion != int(run.GraphVersion) {
		t.Errorf("graph_version = %d, want %d", resp.GraphVersion, run.GraphVersion)
	}
	if got, want := len(resp.Expansions), int(run.GraphVersion)-1; got != want {
		t.Fatalf("expansions = %d, want %d (graph_version - 1)", got, want)
	}
	if len(resp.Expansions) != len(events) {
		t.Fatalf("expansions = %d, want %d (one per graph_expanded event)", len(resp.Expansions), len(events))
	}

	// The delta feed is linear: each expansion's from_version is the previous
	// to_version, and version = from_version + 1.
	prev := 1
	for i, e := range resp.Expansions {
		if e.FromVersion != prev {
			t.Errorf("expansion[%d] from_version = %d, want %d", i, e.FromVersion, prev)
		}
		if e.Version != e.FromVersion+1 {
			t.Errorf("expansion[%d] version = %d, want from_version+1 = %d", i, e.Version, e.FromVersion+1)
		}
		prev = e.Version
	}

	// Every node's introducing version is either 1 (authored) or the version
	// of some expansion; the row-derived added set equals the event delta.
	rowSteps := map[int][]string{}
	for _, n := range resp.Nodes {
		if n.GraphVersion != 1 && !hasExpansionVersion(resp.Expansions, n.GraphVersion) {
			t.Errorf("node %q graph_version %d has no matching expansion", n.ID, n.GraphVersion)
		}
		rowSteps[n.GraphVersion] = append(rowSteps[n.GraphVersion], n.ID)
	}
	for _, e := range resp.Expansions {
		assertSameSet(t, "added_steps for version "+strconv.Itoa(e.Version), rowSteps[e.Version], e.AddedSteps)
	}

	// Injected rows carry the origin the event named; authored rows report
	// "definition".
	for _, n := range resp.Nodes {
		switch n.ID {
		case "plan", "join":
			if n.Origin.Kind != "definition" || n.Origin.Step != "" {
				t.Errorf("node %q origin = %+v, want definition", n.ID, n.Origin)
			}
		case "work#1":
			if n.Origin.Kind != "planner" || n.Origin.Step != "plan" {
				t.Errorf("node %q origin = %+v, want planner/plan", n.ID, n.Origin)
			}
			if n.Depth != 1 || n.GraphVersion != 2 {
				t.Errorf("node %q depth/version = %d/%d, want 1/2", n.ID, n.Depth, n.GraphVersion)
			}
		}
	}

	// added_at resolves through the version->time map: authored nodes carry
	// the run's creation time, injected nodes the expansion event's time.
	for _, n := range resp.Nodes {
		if n.GraphVersion == 1 && !n.AddedAt.Equal(run.CreatedAt) {
			t.Errorf("authored node %q added_at = %s, want run created_at %s", n.ID, n.AddedAt, run.CreatedAt)
		}
		if n.GraphVersion == 2 && !n.AddedAt.Equal(events[0].CreatedAt) {
			t.Errorf("injected node %q added_at = %s, want event time %s", n.ID, n.AddedAt, events[0].CreatedAt)
		}
	}
}

func hasExpansionVersion(exps []GraphExpansionView, v int) bool {
	for _, e := range exps {
		if e.Version == v {
			return true
		}
	}
	return false
}

func assertSameSet(t *testing.T, label string, a, b []string) {
	t.Helper()
	set := map[string]int{}
	for _, s := range a {
		set[s]++
	}
	for _, s := range b {
		set[s]--
	}
	for s, n := range set {
		if n != 0 {
			t.Errorf("%s: sets differ on %q (got %v, want %v)", label, s, a, b)
			return
		}
	}
}
