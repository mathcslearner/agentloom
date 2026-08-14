package dag_test

import (
	"encoding/json"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// TestValidateTemplateLint is the ticket 8.2 static-lint matrix: template
// references in step configs must name existing, strictly upstream steps
// and declared run parameters; malformed expressions and references are
// rejected with path-qualified issues.
func TestValidateTemplateLint(t *testing.T) {
	t.Parallel()

	echo := func(id, input string) dag.Step {
		return dag.Step{
			ID: id, Type: dag.StepEcho,
			Config: &dag.EchoConfig{Input: json.RawMessage(`"` + input + `"`)},
		}
	}
	// a → b → c, plus a sibling branch a → side.
	chain := func(steps ...dag.Step) *dag.Definition {
		return &dag.Definition{
			SchemaVersion: dag.CurrentSchemaVersion,
			Name:          "tmpl-lint",
			Params:        map[string]dag.ParamSpec{"greeting": {Type: dag.ParamString}},
			Steps:         steps,
			Edges: []dag.Edge{
				{From: "a", To: "b"},
				{From: "b", To: "c"},
				{From: "a", To: "side"},
			},
		}
	}

	cases := []struct {
		name     string
		steps    []dag.Step
		wantErrs []issueRef
	}{
		{
			name: "upstream refs and declared params are clean",
			steps: []dag.Step{
				echo("a", "${{ run.params.greeting }}"),
				echo("b", "${{ steps.a.output }}"),
				echo("c", "${{ steps.a.output }} then ${{ steps.b.output }}"),
				echo("side", "plain"),
			},
		},
		{
			name: "transitive upstream counts",
			steps: []dag.Step{
				echo("a", "x"), echo("b", "y"),
				echo("c", "${{ steps.a.output }}"),
				echo("side", "z"),
			},
		},
		{
			name: "unknown step",
			steps: []dag.Step{
				echo("a", "x"), echo("b", "${{ steps.ghost.output }}"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateRefUnknownStep, "steps[1].config.input"}},
		},
		{
			name: "downstream step is not upstream",
			steps: []dag.Step{
				echo("a", "${{ steps.b.output }}"),
				echo("b", "x"), echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateRefNotUpstream, "steps[0].config.input"}},
		},
		{
			name: "sibling branch is not upstream",
			steps: []dag.Step{
				echo("a", "x"), echo("b", "${{ steps.side.output }}"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateRefNotUpstream, "steps[1].config.input"}},
		},
		{
			name: "self reference",
			steps: []dag.Step{
				echo("a", "x"), echo("b", "${{ steps.b.output }}"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateRefNotUpstream, "steps[1].config.input"}},
		},
		{
			name: "undeclared run parameter",
			steps: []dag.Step{
				echo("a", "${{ run.params.nope }}"), echo("b", "x"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateRefUnknownParam, "steps[0].config.input"}},
		},
		{
			name: "malformed reference",
			steps: []dag.Step{
				echo("a", "${{ steps.b.result }}"), echo("b", "x"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateRefInvalid, "steps[0].config.input"}},
		},
		{
			name: "unknown root",
			steps: []dag.Step{
				echo("a", "${{ secrets.key }}"), echo("b", "x"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateRefInvalid, "steps[0].config.input"}},
		},
		{
			name: "syntax error",
			steps: []dag.Step{
				echo("a", "${{ steps.b.output"), echo("b", "x"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateInvalid, "steps[0].config.input"}},
		},
		{
			name: "unknown function",
			steps: []dag.Step{
				echo("a", "${{ printf 'x' }}"), echo("b", "x"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{{dag.CodeTemplateInvalid, "steps[0].config.input"}},
		},
		{
			name: "lenient get path is not linted",
			steps: []dag.Step{
				echo("a", "${{ get 'steps.ghost.output.x' | default 'none' }}"),
				echo("b", "x"), echo("c", "y"), echo("side", "z"),
			},
		},
		{
			name: "multiple problems all reported",
			steps: []dag.Step{
				echo("a", "${{ steps.ghost.output }} and ${{ run.params.nope }}"),
				echo("b", "${{ steps.c.output }}"),
				echo("c", "y"), echo("side", "z"),
			},
			wantErrs: []issueRef{
				{dag.CodeTemplateRefUnknownStep, "steps[0].config.input"},
				{dag.CodeTemplateRefUnknownParam, "steps[0].config.input"},
				{dag.CodeTemplateRefNotUpstream, "steps[1].config.input"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issues, _ := dag.Validate(chain(tc.steps...))
			wantIssues(t, issues, tc.wantErrs, nil)
		})
	}
}

// TestValidateTemplateLintLoopEdgeAncestry pins that reachability through
// a loop edge alone does not make a step upstream: only normal-edge
// ancestry guarantees the referenced output is recorded before the
// referencing step runs.
func TestValidateTemplateLintLoopEdgeAncestry(t *testing.T) {
	t.Parallel()

	def := &dag.Definition{
		SchemaVersion: dag.CurrentSchemaVersion,
		Name:          "loop-ancestry",
		Steps: []dag.Step{
			// writer → critic (normal), critic → writer (loop). The critic
			// may reference the writer (normal-edge upstream); the writer
			// referencing the critic is only reachable through the loop
			// edge and must be flagged.
			{
				ID: "writer", Type: dag.StepEcho,
				Config: &dag.EchoConfig{Input: json.RawMessage(`"${{ steps.critic.output }}"`)},
			},
			{
				ID: "critic", Type: dag.StepEcho,
				Config: &dag.EchoConfig{Input: json.RawMessage(`"${{ steps.writer.output }}"`)},
			},
		},
		Edges: []dag.Edge{
			{From: "writer", To: "critic"},
			{
				From: "critic", To: "writer", Type: dag.EdgeLoop,
				Condition: "output.x == 'revise'", MaxIterations: 3,
			},
		},
	}
	issues, _ := dag.Validate(def)
	wantIssues(t, issues, []issueRef{
		{dag.CodeTemplateRefNotUpstream, "steps[0].config.input"},
	}, nil)
}

// TestValidateTemplatedSleepDurationDefersParseCheck pins the ticket 8.2
// carve-out: a templated sleep duration skips the literal parseability
// check (the executor re-validates the rendered value at runtime), while
// a malformed literal one still fails.
func TestValidateTemplatedSleepDurationDefersParseCheck(t *testing.T) {
	t.Parallel()

	def := func(duration string) *dag.Definition {
		return &dag.Definition{
			SchemaVersion: dag.CurrentSchemaVersion,
			Name:          "sleepy",
			Params:        map[string]dag.ParamSpec{"nap": {Type: dag.ParamString}},
			Steps: []dag.Step{
				{ID: "zzz", Type: dag.StepSleep, Config: &dag.SleepConfig{Duration: duration}},
			},
			Edges: []dag.Edge{},
		}
	}

	issues, err := dag.Validate(def("${{ run.params.nap }}"))
	if err != nil || len(issues) != 0 {
		t.Errorf("templated duration: want clean validation, got issues=%v err=%v", issues, err)
	}
	issues, _ = dag.Validate(def("not-a-duration"))
	wantIssues(t, issues, []issueRef{{dag.CodeConfigFieldInvalid, "steps[0].config.duration"}}, nil)
}
