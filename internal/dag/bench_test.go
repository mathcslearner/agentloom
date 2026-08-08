package dag_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// synthetic10k builds the M1 exit-criteria graph: 100 layers of 100 steps
// (10k steps, the MaxSteps limit), each non-entry step fed by two steps of
// the previous layer (19.8k edges, just under MaxEdges), with a join
// opening every non-entry layer.
func synthetic10k() *dag.Definition {
	const layers, width = 100, 100
	id := func(l, w int) string { return fmt.Sprintf("l%dn%d", l, w) }
	steps := make([]dag.Step, 0, layers*width)
	edges := make([]dag.Edge, 0, (layers-1)*width*2)
	for l := 0; l < layers; l++ {
		for w := 0; w < width; w++ {
			s := dag.Step{ID: id(l, w), Type: dag.StepNoop}
			if l > 0 && w == 0 {
				mode := dag.JoinAll
				if l%2 == 0 {
					mode = dag.JoinAny
				}
				s = dag.Step{ID: id(l, w), Type: dag.StepJoin, Config: &dag.JoinConfig{Mode: mode}}
			}
			steps = append(steps, s)
			if l > 0 {
				edges = append(edges,
					dag.Edge{From: id(l-1, w), To: id(l, w)},
					dag.Edge{From: id(l-1, (w+1)%width), To: id(l, w)},
				)
			}
		}
	}
	return &dag.Definition{
		SchemaVersion: dag.CurrentSchemaVersion,
		Name:          "synthetic-10k",
		Steps:         steps,
		Edges:         edges,
	}
}

// halfDone marks the first 50 layers completed, leaving layer 50 as the
// readiness frontier.
func halfDone() map[string]bool {
	completed := make(map[string]bool, 5000)
	for l := 0; l < 50; l++ {
		for w := 0; w < 100; w++ {
			completed[fmt.Sprintf("l%dn%d", l, w)] = true
		}
	}
	return completed
}

// TestSynthetic10kPerformance is the M1 exit criterion: a 10k-step graph
// validates and computes readiness in under 100ms. The wall-clock gate is
// skipped under the race detector (its instrumentation slows pure CPU work
// severalfold); the benchmarks below give the real numbers.
func TestSynthetic10kPerformance(t *testing.T) {
	t.Parallel()

	def := synthetic10k()
	completed := halfDone()

	start := time.Now()
	issues, err := dag.Validate(def)
	if err != nil || len(issues) != 0 {
		t.Fatalf("Validate: issues %v, err %v", issues, err)
	}
	g, err := dag.NewGraph(def)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	ready, newlySkipped, err := g.ReadySteps(completed, nil, nil)
	if err != nil {
		t.Fatalf("ReadySteps: %v", err)
	}
	elapsed := time.Since(start)

	if len(ready) != 100 || len(newlySkipped) != 0 {
		t.Fatalf("ready frontier = %d steps (want 100), newlySkipped = %d (want 0)", len(ready), len(newlySkipped))
	}
	if raceEnabled {
		t.Logf("validate + readiness on 10k steps took %v (100ms gate skipped under -race)", elapsed)
		return
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("validate + readiness on 10k steps took %v, want < 100ms", elapsed)
	}
}

func BenchmarkValidate10k(b *testing.B) {
	def := synthetic10k()
	b.ResetTimer()
	for b.Loop() {
		if _, err := dag.Validate(def); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadySteps10k(b *testing.B) {
	def := synthetic10k()
	g, err := dag.NewGraph(def)
	if err != nil {
		b.Fatal(err)
	}
	completed := halfDone()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := g.ReadySteps(completed, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
