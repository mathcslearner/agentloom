package dag_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// mkDef assembles a definition from bare steps and edges for graph tests.
func mkDef(steps []dag.Step, edges ...dag.Edge) *dag.Definition {
	return &dag.Definition{
		SchemaVersion: dag.CurrentSchemaVersion,
		Name:          "t",
		Steps:         steps,
		Edges:         edges,
	}
}

// noops builds noop steps with the given IDs.
func noops(ids ...string) []dag.Step {
	steps := make([]dag.Step, len(ids))
	for i, id := range ids {
		steps[i] = dag.Step{ID: id, Type: dag.StepNoop}
	}
	return steps
}

// join builds a join step with the given mode.
func join(id string, mode dag.JoinMode) dag.Step {
	return dag.Step{ID: id, Type: dag.StepJoin, Config: &dag.JoinConfig{Mode: mode}}
}

func TestNewGraphRejectsUnbuildable(t *testing.T) {
	t.Parallel()

	if _, err := dag.NewGraph(nil); err == nil {
		t.Error("NewGraph(nil): want error, got nil")
	}
	if _, err := dag.NewGraph(mkDef(noops("a", "a"))); err == nil {
		t.Error("NewGraph with duplicate IDs: want error, got nil")
	}
	if _, err := dag.NewGraph(mkDef(noops("a"), dag.Edge{From: "a", To: "ghost"})); err == nil {
		t.Error("NewGraph with unknown endpoint: want error, got nil")
	}
	if _, err := dag.NewGraph(mkDef(noops("a"), dag.Edge{From: "ghost", To: "a"})); err == nil {
		t.Error("NewGraph with unknown source: want error, got nil")
	}
}

func TestTopoOrder(t *testing.T) {
	t.Parallel()

	// Diamond with a loop edge back into it; the loop edge must not affect
	// the order.
	def := mkDef(noops("a", "b", "c", "d"),
		dag.Edge{From: "a", To: "b"},
		dag.Edge{From: "a", To: "c"},
		dag.Edge{From: "b", To: "d"},
		dag.Edge{From: "c", To: "d"},
		dag.Edge{From: "d", To: "a", Type: dag.EdgeLoop, Condition: "output.go", MaxIterations: 2},
	)
	g, err := dag.NewGraph(def)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	order, err := g.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	if want := []string{"a", "b", "c", "d"}; !slices.Equal(order, want) {
		t.Errorf("TopoOrder = %v, want %v", order, want)
	}
	// Deterministic across calls.
	again, err := g.TopoOrder()
	if err != nil || !slices.Equal(order, again) {
		t.Errorf("TopoOrder not deterministic: %v vs %v (err %v)", order, again, err)
	}
}

func TestTopoOrderCycleError(t *testing.T) {
	t.Parallel()

	g, err := dag.NewGraph(mkDef(noops("a", "b"),
		dag.Edge{From: "a", To: "b"},
		dag.Edge{From: "b", To: "a"},
	))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	if _, err := g.TopoOrder(); err == nil {
		t.Error("TopoOrder on cyclic graph: want error, got nil")
	}
}

func TestAncestors(t *testing.T) {
	t.Parallel()

	def := mkDef(noops("a", "b", "c", "d", "e"),
		dag.Edge{From: "a", To: "b"},
		dag.Edge{From: "b", To: "c"},
		dag.Edge{From: "d", To: "c"},
		// e is disconnected; the loop edge below must not create ancestry.
		dag.Edge{From: "c", To: "b", Type: dag.EdgeLoop, Condition: "output.go", MaxIterations: 2},
	)
	g, err := dag.NewGraph(def)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	cases := []struct {
		step string
		want []string
	}{
		{"a", nil},
		{"b", []string{"a"}},
		{"c", []string{"a", "b", "d"}},
		{"e", nil},
	}
	for _, tc := range cases {
		got, err := g.Ancestors(tc.step)
		if err != nil {
			t.Fatalf("Ancestors(%q): %v", tc.step, err)
		}
		var ids []string
		for id := range got {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, tc.want) {
			t.Errorf("Ancestors(%q) = %v, want %v", tc.step, ids, tc.want)
		}
	}
	if _, err := g.Ancestors("ghost"); err == nil {
		t.Error("Ancestors(unknown): want error, got nil")
	}
}

// TestValidateCycleRules is the 1.4 acceptance: loop-marked cycles are
// accepted, every other cycle is rejected with the offending path in the
// error message.
func TestValidateCycleRules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		def         *dag.Definition
		wantErrs    []issueRef
		wantInError string
	}{
		{
			name: "critic loop accepted",
			def: mkDef(noops("draft", "critique", "send"),
				dag.Edge{From: "draft", To: "critique"},
				dag.Edge{From: "critique", To: "send"},
				dag.Edge{From: "critique", To: "draft", Type: dag.EdgeLoop, Condition: "output.verdict == 'revise'", MaxIterations: 3},
			),
		},
		{
			name: "two-cycle rejected with path",
			def: mkDef(noops("start", "a", "b"),
				dag.Edge{From: "start", To: "a"},
				dag.Edge{From: "a", To: "b"},
				dag.Edge{From: "b", To: "a"},
			),
			wantErrs:    []issueRef{{dag.CodeCycle, "edges[2]"}},
			wantInError: "a -> b -> a",
		},
		{
			name: "self-loop rejected",
			def: mkDef(noops("start", "a"),
				dag.Edge{From: "start", To: "a"},
				dag.Edge{From: "a", To: "a"},
			),
			wantErrs:    []issueRef{{dag.CodeCycle, "edges[1]"}},
			wantInError: "a -> a",
		},
		{
			name: "two independent cycles both reported",
			def: mkDef(noops("e", "a", "b", "c", "d"),
				dag.Edge{From: "e", To: "a"},
				dag.Edge{From: "e", To: "c"},
				dag.Edge{From: "a", To: "b"},
				dag.Edge{From: "b", To: "a"},
				dag.Edge{From: "c", To: "d"},
				dag.Edge{From: "d", To: "c"},
			),
			wantErrs: []issueRef{
				{dag.CodeCycle, "edges[3]"},
				{dag.CodeCycle, "edges[5]"},
			},
		},
		{
			name: "self loop edge fails ancestry",
			def: mkDef(noops("a", "b"),
				dag.Edge{From: "a", To: "b"},
				dag.Edge{From: "b", To: "b", Type: dag.EdgeLoop, Condition: "output.go", MaxIterations: 2},
			),
			wantErrs: []issueRef{{dag.CodeLoopEdgeNotAncestor, "edges[1]"}},
		},
		{
			name: "cycle and bad loop edge reported together",
			def: mkDef(noops("start", "a", "b", "c"),
				dag.Edge{From: "start", To: "a"},
				dag.Edge{From: "a", To: "b"},
				dag.Edge{From: "b", To: "a"},
				dag.Edge{From: "a", To: "c", Type: dag.EdgeLoop, Condition: "output.go", MaxIterations: 2},
			),
			wantErrs: []issueRef{
				{dag.CodeCycle, "edges[2]"},
				{dag.CodeLoopEdgeNotAncestor, "edges[3]"},
			},
		},
		{
			name: "graph rules skipped when an endpoint is unknown",
			def: mkDef(noops("start", "a", "b"),
				dag.Edge{From: "start", To: "a"},
				dag.Edge{From: "a", To: "b"},
				dag.Edge{From: "b", To: "a"}, // a⇄b would be a cycle, but the gate holds
				dag.Edge{From: "b", To: "ghost"},
			),
			wantErrs: []issueRef{{dag.CodeUnknownEdgeEndpoint, "edges[3].to"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issues, err := dag.Validate(tc.def)
			if len(tc.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("Validate: want nil error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate: want error, got nil")
			}
			wantIssues(t, issues, tc.wantErrs, nil)
			if tc.wantInError != "" && !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("error does not contain cycle path %q; full error:\n%v", tc.wantInError, err)
			}
		})
	}
}
