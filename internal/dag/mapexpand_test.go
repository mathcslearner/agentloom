package dag_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// mustTemplate decodes a definition carrying a templates section and returns
// the named sub-template.
func mustTemplate(t *testing.T, defJSON, name string) *dag.Template {
	t.Helper()
	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	tmpl, ok := def.Templates[name]
	if !ok || tmpl == nil {
		t.Fatalf("template %q not decoded", name)
	}
	return tmpl
}

// items builds a []json.RawMessage from JSON literals.
func items(lits ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(lits))
	for i, l := range lits {
		out[i] = json.RawMessage(l)
	}
	return out
}

// stepByID finds a plan step by id.
func stepByID(steps []dag.Step, id string) (dag.Step, bool) {
	for _, s := range steps {
		if s.ID == id {
			return s, true
		}
	}
	return dag.Step{}, false
}

// hasEdge reports whether the plan carries a normal edge from→to.
func hasEdge(edges []dag.Edge, from, to string) bool {
	for _, e := range edges {
		if e.From == from && e.To == to && !e.IsLoop() {
			return true
		}
	}
	return false
}

const singleStepBodyDef = `{
	"schema_version": 1, "name": "x",
	"templates": {
		"analyze_one": {"steps": [
			{"id": "analyze", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "Item ${{ item_index }}: ${{ item }}", "max_tokens": 64}}
		]}
	},
	"steps": [
		{"id": "src", "type": "echo", "config": {"input": {"items": ["a"]}}},
		{"id": "m", "type": "map", "config": {"items": "${{ steps.src.output.items }}", "body": "analyze_one"}}
	],
	"edges": [{"from": "src", "to": "m"}]
}`

// TestGenerateMapExpansionSingleStep: one llm step per item, spliced after the
// map and into a generated gather, with item / item_index references rewritten
// to the map step's output.
func TestGenerateMapExpansionSingleStep(t *testing.T) {
	t.Parallel()
	tmpl := mustTemplate(t, singleStepBodyDef, "analyze_one")

	plan, err := dag.GenerateMapExpansion(tmpl, items(`"a"`, `"b"`, `"c"`), "m")
	if err != nil {
		t.Fatalf("GenerateMapExpansion: %v", err)
	}
	// 3 instances + 1 gather.
	if len(plan.Steps) != 4 {
		t.Fatalf("steps = %d, want 4 (3 instances + gather)", len(plan.Steps))
	}
	gatherID := dag.MapGatherID("m")
	if gatherID != "m#gather" {
		t.Fatalf("gather id = %q, want m#gather", gatherID)
	}
	for _, id := range []string{"analyze#0", "analyze#1", "analyze#2", gatherID} {
		if _, ok := stepByID(plan.Steps, id); !ok {
			t.Errorf("missing generated step %q", id)
		}
	}
	// Splice edges: map → each instance (after), each instance → gather (into
	// the barrier). 3 + 3 = 6 edges (single-step bodies have no internal edges).
	if len(plan.Edges) != 6 {
		t.Fatalf("edges = %d, want 6", len(plan.Edges))
	}
	for k, inst := range []string{"analyze#0", "analyze#1", "analyze#2"} {
		if !hasEdge(plan.Edges, "m", inst) {
			t.Errorf("missing after-splice edge m → %s", inst)
		}
		if !hasEdge(plan.Edges, inst, gatherID) {
			t.Errorf("missing gather edge %s → %s", inst, gatherID)
		}
		// The instance's prompt references its item through the map's output.
		s, _ := stepByID(plan.Steps, inst)
		lc, ok := s.Config.(*dag.LLMConfig)
		if !ok {
			t.Fatalf("%s config is %T, want *LLMConfig", inst, s.Config)
		}
		wantItem := "steps.m.output.items." + string(rune('0'+k))
		wantIdx := "steps.m.output.indices." + string(rune('0'+k))
		if !strings.Contains(lc.Prompt, wantItem) {
			t.Errorf("%s prompt %q does not reference %q", inst, lc.Prompt, wantItem)
		}
		if !strings.Contains(lc.Prompt, wantIdx) {
			t.Errorf("%s prompt %q does not reference %q", inst, lc.Prompt, wantIdx)
		}
		if strings.Contains(lc.Prompt, "${{ item") {
			t.Errorf("%s prompt still carries an un-rewritten item root: %q", inst, lc.Prompt)
		}
	}
	// The gather collects the ordered per-instance outputs.
	gs, _ := stepByID(plan.Steps, gatherID)
	if gs.Type != dag.StepGather {
		t.Errorf("gather type = %q, want gather", gs.Type)
	}
	gc, ok := gs.Config.(*dag.GatherConfig)
	if !ok {
		t.Fatalf("gather config is %T, want *GatherConfig", gs.Config)
	}
	var refs []string
	if err := json.Unmarshal(gc.Items, &refs); err != nil {
		t.Fatalf("gather items: %v", err)
	}
	want := []string{
		"${{ steps.analyze#0.output }}",
		"${{ steps.analyze#1.output }}",
		"${{ steps.analyze#2.output }}",
	}
	if len(refs) != len(want) {
		t.Fatalf("gather refs = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("gather ref[%d] = %q, want %q", i, refs[i], want[i])
		}
	}
}

const multiStepBodyDef = `{
	"schema_version": 1, "name": "x",
	"templates": {
		"analyze_two": {
			"steps": [
				{"id": "summarize", "type": "llm",
				 "config": {"model": "mock/sim-1", "prompt": "Summarize ${{ item }}", "max_tokens": 64}},
				{"id": "score", "type": "llm",
				 "config": {"model": "mock/sim-1", "prompt": "Rate ${{ steps.summarize.output.text }}", "max_tokens": 16}}
			],
			"edges": [{"from": "summarize", "to": "score"}]
		}
	},
	"steps": [
		{"id": "src", "type": "echo", "config": {"input": {"items": ["a"]}}},
		{"id": "m", "type": "map", "config": {"items": "${{ steps.src.output.items }}", "body": "analyze_two"}}
	],
	"edges": [{"from": "src", "to": "m"}]
}`

// TestGenerateMapExpansionMultiStep: a two-step body rewrites the internal
// step reference (steps.summarize → steps.summarize#k), splices the entry after
// the map, and points the sink (score#k) at the gather.
func TestGenerateMapExpansionMultiStep(t *testing.T) {
	t.Parallel()
	tmpl := mustTemplate(t, multiStepBodyDef, "analyze_two")

	plan, err := dag.GenerateMapExpansion(tmpl, items(`"a"`, `"b"`), "m")
	if err != nil {
		t.Fatalf("GenerateMapExpansion: %v", err)
	}
	// 2 items × 2 steps + gather = 5 steps.
	if len(plan.Steps) != 5 {
		t.Fatalf("steps = %d, want 5", len(plan.Steps))
	}
	gatherID := dag.MapGatherID("m")
	for k := 0; k < 2; k++ {
		suffix := "#" + string(rune('0'+k))
		// Entry (summarize) spliced after the map; sink (score) into the gather.
		if !hasEdge(plan.Edges, "m", "summarize"+suffix) {
			t.Errorf("missing after-splice edge m → summarize%s", suffix)
		}
		if !hasEdge(plan.Edges, "summarize"+suffix, "score"+suffix) {
			t.Errorf("missing internal edge summarize%s → score%s", suffix, suffix)
		}
		if !hasEdge(plan.Edges, "score"+suffix, gatherID) {
			t.Errorf("missing gather edge score%s → %s", suffix, gatherID)
		}
		// score#k references summarize#k (rewritten internal ref).
		s, _ := stepByID(plan.Steps, "score"+suffix)
		lc := s.Config.(*dag.LLMConfig)
		wantRef := "steps.summarize" + suffix + ".output.text"
		if !strings.Contains(lc.Prompt, wantRef) {
			t.Errorf("score%s prompt %q does not reference %q", suffix, lc.Prompt, wantRef)
		}
	}
	// The gather collects the sink instances in order.
	gs, _ := stepByID(plan.Steps, gatherID)
	var refs []string
	_ = json.Unmarshal(gs.Config.(*dag.GatherConfig).Items, &refs)
	if len(refs) != 2 || refs[0] != "${{ steps.score#0.output }}" || refs[1] != "${{ steps.score#1.output }}" {
		t.Errorf("gather refs = %v, want the two score sinks in order", refs)
	}
}

// TestGenerateMapExpansionEmpty: an empty list yields a gather-only plan whose
// result is the empty ordered array (a valid no-op-shaped expansion).
func TestGenerateMapExpansionEmpty(t *testing.T) {
	t.Parallel()
	tmpl := mustTemplate(t, singleStepBodyDef, "analyze_one")

	plan, err := dag.GenerateMapExpansion(tmpl, nil, "m")
	if err != nil {
		t.Fatalf("GenerateMapExpansion: %v", err)
	}
	if len(plan.Steps) != 1 || len(plan.Edges) != 0 {
		t.Fatalf("steps/edges = %d/%d, want 1/0 (gather only)", len(plan.Steps), len(plan.Edges))
	}
	gs := plan.Steps[0]
	if gs.ID != dag.MapGatherID("m") || gs.Type != dag.StepGather {
		t.Errorf("gather = %s/%s, want m#gather/gather", gs.ID, gs.Type)
	}
	var refs []string
	_ = json.Unmarshal(gs.Config.(*dag.GatherConfig).Items, &refs)
	if len(refs) != 0 {
		t.Errorf("gather refs = %v, want empty", refs)
	}
}

// TestGenerateMapExpansionValidatesThroughExpansion: the generated delta passes
// dag.ValidateExpansion under a map origin (instance ids accepted, refs resolve
// against the merged graph, no cycles).
func TestGenerateMapExpansionValidatesThroughExpansion(t *testing.T) {
	t.Parallel()
	tmpl := mustTemplate(t, multiStepBodyDef, "analyze_two")
	plan, err := dag.GenerateMapExpansion(tmpl, items(`"a"`, `"b"`), "m")
	if err != nil {
		t.Fatalf("GenerateMapExpansion: %v", err)
	}
	in := dag.ExpansionInput{
		Plan:   plan,
		Origin: dag.ExpansionOrigin{Kind: dag.OriginMap, StepID: "m", Depth: 0},
		Existing: map[string]dag.ExpansionAnchor{
			"src": {Type: dag.StepEcho, Status: dag.AnchorTerminal, Succeeded: true},
			"m":   {Type: dag.StepMap, Status: dag.AnchorActive, Succeeded: true},
		},
		ExistingEdges:    []dag.Edge{{From: "src", To: "m"}},
		PerExpansionCap:  32,
		Caps:             (*dag.ExpansionPolicy)(nil).Resolve(),
		CurrentStepCount: 2,
		ExpansionsSoFar:  0,
	}
	verdict := dag.ValidateExpansion(in)
	if !verdict.OK() {
		t.Fatalf("generated map delta rejected: %v", verdict.Err())
	}
}
