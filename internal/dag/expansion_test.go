package dag_test

import (
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// ---- DecodePlanOutput ----

func TestDecodePlanOutput_Valid(t *testing.T) {
	t.Parallel()
	data := []byte(`{
		"schema_version": 1,
		"steps": [
			{"id": "research", "type": "llm", "config": {"model": "mock/sim-1", "prompt": "go"}},
			{"id": "write", "type": "llm", "config": {"model": "mock/sim-1", "prompt": "write"}}
		],
		"edges": [{"from": "research", "to": "write"}]
	}`)
	plan, err := dag.DecodePlanOutput(data)
	if err != nil {
		t.Fatalf("DecodePlanOutput: %v", err)
	}
	if plan.SchemaVersion != dag.PlanSchemaVersion {
		t.Errorf("schema_version = %d, want %d", plan.SchemaVersion, dag.PlanSchemaVersion)
	}
	if len(plan.Steps) != 2 || len(plan.Edges) != 1 {
		t.Fatalf("got %d steps, %d edges", len(plan.Steps), len(plan.Edges))
	}
	if plan.Steps[0].ID != "research" {
		t.Errorf("first step id = %q", plan.Steps[0].ID)
	}
}

func TestDecodePlanOutput_EdgesOptional(t *testing.T) {
	t.Parallel()
	plan, err := dag.DecodePlanOutput([]byte(`{"schema_version":1,"steps":[{"id":"x","type":"noop"}]}`))
	if err != nil {
		t.Fatalf("DecodePlanOutput: %v", err)
	}
	if len(plan.Edges) != 0 {
		t.Errorf("edges = %v, want none", plan.Edges)
	}
}

func TestDecodePlanOutput_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing schema_version": `{"steps":[{"id":"x","type":"noop"}]}`,
		"wrong schema_version":   `{"schema_version":2,"steps":[{"id":"x","type":"noop"}]}`,
		"missing steps":          `{"schema_version":1}`,
		"unknown top field":      `{"schema_version":1,"steps":[{"id":"x","type":"noop"}],"bogus":1}`,
		"unknown config field":   `{"schema_version":1,"steps":[{"id":"x","type":"llm","config":{"model":"m","prompt":"p","bogus":1}}]}`,
		"not an object":          `[]`,
		"invalid json":           `{`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := dag.DecodePlanOutput([]byte(doc)); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

// ---- ValidateExpansion ----

// planner is the standard origin: an active (mid-completion) planner step.
func plannerOrigin(depth int) dag.ExpansionOrigin {
	return dag.ExpansionOrigin{Kind: dag.OriginPlanner, StepID: "plan", Depth: depth}
}

// baseInput builds an ExpansionInput with generous caps and a single active
// origin step, so a test overrides only what it exercises.
func baseInput(plan dag.PlanOutput) dag.ExpansionInput {
	return dag.ExpansionInput{
		Plan:   plan,
		Origin: plannerOrigin(0),
		Existing: map[string]dag.ExpansionAnchor{
			"plan": {Type: dag.StepPlanner, Status: dag.AnchorActive},
		},
		PerExpansionCap:  32,
		Caps:             (&dag.ExpansionPolicy{}).Resolve(),
		CurrentStepCount: 1,
		ExpansionsSoFar:  0,
	}
}

func llmStep(id string) dag.Step {
	return dag.Step{ID: id, Type: dag.StepLLM, Config: &dag.LLMConfig{Model: "mock/sim-1", Prompt: "go"}}
}

func TestValidateExpansion_AfterSplice(t *testing.T) {
	t.Parallel()
	in := baseInput(dag.PlanOutput{
		SchemaVersion: 1,
		Steps:         []dag.Step{llmStep("x")},
		Edges:         []dag.Edge{{From: "plan", To: "x"}},
	})
	v := dag.ValidateExpansion(in)
	if !v.OK() {
		t.Fatalf("want OK, got issues: %v", v.Issues)
	}
}

func TestValidateExpansion_BeforeAndParallelSplice(t *testing.T) {
	t.Parallel()
	in := baseInput(dag.PlanOutput{
		SchemaVersion: 1,
		Steps:         []dag.Step{llmStep("x")},
		Edges:         []dag.Edge{{From: "src", To: "x"}, {From: "x", To: "gather"}},
	})
	in.Existing["src"] = dag.ExpansionAnchor{Type: dag.StepLLM, Status: dag.AnchorActive}
	in.Existing["gather"] = dag.ExpansionAnchor{Type: dag.StepJoin, Status: dag.AnchorPending}
	in.CurrentStepCount = 3
	v := dag.ValidateExpansion(in)
	if !v.OK() {
		t.Fatalf("want OK for parallel/before splice, got: %v", v.Issues)
	}
}

func TestValidateExpansion_Rejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		mut   func(in *dag.ExpansionInput)
		code  dag.ValidationCode
		isCap bool
	}{
		{
			name: "id collides with existing",
			mut: func(in *dag.ExpansionInput) {
				in.Plan.Steps = []dag.Step{llmStep("plan")}
				in.Plan.Edges = nil
			},
			code: dag.CodeExpansionAnchorInvalid,
		},
		{
			name: "duplicate delta id",
			mut: func(in *dag.ExpansionInput) {
				in.Plan.Steps = []dag.Step{llmStep("x"), llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}}
			},
			code: dag.CodeDuplicateStepID,
		},
		{
			name: "invalid delta id",
			mut: func(in *dag.ExpansionInput) {
				in.Plan.Steps = []dag.Step{llmStep("bad#0")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "bad#0"}}
			},
			code: dag.CodeInvalidStepID,
		},
		{
			name: "unknown endpoint",
			mut: func(in *dag.ExpansionInput) {
				in.Plan.Steps = []dag.Step{llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "ghost", To: "x"}}
			},
			code: dag.CodeUnknownEdgeEndpoint,
		},
		{
			name: "edge between two existing",
			mut: func(in *dag.ExpansionInput) {
				in.Existing["a"] = dag.ExpansionAnchor{Type: dag.StepNoop, Status: dag.AnchorPending}
				in.Existing["b"] = dag.ExpansionAnchor{Type: dag.StepNoop, Status: dag.AnchorPending}
				in.Plan.Steps = []dag.Step{llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}, {From: "a", To: "b"}}
			},
			code: dag.CodeExpansionAnchorInvalid,
		},
		{
			name: "from terminal anchor",
			mut: func(in *dag.ExpansionInput) {
				in.Existing["done"] = dag.ExpansionAnchor{Type: dag.StepNoop, Status: dag.AnchorTerminal}
				in.Plan.Steps = []dag.Step{llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "done", To: "x"}}
			},
			code: dag.CodeExpansionAnchorInvalid,
		},
		{
			name: "to non-pending anchor",
			mut: func(in *dag.ExpansionInput) {
				in.Existing["run"] = dag.ExpansionAnchor{Type: dag.StepNoop, Status: dag.AnchorActive}
				in.Plan.Steps = []dag.Step{llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}, {From: "x", To: "run"}}
			},
			code: dag.CodeExpansionAnchorInvalid,
		},
		{
			name: "delta introduces a cycle",
			mut: func(in *dag.ExpansionInput) {
				in.Plan.Steps = []dag.Step{llmStep("x"), llmStep("y")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}, {From: "x", To: "y"}, {From: "y", To: "x"}}
			},
			code: dag.CodeCycle,
		},
		{
			name: "bad step config",
			mut: func(in *dag.ExpansionInput) {
				in.Plan.Steps = []dag.Step{{ID: "x", Type: dag.StepLLM, Config: &dag.LLMConfig{Prompt: "no model"}}}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}}
			},
			code: dag.CodeConfigFieldRequired,
		},
		{
			name: "loop edge missing condition",
			mut: func(in *dag.ExpansionInput) {
				in.Plan.Steps = []dag.Step{llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}, {From: "x", To: "plan", Type: dag.EdgeLoop, MaxIterations: 2}}
			},
			code: dag.CodeLoopFieldRequired,
		},
		{
			name: "per-expansion cap exceeded",
			mut: func(in *dag.ExpansionInput) {
				in.PerExpansionCap = 1
				in.Plan.Steps = []dag.Step{llmStep("x"), llmStep("y")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}, {From: "plan", To: "y"}}
			},
			code:  dag.CodeExpansionCapExceeded,
			isCap: true,
		},
		{
			name: "max_total_steps exceeded",
			mut: func(in *dag.ExpansionInput) {
				in.Caps.MaxTotalSteps = 1
				in.CurrentStepCount = 1
				in.Plan.Steps = []dag.Step{llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}}
			},
			code:  dag.CodeExpansionCapExceeded,
			isCap: true,
		},
		{
			name: "max_expansions exceeded",
			mut: func(in *dag.ExpansionInput) {
				in.Caps.MaxExpansions = 2
				in.ExpansionsSoFar = 2
				in.Plan.Steps = []dag.Step{llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}}
			},
			code:  dag.CodeExpansionCapExceeded,
			isCap: true,
		},
		{
			name: "max_depth exceeded",
			mut: func(in *dag.ExpansionInput) {
				in.Caps.MaxDepth = 2
				in.Origin.Depth = 2
				in.Plan.Steps = []dag.Step{llmStep("x")}
				in.Plan.Edges = []dag.Edge{{From: "plan", To: "x"}}
			},
			code:  dag.CodeExpansionCapExceeded,
			isCap: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := baseInput(dag.PlanOutput{SchemaVersion: 1})
			tc.mut(&in)
			v := dag.ValidateExpansion(in)
			if v.OK() {
				t.Fatalf("want rejection, got OK")
			}
			if !hasCode(v.Issues, tc.code) {
				t.Errorf("want code %q, got issues: %v", tc.code, v.Issues)
			}
			if v.CapExceeded() != tc.isCap {
				t.Errorf("CapExceeded() = %v, want %v (issues: %v)", v.CapExceeded(), tc.isCap, v.Issues)
			}
			if v.Err() == nil {
				t.Error("Err() = nil for a rejected expansion")
			}
		})
	}
}

func hasCode(issues []*dag.ValidationIssue, code dag.ValidationCode) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// ---- ExpansionPolicy resolution + definition-level validation ----

func TestExpansionPolicyResolveDefaults(t *testing.T) {
	t.Parallel()
	caps := (*dag.ExpansionPolicy)(nil).Resolve()
	if caps.MaxAddedStepsPerExpansion != dag.DefaultMaxAddedStepsPerExpansion ||
		caps.MaxExpansions != dag.DefaultMaxExpansions ||
		caps.MaxDepth != dag.DefaultMaxDepth ||
		caps.MaxTotalSteps != dag.DefaultMaxTotalSteps {
		t.Fatalf("nil policy did not resolve to defaults: %+v", caps)
	}
	n := 8
	caps = (&dag.ExpansionPolicy{MaxAddedSteps: &n}).Resolve()
	if caps.MaxAddedStepsPerExpansion != 8 {
		t.Errorf("override not applied: %+v", caps)
	}
}

func TestValidate_ExpansionBlockBounds(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"negative cap":                 `"expansion":{"max_added_steps":-1},`,
		"max_total_steps over ceiling": `"expansion":{"max_total_steps":100000},`,
		"planner over run per-exp cap": `"expansion":{"max_added_steps":4},`,
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doc := `{"schema_version":1,"name":"t",` + block +
				`"steps":[{"id":"plan","type":"planner","config":{"model":"mock/sim-1","prompt":"go","max_added_steps":10}}],"edges":[]}`
			def, err := dag.Decode([]byte(doc))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if _, verr := dag.Validate(def); verr == nil {
				t.Fatalf("want validation error for %s", name)
			}
		})
	}
}

func TestValidate_ExpansionBlockRoundTrips(t *testing.T) {
	t.Parallel()
	doc := `{"schema_version":1,"name":"t","expansion":{"max_added_steps":8,"max_expansions":10,"max_depth":3},` +
		`"steps":[{"id":"plan","type":"planner","config":{"model":"mock/sim-1","prompt":"go","max_added_steps":4}}],"edges":[]}`
	def, err := dag.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	issues, err := dag.Validate(def)
	if err != nil {
		t.Fatalf("Validate: %v (issues %v)", err, issues)
	}
	if len(issues) != 0 {
		t.Fatalf("want zero issues, got %v", issues)
	}
	out, err := dag.Encode(def)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(out), `"expansion":{"max_added_steps":8`) {
		t.Errorf("canonical encoding dropped or reordered expansion block:\n%s", out)
	}
	// Round-trip must be lossless.
	def2, err := dag.Decode(out)
	if err != nil {
		t.Fatalf("re-Decode: %v", err)
	}
	out2, err := dag.Encode(def2)
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if string(out) != string(out2) {
		t.Errorf("round trip not lossless:\n%s\n%s", out, out2)
	}
}
