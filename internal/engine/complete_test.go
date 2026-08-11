package engine

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// edge builds a normal-edge row for plan tests; when == "" means no `when`.
func edge(ordinal int32, when string) gen.RunEdge {
	e := gen.RunEdge{Ordinal: ordinal, EdgeType: store.EdgeTypeNormal}
	if when != "" {
		e.WhenExpr = &when
	}
	return e
}

func loopEdge(ordinal int32) gen.RunEdge {
	cond := "output.verdict == 'revise'"
	return gen.RunEdge{Ordinal: ordinal, EdgeType: store.EdgeTypeLoop, Condition: &cond}
}

// fired extracts the verdict list as ordinal → fired for compact asserts.
func fired(verdicts []edgeVerdict) map[int32]bool {
	m := make(map[int32]bool, len(verdicts))
	for _, v := range verdicts {
		m[v.ordinal] = v.fired
	}
	return m
}

func TestPlanEdgesAllMatching(t *testing.T) {
	t.Parallel()

	output := json.RawMessage(`{"category": "refund", "score": 7}`)
	edges := []gen.RunEdge{
		edge(0, ""),                              // unconditioned: fires
		edge(1, `output.category == 'refund'`),   // true: fires
		edge(2, `output.category == 'billing'`),  // false: skips
		edge(3, `output.score > 5`),              // true: fires
		edge(4, `run.params.tenant == 'volume'`), // params-driven: true
	}
	verdicts, err := planEdges(string(dag.StepEcho), edges, output, map[string]any{"tenant": "volume"})
	if err != nil {
		t.Fatalf("planEdges: %v", err)
	}
	want := map[int32]bool{0: true, 1: true, 2: false, 3: true, 4: true}
	got := fired(verdicts)
	if len(got) != len(want) {
		t.Fatalf("verdicts = %v, want %v", got, want)
	}
	for ord, w := range want {
		if got[ord] != w {
			t.Errorf("edge %d fired = %v, want %v", ord, got[ord], w)
		}
	}
}

func TestPlanEdgesBranchFirstMatch(t *testing.T) {
	t.Parallel()

	branchEdges := []gen.RunEdge{
		edge(0, `output.category == 'refund'`),
		edge(1, `output.category == 'complaint'`),
		edge(2, ""), // trailing default
	}
	cases := []struct {
		name   string
		output string
		edges  []gen.RunEdge
		want   map[int32]bool
	}{
		{
			name:   "first true fires, rest skip including default",
			output: `{"category": "refund"}`,
			edges:  branchEdges,
			want:   map[int32]bool{0: true, 1: false, 2: false},
		},
		{
			name:   "second matches after first misses",
			output: `{"category": "complaint"}`,
			edges:  branchEdges,
			want:   map[int32]bool{0: false, 1: true, 2: false},
		},
		{
			name:   "default fires when nothing matches",
			output: `{"category": "other"}`,
			edges:  branchEdges,
			want:   map[int32]bool{0: false, 1: false, 2: true},
		},
		{
			name:   "no match and no default skips everything",
			output: `{"category": "other"}`,
			edges:  branchEdges[:2],
			want:   map[int32]bool{0: false, 1: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdicts, err := planEdges(string(dag.StepBranch), tc.edges, json.RawMessage(tc.output), nil)
			if err != nil {
				t.Fatalf("planEdges: %v", err)
			}
			got := fired(verdicts)
			for ord, w := range tc.want {
				if got[ord] != w {
					t.Errorf("edge %d fired = %v, want %v", ord, got[ord], w)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("verdicts = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlanEdgesBranchSkipsPredicatesAfterMatch pins that a branch stops
// evaluating once matched: an expression that would error on this output
// must not surface when a prior edge already fired.
func TestPlanEdgesBranchSkipsPredicatesAfterMatch(t *testing.T) {
	t.Parallel()

	edges := []gen.RunEdge{
		edge(0, `output.category == 'refund'`),
		edge(1, `output.missing.field == 'x'`), // would EvalError if evaluated
	}
	verdicts, err := planEdges(string(dag.StepBranch), edges, json.RawMessage(`{"category": "refund"}`), nil)
	if err != nil {
		t.Fatalf("planEdges: %v (post-match predicates must not be evaluated)", err)
	}
	got := fired(verdicts)
	if !got[0] || got[1] {
		t.Errorf("verdicts = %v, want edge 0 fired and edge 1 skipped", got)
	}
}

func TestPlanEdgesExcludesLoopEdges(t *testing.T) {
	t.Parallel()

	edges := []gen.RunEdge{loopEdge(0), edge(1, "")}
	verdicts, err := planEdges(string(dag.StepEcho), edges, nil, nil)
	if err != nil {
		t.Fatalf("planEdges: %v", err)
	}
	if len(verdicts) != 1 || verdicts[0].ordinal != 1 {
		t.Errorf("verdicts = %+v, want only the normal edge (ordinal 1)", verdicts)
	}
}

func TestPlanEdgesEvalErrorIsTyped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		when   string
		output string
	}{
		{"missing field", `output.missing.field == 'x'`, `{"a": 1}`},
		{"predicate on nil output", `output.category == 'x'`, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var output json.RawMessage
			if tc.output != "" {
				output = json.RawMessage(tc.output)
			}
			_, err := planEdges(string(dag.StepEcho), []gen.RunEdge{edge(0, tc.when)}, output, nil)
			var evalErr *dag.EvalError
			if !errors.As(err, &evalErr) {
				t.Fatalf("error = %v, want *dag.EvalError (never coerced to false)", err)
			}
		})
	}
}

func TestPlanEdgesBadOutputJSON(t *testing.T) {
	t.Parallel()

	_, err := planEdges(string(dag.StepEcho), []gen.RunEdge{edge(0, `output == 1`)}, json.RawMessage(`{not json`), nil)
	if err == nil {
		t.Fatal("planEdges accepted malformed output JSON")
	}
}

func TestAllSkippedVerdicts(t *testing.T) {
	t.Parallel()

	verdicts := allSkippedVerdicts([]gen.RunEdge{edge(0, `output.x == 1`), loopEdge(1), edge(2, "")})
	got := fired(verdicts)
	want := map[int32]bool{0: false, 2: false}
	if len(got) != len(want) {
		t.Fatalf("verdicts = %v, want %v (loop edge excluded, nothing fired)", got, want)
	}
	for ord, w := range want {
		if got[ord] != w {
			t.Errorf("edge %d fired = %v, want %v", ord, got[ord], w)
		}
	}
}

func TestIsJoinAny(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		step    gen.RunStep
		want    bool
		wantErr bool
	}{
		{"join any", gen.RunStep{StepType: "join", Config: json.RawMessage(`{"mode": "any"}`)}, true, false},
		{"join all", gen.RunStep{StepType: "join", Config: json.RawMessage(`{"mode": "all"}`)}, false, false},
		{"join with absent config gets the all-shaped rule", gen.RunStep{StepType: "join"}, false, false},
		{"non-join never", gen.RunStep{StepType: "echo", Config: json.RawMessage(`{"mode": "any"}`)}, false, false},
		{"corrupt config errors", gen.RunStep{StepType: "join", Config: json.RawMessage(`{"unknown_field": 1}`)}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := isJoinAny(tc.step)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("isJoinAny = %v, want %v", got, tc.want)
			}
		})
	}
}
