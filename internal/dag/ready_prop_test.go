package dag_test

import (
	"fmt"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// genDefinition draws a random valid workflow definition: a DAG by
// construction (normal edges only go from lower to higher declaration
// index), a mix of noop and join steps, and optionally loop edges whose
// ancestry holds by construction (each doubles an existing normal edge in
// reverse).
func genDefinition(t *rapid.T) *dag.Definition {
	n := rapid.IntRange(1, 25).Draw(t, "steps")
	steps := make([]dag.Step, n)
	for i := range steps {
		id := fmt.Sprintf("s%d", i)
		switch rapid.IntRange(0, 9).Draw(t, "kind_"+id) {
		case 0:
			steps[i] = dag.Step{ID: id, Type: dag.StepJoin, Config: &dag.JoinConfig{Mode: dag.JoinAll}}
		case 1:
			steps[i] = dag.Step{ID: id, Type: dag.StepJoin, Config: &dag.JoinConfig{Mode: dag.JoinAny}}
		default:
			steps[i] = dag.Step{ID: id, Type: dag.StepNoop}
		}
	}

	var edges []dag.Edge
	if n > 1 {
		m := rapid.IntRange(0, 3*n).Draw(t, "edges")
		for k := 0; k < m; k++ {
			from := rapid.IntRange(0, n-2).Draw(t, fmt.Sprintf("from%d", k))
			to := rapid.IntRange(from+1, n-1).Draw(t, fmt.Sprintf("to%d", k))
			edges = append(edges, dag.Edge{From: steps[from].ID, To: steps[to].ID})
		}
	}
	normal := len(edges)
	for k := rapid.IntRange(0, 2).Draw(t, "loops"); k > 0 && normal > 0; k-- {
		e := edges[rapid.IntRange(0, normal-1).Draw(t, fmt.Sprintf("loop%d", k))]
		edges = append(edges, dag.Edge{
			From: e.To, To: e.From, Type: dag.EdgeLoop,
			Condition: "output.go", MaxIterations: 3,
		})
	}
	return mkDef(steps, edges...)
}

// naiveReadySteps is the obviously-correct reference implementation of
// ADR-003's readiness, skip-propagation, and join rules, written as a
// repeat-until-stable fixpoint straight off the definition. The library's
// single-pass version must agree with it exactly.
func naiveReadySteps(def *dag.Definition, completed, skipped, failed map[string]bool) (ready, newlySkipped []string) {
	state := make(map[string]string, len(def.Steps)) // pending | completed | skipped | failed
	for _, s := range def.Steps {
		switch {
		case completed[s.ID]:
			state[s.ID] = "completed"
		case skipped[s.ID]:
			state[s.ID] = "skipped"
		case failed[s.ID]:
			state[s.ID] = "failed"
		default:
			state[s.ID] = "pending"
		}
	}
	parents := make(map[string][]string)
	for _, e := range def.Edges {
		if !e.IsLoop() {
			parents[e.To] = append(parents[e.To], e.From)
		}
	}

	for changed := true; changed; {
		changed = false
		for _, s := range def.Steps {
			if state[s.ID] != "pending" || len(parents[s.ID]) == 0 {
				continue
			}
			all := true
			for _, p := range parents[s.ID] {
				if state[p] != "skipped" {
					all = false
				}
			}
			if all {
				state[s.ID] = "skipped"
				changed = true
			}
		}
	}

	for _, s := range def.Steps {
		if state[s.ID] == "skipped" && !skipped[s.ID] {
			newlySkipped = append(newlySkipped, s.ID)
		}
		if state[s.ID] != "pending" {
			continue
		}
		anyFired, allResolved := false, true
		for _, p := range parents[s.ID] {
			switch state[p] {
			case "completed":
				anyFired = true
			case "pending", "failed":
				allResolved = false
			}
		}
		isAny := s.Type == dag.StepJoin && s.Config != nil && s.Config.(*dag.JoinConfig).Mode == dag.JoinAny
		switch {
		case len(parents[s.ID]) == 0, isAny && anyFired, !isAny && allResolved && anyFired:
			ready = append(ready, s.ID)
		}
	}
	return ready, newlySkipped
}

// TestReadyStepsProperties drives random run progressions over random
// valid graphs and checks, each round: the library agrees with the naive
// reference; readiness is monotonic (a ready step left undispatched stays
// ready); results are disjoint from the inputs and each other; and the
// progression terminates, completing every step when nothing failed.
func TestReadyStepsProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		def := genDefinition(t)
		if _, err := dag.Validate(def); err != nil {
			t.Fatalf("generated definition must be valid: %v", err)
		}
		g, err := dag.NewGraph(def)
		if err != nil {
			t.Fatalf("NewGraph: %v", err)
		}

		completed, skipped, failed := map[string]bool{}, map[string]bool{}, map[string]bool{}
		deferred := map[string]bool{} // ready steps deliberately left pending
		for round := 0; ; round++ {
			if round > len(def.Steps)+2 {
				t.Fatalf("progression did not terminate after %d rounds", round)
			}
			ready, newlySkipped, err := g.ReadySteps(completed, skipped, failed)
			if err != nil {
				t.Fatalf("ReadySteps: %v", err)
			}

			wantReady, wantSkipped := naiveReadySteps(def, completed, skipped, failed)
			if !slices.Equal(ready, wantReady) {
				t.Fatalf("ready = %v, reference says %v", ready, wantReady)
			}
			if !slices.Equal(newlySkipped, wantSkipped) {
				t.Fatalf("newlySkipped = %v, reference says %v", newlySkipped, wantSkipped)
			}
			for _, id := range ready {
				if completed[id] || skipped[id] || failed[id] || slices.Contains(newlySkipped, id) {
					t.Fatalf("ready step %q is already terminal", id)
				}
			}
			for id := range deferred {
				if !slices.Contains(ready, id) {
					t.Fatalf("monotonicity violated: %q was ready, stayed pending, and is no longer ready", id)
				}
			}

			for _, id := range newlySkipped {
				skipped[id] = true
			}
			if len(ready) == 0 {
				if len(failed) == 0 {
					for _, s := range def.Steps {
						if !completed[s.ID] && !skipped[s.ID] {
							t.Fatalf("no failures, yet step %q never resolved", s.ID)
						}
					}
				}
				return
			}

			clear(deferred)
			acted := false
			for i, id := range ready {
				act := rapid.IntRange(0, 9).Draw(t, fmt.Sprintf("act_%d_%d", round, i))
				switch {
				case act <= 5 || (!acted && i == len(ready)-1): // progress each round
					completed[id] = true
					acted = true
				case act <= 7:
					failed[id] = true
					acted = true
				case act == 8:
					// The engine seeds runtime when-false/branch outcomes by
					// skipping a ready step (ADR-003), so the generator must
					// reach states where a skipped step has completed parents.
					skipped[id] = true
					acted = true
				default:
					deferred[id] = true
				}
			}
		}
	})
}

// TestTopoOrderProperty checks that TopoOrder respects every normal edge
// on random valid graphs.
func TestTopoOrderProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		def := genDefinition(t)
		g, err := dag.NewGraph(def)
		if err != nil {
			t.Fatalf("NewGraph: %v", err)
		}
		order, err := g.TopoOrder()
		if err != nil {
			t.Fatalf("TopoOrder: %v", err)
		}
		pos := make(map[string]int, len(order))
		for i, id := range order {
			pos[id] = i
		}
		if len(pos) != len(def.Steps) {
			t.Fatalf("order has %d unique steps, want %d", len(pos), len(def.Steps))
		}
		for _, e := range def.Edges {
			if !e.IsLoop() && pos[e.From] >= pos[e.To] {
				t.Fatalf("edge %s->%s violated: positions %d >= %d", e.From, e.To, pos[e.From], pos[e.To])
			}
		}
	})
}

// TestCycleRejectionProperty checks that reversing any normal edge of a
// random valid graph into a second normal edge creates a cycle Validate
// rejects with cycle_detected.
func TestCycleRejectionProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		def := genDefinition(t)
		var normals []int
		for i, e := range def.Edges {
			if !e.IsLoop() {
				normals = append(normals, i)
			}
		}
		if len(normals) == 0 {
			t.Skip("no normal edges to reverse")
		}
		e := def.Edges[normals[rapid.IntRange(0, len(normals)-1).Draw(t, "edge")]]
		def.Edges = append(def.Edges, dag.Edge{From: e.To, To: e.From})
		issues, err := dag.Validate(def)
		if err == nil {
			t.Fatalf("cycle %s<->%s not rejected", e.From, e.To)
		}
		if !slices.ContainsFunc(issues, func(i *dag.ValidationIssue) bool { return i.Code == dag.CodeCycle }) {
			t.Fatalf("no cycle_detected issue in %v", issues)
		}
	})
}
