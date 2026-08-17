package dag_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// loopDef builds the reference writer⇄critic definition: a pre-loop step, a
// draft that references the pre-loop output and a run param, a critic that
// references the draft, a marked loop edge, and a conditioned exit edge.
func loopDef(t *testing.T) *dag.Definition {
	t.Helper()
	doc := []byte(`{
	  "schema_version": 1,
	  "name": "loop",
	  "params": {"brief": {"type": "string", "required": true}},
	  "steps": [
	    {"id": "pre", "type": "llm", "config": {"model": "mock/sim-1", "prompt": "seed ${{ run.params.brief }}"}},
	    {"id": "draft", "type": "llm", "config": {"model": "mock/sim-1", "prompt": "write ${{ steps.pre.output.text }} for ${{ run.params.brief }}"}},
	    {"id": "critique", "type": "llm", "config": {"model": "mock/sim-1", "prompt": "review ${{ steps.draft.output.text }}"}},
	    {"id": "publish", "type": "echo", "config": {"input": {"ok": true}}}
	  ],
	  "edges": [
	    {"from": "pre", "to": "draft"},
	    {"from": "draft", "to": "critique"},
	    {"from": "critique", "to": "draft", "type": "loop", "condition": "output.verdict == 'revise'", "max_iterations": 3},
	    {"from": "critique", "to": "publish", "when": "output.verdict == 'approve'"}
	  ]
	}`)
	def, err := dag.Decode(doc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return def
}

func loopEdgeOf(def *dag.Definition) dag.Edge {
	for _, e := range def.Edges {
		if e.IsLoop() {
			return e
		}
	}
	return dag.Edge{}
}

func TestGenerateLoopExpansion_Structure(t *testing.T) {
	t.Parallel()
	def := loopDef(t)
	plan, err := dag.GenerateLoopExpansion(def, loopEdgeOf(def), "critique", 1)
	if err != nil {
		t.Fatalf("GenerateLoopExpansion: %v", err)
	}

	// Body clones only draft + critique (pre and publish are outside the body).
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2 (draft#1, critique#1): %+v", len(plan.Steps), plan.Steps)
	}
	if _, ok := stepByID(plan.Steps, "draft#1"); !ok {
		t.Errorf("missing draft#1")
	}
	if _, ok := stepByID(plan.Steps, "critique#1"); !ok {
		t.Errorf("missing critique#1")
	}
	if _, ok := stepByID(plan.Steps, "pre#1"); ok {
		t.Errorf("pre#1 should not be cloned (outside body)")
	}

	// Edges: internal body edge, after-splice, before-splice exit.
	if !hasEdge(plan.Edges, "draft#1", "critique#1") {
		t.Errorf("missing internal edge draft#1 -> critique#1")
	}
	if !hasEdge(plan.Edges, "critique", "draft#1") {
		t.Errorf("missing after-splice critique -> draft#1")
	}
	if !hasEdge(plan.Edges, "critique#1", "publish") {
		t.Errorf("missing before-splice exit critique#1 -> publish")
	}
	// The exit clone carries the authored `when`; it is a normal edge.
	for _, e := range plan.Edges {
		if e.From == "critique#1" && e.To == "publish" {
			if e.IsLoop() {
				t.Errorf("exit edge should be normal, got loop")
			}
			if e.When != "output.verdict == 'approve'" {
				t.Errorf("exit edge when = %q, want the authored predicate", e.When)
			}
		}
	}
}

func TestGenerateLoopExpansion_BodyOnlyRewrite(t *testing.T) {
	t.Parallel()
	def := loopDef(t)
	plan, err := dag.GenerateLoopExpansion(def, loopEdgeOf(def), "critique", 2)
	if err != nil {
		t.Fatalf("GenerateLoopExpansion: %v", err)
	}
	draft, _ := stepByID(plan.Steps, "draft#2")
	draftCfg, _ := json.Marshal(draft.Config)
	// A reference to the pre-loop step is NOT suffixed; the run param is left
	// alone; this is the key divergence from map's rewrite-everything.
	if !strings.Contains(string(draftCfg), "steps.pre.output.text") {
		t.Errorf("draft#2 config lost the un-suffixed pre ref: %s", draftCfg)
	}
	if strings.Contains(string(draftCfg), "steps.pre#2") {
		t.Errorf("draft#2 wrongly suffixed the pre-loop ref: %s", draftCfg)
	}
	critique, _ := stepByID(plan.Steps, "critique#2")
	critCfg, _ := json.Marshal(critique.Config)
	// A reference to a body member IS suffixed.
	if !strings.Contains(string(critCfg), "steps.draft#2.output.text") {
		t.Errorf("critique#2 did not suffix the body ref: %s", critCfg)
	}
}

func TestGenerateLoopExpansion_ValidatesAsExpansion(t *testing.T) {
	t.Parallel()
	def := loopDef(t)
	plan, err := dag.GenerateLoopExpansion(def, loopEdgeOf(def), "critique", 1)
	if err != nil {
		t.Fatalf("GenerateLoopExpansion: %v", err)
	}
	in := dag.ExpansionInput{
		Plan:   plan,
		Origin: dag.ExpansionOrigin{Kind: dag.OriginLoop, StepID: "critique", Depth: 0},
		Existing: map[string]dag.ExpansionAnchor{
			"pre":      {Type: dag.StepLLM, Status: dag.AnchorTerminal, Succeeded: true},
			"draft":    {Type: dag.StepLLM, Status: dag.AnchorTerminal, Succeeded: true},
			"critique": {Type: dag.StepLLM, Status: dag.AnchorActive},
			"publish":  {Type: dag.StepEcho, Status: dag.AnchorPending},
		},
		ExistingEdges:    def.Edges,
		RunParams:        json.RawMessage(`{"brief":"x"}`),
		PerExpansionCap:  32,
		Caps:             (&dag.ExpansionPolicy{}).Resolve(),
		CurrentStepCount: 4,
		ExpansionsSoFar:  0,
	}
	v := dag.ValidateExpansion(in)
	if !v.OK() {
		t.Fatalf("generated loop delta rejected: %v", v.Issues)
	}
}
