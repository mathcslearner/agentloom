package dag_test

import (
	"slices"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// set builds a step-ID set.
func set(ids ...string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestReadySteps(t *testing.T) {
	t.Parallel()

	chain := mkDef(noops("a", "b", "c"),
		dag.Edge{From: "a", To: "b"},
		dag.Edge{From: "b", To: "c"},
	)

	// The ADR-003 worked example: fetch → route(branch) fans out into three
	// flows that fan back into gather(join …) → reply. The engine seeds the
	// skipped set with the flows the branch rule passed over.
	branchFanIn := func(mode dag.JoinMode) *dag.Definition {
		return mkDef(
			[]dag.Step{
				{ID: "fetch", Type: dag.StepNoop},
				{ID: "route", Type: dag.StepBranch},
				{ID: "refund_flow", Type: dag.StepNoop},
				{ID: "billing_flow", Type: dag.StepNoop},
				{ID: "generic_flow", Type: dag.StepNoop},
				join("gather", mode),
				{ID: "reply", Type: dag.StepNoop},
			},
			dag.Edge{From: "fetch", To: "route"},
			dag.Edge{From: "route", To: "refund_flow", When: "output.category == 'refund'"},
			dag.Edge{From: "route", To: "billing_flow", When: "output.category == 'billing'"},
			dag.Edge{From: "route", To: "generic_flow"},
			dag.Edge{From: "refund_flow", To: "gather"},
			dag.Edge{From: "billing_flow", To: "gather"},
			dag.Edge{From: "generic_flow", To: "gather"},
			dag.Edge{From: "gather", To: "reply"},
		)
	}

	// Two parents into one join.
	fanIn := func(mode dag.JoinMode) *dag.Definition {
		return mkDef(
			[]dag.Step{{ID: "a", Type: dag.StepNoop}, {ID: "b", Type: dag.StepNoop}, join("j", mode)},
			dag.Edge{From: "a", To: "j"},
			dag.Edge{From: "b", To: "j"},
		)
	}

	// Two parents into a plain (non-join) step.
	fanInNoop := mkDef(noops("a", "b", "c"),
		dag.Edge{From: "a", To: "c"},
		dag.Edge{From: "b", To: "c"},
	)

	// Critic loop: the loop edge must not affect entry or readiness.
	criticLoop := mkDef(noops("draft", "critique", "send"),
		dag.Edge{From: "draft", To: "critique"},
		dag.Edge{From: "critique", To: "send"},
		dag.Edge{From: "critique", To: "draft", Type: dag.EdgeLoop, Condition: "output.verdict == 'revise'", MaxIterations: 3},
	)

	cases := []struct {
		name                        string
		def                         *dag.Definition
		completed, skipped, failed  map[string]bool
		wantReady, wantNewlySkipped []string
	}{
		{
			name: "entry step ready at run creation",
			def:  chain, wantReady: []string{"a"},
		},
		{
			name: "chain advances one step",
			def:  chain, completed: set("a"), wantReady: []string{"b"},
		},
		{
			name: "completed steps are not re-returned",
			def:  chain, completed: set("a", "b", "c"),
		},
		{
			name: "skip cascades down the chain",
			def:  chain, skipped: set("a"), wantNewlySkipped: []string{"b", "c"},
		},
		{
			name:      "branch fan-in per the ADR worked example",
			def:       branchFanIn(dag.JoinAll),
			completed: set("fetch", "route", "billing_flow"),
			skipped:   set("refund_flow", "generic_flow"),
			wantReady: []string{"gather"},
		},
		{
			// generic_flow is still pending (its route edge defaults to fired
			// until the engine resolves it), so the join-all edge from it is
			// unresolved and gather must wait.
			name:      "join all waits for the skip resolution",
			def:       branchFanIn(dag.JoinAll),
			completed: set("fetch", "route", "billing_flow"),
			skipped:   set("refund_flow"),
			wantReady: []string{"generic_flow"},
		},
		{
			// Without seeded skips, the sibling flows are ready too (their
			// route edges default to fired) — but gather(any) is already
			// ready off billing_flow alone.
			name:      "join any fires before siblings resolve",
			def:       branchFanIn(dag.JoinAny),
			completed: set("fetch", "route", "billing_flow"),
			wantReady: []string{"refund_flow", "generic_flow", "gather"},
		},
		{
			name: "join all with skipped and fired parents",
			def:  fanIn(dag.JoinAll), completed: set("a"), skipped: set("b"),
			wantReady: []string{"j"},
		},
		{
			name: "join all blocked by a failed parent",
			def:  fanIn(dag.JoinAll), completed: set("a"), failed: set("b"),
		},
		{
			name: "join all skipped when every parent skipped",
			def:  fanIn(dag.JoinAll), skipped: set("a", "b"),
			wantNewlySkipped: []string{"j"},
		},
		{
			name: "join any fires despite a failed sibling",
			def:  fanIn(dag.JoinAny), completed: set("a"), failed: set("b"),
			wantReady: []string{"j"},
		},
		{
			name: "join any blocked when all parents failed",
			def:  fanIn(dag.JoinAny), failed: set("a", "b"),
		},
		{
			name: "join any skipped when every parent skipped",
			def:  fanIn(dag.JoinAny), skipped: set("a", "b"),
			wantNewlySkipped: []string{"j"},
		},
		{
			name: "non-join waits for all parents",
			def:  fanInNoop, completed: set("a"), wantReady: []string{"b"},
		},
		{
			name: "non-join ready once all parents resolved",
			def:  fanInNoop, completed: set("a", "b"), wantReady: []string{"c"},
		},
		{
			name: "non-join ready with one fired one skipped",
			def:  fanInNoop, completed: set("a"), skipped: set("b"),
			wantReady: []string{"c"},
		},
		{
			name: "non-join blocked by a failed parent",
			def:  fanInNoop, completed: set("a"), failed: set("b"),
		},
		{
			name: "loop edge does not create a dependency",
			def:  criticLoop, wantReady: []string{"draft"},
		},
		{
			name: "loop edge does not fire readiness backward",
			def:  criticLoop, completed: set("draft"), wantReady: []string{"critique"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, err := dag.NewGraph(tc.def)
			if err != nil {
				t.Fatalf("NewGraph: %v", err)
			}
			ready, newlySkipped, err := g.ReadySteps(tc.completed, tc.skipped, tc.failed)
			if err != nil {
				t.Fatalf("ReadySteps: %v", err)
			}
			if !slices.Equal(ready, tc.wantReady) {
				t.Errorf("ready = %v, want %v", ready, tc.wantReady)
			}
			if !slices.Equal(newlySkipped, tc.wantNewlySkipped) {
				t.Errorf("newlySkipped = %v, want %v", newlySkipped, tc.wantNewlySkipped)
			}
		})
	}
}

func TestReadyStepsInputErrors(t *testing.T) {
	t.Parallel()

	g, err := dag.NewGraph(mkDef(noops("a", "b"), dag.Edge{From: "a", To: "b"}))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	if _, _, err := g.ReadySteps(set("ghost"), nil, nil); err == nil {
		t.Error("unknown step in completed: want error, got nil")
	}
	if _, _, err := g.ReadySteps(set("a"), set("a"), nil); err == nil {
		t.Error("overlapping sets: want error, got nil")
	}
}

// TestReadyStepsJoinMissingConfigFallsBackToAll pins the documented
// fallback in joinMode: a join step whose config is absent (possible on
// hand-built definitions; Validate flags it separately) gets the stricter
// all-shaped readiness rule, not join-any.
func TestReadyStepsJoinMissingConfigFallsBackToAll(t *testing.T) {
	t.Parallel()

	g, err := dag.NewGraph(mkDef(
		[]dag.Step{
			{ID: "a", Type: dag.StepNoop},
			{ID: "b", Type: dag.StepNoop},
			{ID: "j", Type: dag.StepJoin}, // no config
		},
		dag.Edge{From: "a", To: "j"},
		dag.Edge{From: "b", To: "j"},
	))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}

	// One parent fired, one pending: join-any would fire; the config-less
	// join must wait like join-all. Only the untouched entry step is ready.
	ready, newlySkipped, err := g.ReadySteps(set("a"), nil, nil)
	if err != nil {
		t.Fatalf("ReadySteps: %v", err)
	}
	if want := []string{"b"}; !slices.Equal(ready, want) {
		t.Errorf("ready = %v, want %v", ready, want)
	}
	if len(newlySkipped) != 0 {
		t.Errorf("newlySkipped = %v, want none", newlySkipped)
	}

	ready, _, err = g.ReadySteps(set("a", "b"), nil, nil)
	if err != nil {
		t.Fatalf("ReadySteps: %v", err)
	}
	if want := []string{"j"}; !slices.Equal(ready, want) {
		t.Errorf("ready after both parents = %v, want %v", ready, want)
	}
}

func TestReadyStepsCyclicGraphError(t *testing.T) {
	t.Parallel()

	g, err := dag.NewGraph(mkDef(noops("a", "b"),
		dag.Edge{From: "a", To: "b"},
		dag.Edge{From: "b", To: "a"},
	))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	if _, _, err := g.ReadySteps(nil, nil, nil); err == nil {
		t.Error("ReadySteps on cyclic graph: want error, got nil")
	}
}
