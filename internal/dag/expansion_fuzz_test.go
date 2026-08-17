package dag_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// FuzzValidateExpansion is ticket 13.5's validator fuzzer (ADR-015). It feeds
// arbitrary bytes through DecodePlanOutput — the same gate a planner's output
// passes (the implicit json_schema validator) before ValidateExpansion ever
// runs in production — and, for every plan that decodes, asserts two properties:
//
//  1. ValidateExpansion never panics (the go test fuzzer flags any panic).
//  2. No acceptance-of-invalid: when the verdict is OK, an INDEPENDENT oracle
//     (NewGraph + TopoOrder over the merged graph, which shares none of
//     ValidateExpansion's code path) confirms the graph-structural invariants
//     ValidateExpansion is responsible for actually hold — every injected id is
//     unique and collision-free, every edge endpoint resolves, and the merged
//     normal-edge graph is acyclic. If ValidateExpansion accepts a plan the
//     oracle rejects, that is a real over-acceptance bug.
//
// The oracle checks necessary conditions only (graph structure), so it never
// produces a false positive: anchor-status splice rules and cap semantics are
// ValidateExpansion's own and are covered by the deterministic tables in
// expansion_test.go. Fuzzing here targets the structural core.
func FuzzValidateExpansion(f *testing.F) {
	// Seed corpus: the 13.3 happy plan (after+before splice into gather), the
	// id-collision reject, the empty plan, an over-cap-ish plan, and a plan with
	// a dangling edge — a spread of accept/reject shapes to grow from.
	f.Add([]byte(`{"schema_version":1,"steps":[` +
		`{"id":"work_a","type":"llm","config":{"model":"mock/sim-1","prompt":"a","max_tokens":64}},` +
		`{"id":"work_b","type":"llm","config":{"model":"mock/sim-1","prompt":"b","max_tokens":64}}],` +
		`"edges":[{"from":"plan","to":"work_a"},{"from":"plan","to":"work_b"},` +
		`{"from":"work_a","to":"gather"},{"from":"work_b","to":"gather"}]}`))
	f.Add([]byte(`{"schema_version":1,"steps":[{"id":"gather","type":"noop"}],"edges":[{"from":"plan","to":"gather"}]}`))
	f.Add([]byte(`{"schema_version":1,"steps":[]}`))
	f.Add([]byte(`{"schema_version":1,"steps":[{"id":"loner","type":"noop"}],"edges":[{"from":"loner","to":"ghost"}]}`))
	f.Add([]byte(`{"schema_version":1,"steps":[` +
		`{"id":"a","type":"noop"},{"id":"b","type":"noop"}],` +
		`"edges":[{"from":"a","to":"b"},{"from":"b","to":"a"}]}`)) // a plan-internal cycle

	f.Fuzz(func(t *testing.T, data []byte) {
		plan, err := dag.DecodePlanOutput(data)
		if err != nil {
			return // not a decodable plan; ValidateExpansion is never reached in production
		}

		in := fuzzExpansionInput(*plan)
		v := dag.ValidateExpansion(in) // must not panic

		// CapExceeded is a rejection signal, so it can never coexist with OK.
		if v.OK() && v.CapExceeded() {
			t.Fatalf("verdict is OK yet reports CapExceeded: %v", v.Issues)
		}
		if !v.OK() {
			return
		}
		if err := oracleMergedGraphValid(in); err != nil {
			t.Fatalf("ValidateExpansion accepted a structurally invalid expansion: %v\nplan: %+v", err, plan)
		}
	})
}

// fuzzExpansionBase is the fixed running graph the fuzzer splices into: the
// standard planner shape plan(planner) -> gather(join) -> report(echo). It is
// acyclic with unique ids, so any oracle failure is attributable to the plan.
var fuzzExpansionBase = struct {
	steps   []dag.Step
	edges   []dag.Edge
	anchors map[string]dag.ExpansionAnchor
}{
	steps: []dag.Step{
		{ID: "plan", Type: dag.StepPlanner},
		{ID: "gather", Type: dag.StepJoin},
		{ID: "report", Type: dag.StepEcho},
	},
	edges: []dag.Edge{{From: "plan", To: "gather"}, {From: "gather", To: "report"}},
	anchors: map[string]dag.ExpansionAnchor{
		// The origin is mid-completion (active); the not-yet-run downstream
		// steps are pending (a valid "before"-splice target).
		"plan":   {Type: dag.StepPlanner, Status: dag.AnchorActive},
		"gather": {Type: dag.StepJoin, Status: dag.AnchorPending},
		"report": {Type: dag.StepEcho, Status: dag.AnchorPending},
	},
}

// fuzzExpansionInput wraps a fuzzed plan against the fixed base graph, with a
// planner origin and default resolved caps (PerExpansionCap raised so cap
// rejections do not dominate the accepted-plan population the oracle checks).
func fuzzExpansionInput(plan dag.PlanOutput) dag.ExpansionInput {
	return dag.ExpansionInput{
		Plan:             plan,
		Origin:           dag.ExpansionOrigin{Kind: dag.OriginPlanner, StepID: "plan", Depth: 0},
		Existing:         fuzzExpansionBase.anchors,
		ExistingEdges:    fuzzExpansionBase.edges,
		PerExpansionCap:  256,
		Caps:             (&dag.ExpansionPolicy{}).Resolve(),
		CurrentStepCount: len(fuzzExpansionBase.steps),
		ExpansionsSoFar:  0,
	}
}

// oracleMergedGraphValid independently rebuilds the merged graph (base steps +
// the plan's injected steps, base edges + the plan's edges) and returns an
// error if it violates a structural invariant ValidateExpansion must enforce:
// NewGraph fails on a duplicate id (collision) or a dangling edge endpoint, and
// TopoOrder fails on a normal-edge cycle. Loop edges are excluded from the cycle
// check by NewGraph, matching ValidateExpansion's normal-edge acyclicity rule.
func oracleMergedGraphValid(in dag.ExpansionInput) error {
	merged := &dag.Definition{
		SchemaVersion: dag.CurrentSchemaVersion,
		Name:          "oracle-merged",
		Steps:         append(append([]dag.Step(nil), fuzzExpansionBase.steps...), in.Plan.Steps...),
		Edges:         append(append([]dag.Edge(nil), fuzzExpansionBase.edges...), in.Plan.Edges...),
	}
	g, err := dag.NewGraph(merged)
	if err != nil {
		return err // duplicate id (collision) or unknown edge endpoint (dangling)
	}
	if _, err := g.TopoOrder(); err != nil {
		return err // normal-edge cycle
	}
	return nil
}
