package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// woStep builds a run-step row for planWriteOff. fired is the step's
// fired_deps counter.
func woStep(id, status string, fired int32) gen.RunStep {
	return gen.RunStep{StepID: id, StepType: "noop", Status: status, FiredDeps: fired}
}

// woJoinAny is woStep for a pending `join any` step — the only status
// whose survival rule the walk consults.
func woJoinAny(id string, fired int32) gen.RunStep {
	st := woStep(id, store.StepStatusPending, fired)
	st.StepType = "join"
	st.Config = json.RawMessage(`{"mode": "any"}`)
	return st
}

// woEdge builds a normal edge row with the given resolution.
func woEdge(from, to, resolution string) gen.RunEdge {
	return gen.RunEdge{FromStep: from, ToStep: to, EdgeType: store.EdgeTypeNormal, Resolution: resolution}
}

func TestPlanWriteOff(t *testing.T) {
	t.Parallel()
	const (
		unresolved = store.EdgeResolutionUnresolved
		fired      = store.EdgeResolutionFired
		dead       = store.StepStatusDeadLettered
		pending    = store.StepStatusPending
	)

	cases := []struct {
		name  string
		steps []gen.RunStep
		edges []gen.RunEdge
		want  []string
	}{
		{
			name: "chain propagates through the fixed point",
			steps: []gen.RunStep{
				woStep("a", dead, 0), woStep("b", pending, 0), woStep("c", pending, 0),
			},
			edges: []gen.RunEdge{woEdge("a", "b", unresolved), woEdge("b", "c", unresolved)},
			want:  []string{"b", "c"},
		},
		{
			name: "independent branch survives",
			steps: []gen.RunStep{
				woStep("deadstep", dead, 0), woStep("blocked", pending, 0),
				woStep("live", store.StepStatusRunning, 1), woStep("after_live", pending, 0),
			},
			edges: []gen.RunEdge{
				woEdge("deadstep", "blocked", unresolved),
				woEdge("live", "after_live", unresolved),
			},
			want: []string{"blocked"},
		},
		{
			name: "join-all diamond dies on one dead parent",
			steps: []gen.RunStep{
				woStep("deadstep", dead, 0), woStep("alive", store.StepStatusReady, 1),
				woStep("joinall", pending, 0),
			},
			edges: []gen.RunEdge{
				woEdge("deadstep", "joinall", unresolved),
				woEdge("alive", "joinall", unresolved),
			},
			want: []string{"joinall"},
		},
		{
			name: "join-any survives one live parent",
			steps: []gen.RunStep{
				woStep("deadstep", dead, 0), woStep("alive", store.StepStatusReady, 1),
				woJoinAny("race", 0),
			},
			edges: []gen.RunEdge{
				woEdge("deadstep", "race", unresolved),
				woEdge("alive", "race", unresolved),
			},
			want: nil,
		},
		{
			name: "join-any dies when every parent is dead",
			steps: []gen.RunStep{
				woStep("dead1", dead, 0), woStep("dead2", store.StepStatusCancelled, 0),
				woJoinAny("orphan", 0),
			},
			edges: []gen.RunEdge{
				woEdge("dead1", "orphan", unresolved),
				woEdge("dead2", "orphan", unresolved),
			},
			want: []string{"orphan"},
		},
		{
			name: "join-any with a fired edge already survives regardless",
			steps: []gen.RunStep{
				woStep("deadstep", dead, 0), woJoinAny("race", 1),
			},
			edges: []gen.RunEdge{
				woEdge("deadstep", "race", unresolved),
				woEdge("gone", "race", fired),
			},
			want: nil,
		},
		{
			name: "join-any dies transitively through a written-off parent",
			steps: []gen.RunStep{
				woStep("deadstep", dead, 0), woStep("mid", pending, 0),
				woJoinAny("race", 0),
			},
			edges: []gen.RunEdge{
				woEdge("deadstep", "mid", unresolved),
				woEdge("mid", "race", unresolved),
			},
			want: []string{"mid", "race"},
		},
		{
			name: "legacy failed source blocks like dead_lettered",
			steps: []gen.RunStep{
				woStep("old", store.StepStatusFailed, 0), woStep("b", pending, 0),
			},
			edges: []gen.RunEdge{woEdge("old", "b", unresolved)},
			want:  []string{"b"},
		},
		{
			name: "resolved edges from a dead step do not block",
			steps: []gen.RunStep{
				// The dead step fired this edge before a later attempt died
				// (impossible in practice — resolution happens at success —
				// but the walk must only consider unresolved edges).
				woStep("deadstep", dead, 0), woStep("b", pending, 1),
			},
			edges: []gen.RunEdge{woEdge("deadstep", "b", fired)},
			want:  nil,
		},
		{
			name: "loop edges are ignored",
			steps: []gen.RunStep{
				woStep("deadstep", dead, 0), woStep("b", pending, 0),
			},
			edges: []gen.RunEdge{
				{FromStep: "deadstep", ToStep: "b", EdgeType: store.EdgeTypeLoop, Resolution: unresolved},
			},
			want: nil,
		},
		{
			name: "no dead steps writes off nothing",
			steps: []gen.RunStep{
				woStep("a", store.StepStatusSucceeded, 0), woStep("b", pending, 0),
			},
			edges: []gen.RunEdge{woEdge("a", "b", unresolved)},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := planWriteOff(tc.steps, tc.edges)
			if err != nil {
				t.Fatalf("planWriteOff: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("planWriteOff = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlanWriteOffCorruptJoinConfig: a join whose stored config no longer
// decodes surfaces an error — the caller decides the disposition, the walk
// never guesses survival semantics.
func TestPlanWriteOffCorruptJoinConfig(t *testing.T) {
	t.Parallel()
	corrupt := woStep("race", store.StepStatusPending, 0)
	corrupt.StepType = "join"
	corrupt.Config = json.RawMessage(`{"mode": 123}`)
	steps := []gen.RunStep{
		woStep("deadstep", store.StepStatusDeadLettered, 0), corrupt,
	}
	edges := []gen.RunEdge{woEdge("deadstep", "race", store.EdgeResolutionUnresolved)}
	if _, err := planWriteOff(steps, edges); err == nil || !strings.Contains(err.Error(), "decoding join config") {
		t.Fatalf("planWriteOff with corrupt join config: %v, want join-config decode error", err)
	}
}
