package dag_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// issueRef identifies one expected validation issue.
type issueRef struct {
	code dag.ValidationCode
	path string
}

// refsOf projects issues of one severity onto comparable refs, preserving
// order.
func refsOf(issues []*dag.ValidationIssue, sev dag.Severity) []issueRef {
	var refs []issueRef
	for _, i := range issues {
		if i.Severity == sev {
			refs = append(refs, issueRef{i.Code, i.Path})
		}
	}
	return refs
}

// wantIssues asserts the exact set of issues Validate reported, split by
// severity: nothing missing, nothing extra.
func wantIssues(t *testing.T, issues []*dag.ValidationIssue, wantErrs, wantWarns []issueRef) {
	t.Helper()
	sort := func(refs []issueRef) []issueRef {
		slices.SortFunc(refs, func(a, b issueRef) int {
			if a.path != b.path {
				return strings.Compare(a.path, b.path)
			}
			return strings.Compare(string(a.code), string(b.code))
		})
		return refs
	}
	if got := sort(refsOf(issues, dag.SeverityError)); !slices.Equal(got, sort(slices.Clone(wantErrs))) {
		t.Errorf("error issues = %v, want %v", got, wantErrs)
	}
	if got := sort(refsOf(issues, dag.SeverityWarning)); !slices.Equal(got, sort(slices.Clone(wantWarns))) {
		t.Errorf("warning issues = %v, want %v", got, wantWarns)
	}
}

// decodeFixture decodes a fixture that must pass the codec layer.
func decodeFixture(t *testing.T, parts ...string) *dag.Definition {
	t.Helper()
	def, err := dag.Decode(readFixture(t, parts...))
	if err != nil {
		t.Fatalf("Decode: fixture must be codec-valid, got: %v", err)
	}
	return def
}

// structuralCases maps every invalid_structural fixture to the exact set
// of error-severity issues it must produce. The corpus-coverage test pins
// this table to the fixture directory.
var structuralCases = map[string][]issueRef{
	"duplicate_step_id.json": {{dag.CodeDuplicateStepID, "steps[1].id"}},
	"invalid_step_id.json": {
		{dag.CodeInvalidStepID, "steps[0].id"},
		{dag.CodeInvalidStepID, "steps[1].id"},
		{dag.CodeInvalidStepID, "steps[2].id"},
	},
	"unknown_edge_endpoint.json": {
		{dag.CodeUnknownEdgeEndpoint, "edges[0].from"},
		{dag.CodeUnknownEdgeEndpoint, "edges[1].to"},
	},
	"no_entry_step.json":     {{dag.CodeNoEntryStep, ""}},
	"no_steps.json":          {{dag.CodeNoSteps, "steps"}},
	"llm_missing_model.json": {{dag.CodeConfigFieldRequired, "steps[0].config.model"}},
	"llm_prompt_and_messages.json": {
		{dag.CodeConfigFieldConflict, "steps[0].config"},
	},
	"llm_no_prompt_or_messages.json": {
		{dag.CodeConfigFieldRequired, "steps[0].config"},
	},
	"tool_missing_tool.json": {{dag.CodeConfigFieldRequired, "steps[0].config.tool"}},
	"retrieve_missing_fields.json": {
		{dag.CodeConfigFieldRequired, "steps[0].config.retriever"},
		{dag.CodeConfigFieldRequired, "steps[0].config.query"},
	},
	"map_missing_fields.json": {
		{dag.CodeConfigFieldRequired, "steps[0].config.items"},
		{dag.CodeConfigFieldRequired, "steps[0].config.body"},
	},
	"planner_missing_model.json":         {{dag.CodeConfigFieldRequired, "steps[0].config.model"}},
	"agent_missing_agent.json":           {{dag.CodeConfigFieldRequired, "steps[0].config.agent"}},
	"human_approval_missing_prompt.json": {{dag.CodeConfigFieldRequired, "steps[0].config.prompt"}},
	"join_missing_mode.json":             {{dag.CodeConfigFieldRequired, "steps[0].config.mode"}},
	"branch_no_out_edges.json":           {{dag.CodeBranchNoOutEdges, "steps[1]"}},
	"branch_default_not_last.json":       {{dag.CodeBranchEdgeUnconditioned, "edges[1]"}},
	"branch_multiple_defaults.json":      {{dag.CodeBranchEdgeUnconditioned, "edges[1]"}},
	"loop_missing_condition.json":        {{dag.CodeLoopFieldRequired, "edges[1].condition"}},
	"loop_missing_max_iterations.json":   {{dag.CodeLoopFieldRequired, "edges[1].max_iterations"}},
	"loop_max_iterations_range.json":     {{dag.CodeLimitExceeded, "edges[1].max_iterations"}},
	"loop_edge_with_when.json":           {{dag.CodeLoopFieldForbidden, "edges[1].when"}},
	"normal_edge_loop_fields.json": {
		{dag.CodeLoopFieldForbidden, "edges[0].condition"},
		{dag.CodeLoopFieldForbidden, "edges[0].max_iterations"},
	},
	"name_too_long.json": {{dag.CodeLimitExceeded, "name"}},
	"expr_too_long.json": {{dag.CodeLimitExceeded, "edges[0].when"}},
	"multi_error_structural.json": {
		{dag.CodeDuplicateStepID, "steps[1].id"},
		{dag.CodeConfigFieldRequired, "steps[1].config.model"},
		{dag.CodeBranchEdgeUnconditioned, "edges[1]"},
		{dag.CodeUnknownEdgeEndpoint, "edges[2].to"},
		{dag.CodeLoopFieldForbidden, "edges[2].max_iterations"},
	},
}

// TestValidateInvalidStructuralFixtures is the ticket 1.3 acceptance test:
// every rule has a fixture that decodes cleanly (the violation is
// structural, not codec-level) and fails Validate with the exact expected
// codes and paths, all reported in one pass.
func TestValidateInvalidStructuralFixtures(t *testing.T) {
	t.Parallel()

	for name, want := range structuralCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			def := decodeFixture(t, "invalid_structural", name)
			issues, err := dag.Validate(def)
			if err == nil {
				t.Fatal("Validate: want error, got nil")
			}
			wantIssues(t, issues, want, nil)
		})
	}
}

// TestValidateStructuralCorpusIsCovered pins the expectation table to the
// fixture directory so a fixture added without expectations (or vice
// versa) fails loudly.
func TestValidateStructuralCorpusIsCovered(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join("testdata", "invalid_structural", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing invalid_structural fixtures: %v (found %d)", err, len(files))
	}
	for _, f := range files {
		if _, ok := structuralCases[filepath.Base(f)]; !ok {
			t.Errorf("%s: fixture has no expectations in structuralCases", filepath.Base(f))
		}
	}
	if len(files) != len(structuralCases) {
		t.Errorf("%d fixtures on disk, %d expectation entries", len(files), len(structuralCases))
	}
}

// TestValidateValidFixtures verifies the whole valid corpus passes
// structural validation; only the dedicated warning fixture may produce
// (warning) issues.
func TestValidateValidFixtures(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join("testdata", "valid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing valid fixtures: %v (found %d)", err, len(files))
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			t.Parallel()
			def := decodeFixture(t, "valid", filepath.Base(f))
			issues, err := dag.Validate(def)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if filepath.Base(f) != "isolated_step.json" && len(issues) != 0 {
				t.Errorf("want no issues, got %v", issues)
			}
		})
	}
}

// TestValidateIsolatedStepWarning verifies the ADR-003 orphan policy: an
// isolated step is a warning, not an error.
func TestValidateIsolatedStepWarning(t *testing.T) {
	t.Parallel()

	def := decodeFixture(t, "valid", "isolated_step.json")
	issues, err := dag.Validate(def)
	if err != nil {
		t.Fatalf("Validate: warnings must not produce an error, got: %v", err)
	}
	wantIssues(t, issues, nil, []issueRef{{dag.CodeIsolatedStep, "steps[2]"}})
}

func TestValidateTableDriven(t *testing.T) {
	t.Parallel()

	noop := func(id string) dag.Step { return dag.Step{ID: id, Type: dag.StepNoop} }
	pair := func(edges ...dag.Edge) *dag.Definition {
		return &dag.Definition{
			SchemaVersion: dag.CurrentSchemaVersion,
			Name:          "t",
			Steps:         []dag.Step{noop("a"), noop("b")},
			Edges:         edges,
		}
	}
	single := func(steps ...dag.Step) *dag.Definition {
		return &dag.Definition{
			SchemaVersion: dag.CurrentSchemaVersion,
			Name:          "t",
			Steps:         steps,
			Edges:         []dag.Edge{},
		}
	}

	cases := []struct {
		name      string
		def       *dag.Definition
		wantErrs  []issueRef
		wantWarns []issueRef
	}{
		{
			name: "valid baseline",
			def:  pair(dag.Edge{From: "a", To: "b"}),
		},
		{
			name: "step id at 64-char limit",
			def:  single(noop("a" + strings.Repeat("x", 63))),
		},
		{
			name:     "step id over 64-char limit",
			def:      single(noop("a" + strings.Repeat("x", 64))),
			wantErrs: []issueRef{{dag.CodeInvalidStepID, "steps[0].id"}},
		},
		{
			name: "reserved characters in step ids",
			def:  single(noop("a.b"), noop("a#b")),
			wantErrs: []issueRef{
				{dag.CodeInvalidStepID, "steps[0].id"},
				{dag.CodeInvalidStepID, "steps[1].id"},
			},
			wantWarns: []issueRef{
				{dag.CodeIsolatedStep, "steps[0]"},
				{dag.CodeIsolatedStep, "steps[1]"},
			},
		},
		{
			name:     "unknown step type",
			def:      single(dag.Step{ID: "x", Type: "bogus"}),
			wantErrs: []issueRef{{dag.CodeUnknownStepType, "steps[0].type"}},
		},
		{
			name: "llm step without config",
			def:  single(dag.Step{ID: "draft", Type: dag.StepLLM}),
			wantErrs: []issueRef{
				{dag.CodeConfigFieldRequired, "steps[0].config.model"},
				{dag.CodeConfigFieldRequired, "steps[0].config"},
			},
		},
		{
			name: "llm with messages only",
			def: single(dag.Step{ID: "draft", Type: dag.StepLLM, Config: &dag.LLMConfig{
				Model:    "anthropic/claude-sonnet-5",
				Messages: []dag.LLMMessage{{Role: "user", Content: "hi"}},
			}}),
		},
		{
			name: "name at 128-byte limit",
			def: &dag.Definition{
				SchemaVersion: dag.CurrentSchemaVersion,
				Name:          strings.Repeat("n", 128),
				Steps:         []dag.Step{noop("a")},
			},
		},
		{
			name: "name over 128-byte limit",
			def: &dag.Definition{
				SchemaVersion: dag.CurrentSchemaVersion,
				Name:          strings.Repeat("n", 129),
				Steps:         []dag.Step{noop("a")},
			},
			wantErrs: []issueRef{{dag.CodeLimitExceeded, "name"}},
		},
		{
			name: "when expression at 1024-byte limit",
			def:  pair(dag.Edge{From: "a", To: "b", When: strings.Repeat("x", 1024)}),
		},
		{
			name:     "when expression over 1024-byte limit",
			def:      pair(dag.Edge{From: "a", To: "b", When: strings.Repeat("x", 1025)}),
			wantErrs: []issueRef{{dag.CodeLimitExceeded, "edges[0].when"}},
		},
		{
			name: "loop max_iterations at cap",
			def: pair(
				dag.Edge{From: "a", To: "b"},
				dag.Edge{From: "b", To: "a", Type: dag.EdgeLoop, Condition: "output.go", MaxIterations: 100},
			),
		},
		{
			name: "loop max_iterations negative",
			def: pair(
				dag.Edge{From: "a", To: "b"},
				dag.Edge{From: "b", To: "a", Type: dag.EdgeLoop, Condition: "output.go", MaxIterations: -2},
			),
			wantErrs: []issueRef{{dag.CodeLimitExceeded, "edges[1].max_iterations"}},
		},
		{
			name: "branch with conditioned edges and trailing default",
			def: &dag.Definition{
				SchemaVersion: dag.CurrentSchemaVersion,
				Name:          "t",
				Steps: []dag.Step{
					noop("start"),
					{ID: "route", Type: dag.StepBranch},
					noop("a"), noop("b"),
				},
				Edges: []dag.Edge{
					{From: "start", To: "route"},
					{From: "route", To: "a", When: "output.x == 1"},
					{From: "route", To: "b"},
				},
			},
		},
		{
			name: "step touched only by a loop edge is not isolated",
			def: &dag.Definition{
				SchemaVersion: dag.CurrentSchemaVersion,
				Name:          "t",
				Steps:         []dag.Step{noop("a"), noop("b"), noop("c")},
				Edges: []dag.Edge{
					{From: "a", To: "b"},
					{From: "b", To: "c", Type: dag.EdgeLoop, Condition: "output.go", MaxIterations: 3},
				},
			},
		},
		{
			name:      "isolated step warns without failing",
			def:       single(noop("a"), noop("b"), noop("stray"), noop("d")),
			wantWarns: nil, // overridden below: every step here is isolated
		},
	}
	// The all-isolated case: every step in a multi-step, edge-free
	// definition warns.
	cases[len(cases)-1].wantWarns = []issueRef{
		{dag.CodeIsolatedStep, "steps[0]"},
		{dag.CodeIsolatedStep, "steps[1]"},
		{dag.CodeIsolatedStep, "steps[2]"},
		{dag.CodeIsolatedStep, "steps[3]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issues, err := dag.Validate(tc.def)
			if len(tc.wantErrs) == 0 && err != nil {
				t.Fatalf("Validate: want nil error, got: %v", err)
			}
			if len(tc.wantErrs) > 0 && err == nil {
				t.Fatal("Validate: want error, got nil")
			}
			wantIssues(t, issues, tc.wantErrs, tc.wantWarns)
		})
	}
}

// TestValidateCountLimits exercises the step/edge count limits with
// generated definitions (committing multi-hundred-KB fixtures would be
// noise; the limit rule is the same code path either way).
func TestValidateCountLimits(t *testing.T) {
	t.Parallel()

	t.Run("steps over limit", func(t *testing.T) {
		t.Parallel()
		n := dag.MaxSteps + 1
		steps := make([]dag.Step, n)
		edges := make([]dag.Edge, n-1) // chain: no isolated steps, one entry
		for i := range steps {
			steps[i] = dag.Step{ID: fmt.Sprintf("s%d", i), Type: dag.StepNoop}
			if i > 0 {
				edges[i-1] = dag.Edge{From: fmt.Sprintf("s%d", i-1), To: fmt.Sprintf("s%d", i)}
			}
		}
		def := &dag.Definition{SchemaVersion: dag.CurrentSchemaVersion, Name: "big", Steps: steps, Edges: edges}
		issues, err := dag.Validate(def)
		if err == nil {
			t.Fatal("Validate: want error, got nil")
		}
		wantIssues(t, issues, []issueRef{{dag.CodeLimitExceeded, "steps"}}, nil)
	})

	t.Run("edges over limit", func(t *testing.T) {
		t.Parallel()
		edges := make([]dag.Edge, dag.MaxEdges+1)
		for i := range edges {
			edges[i] = dag.Edge{From: "a", To: "b"} // duplicates are not a 1.3 rule
		}
		def := &dag.Definition{
			SchemaVersion: dag.CurrentSchemaVersion,
			Name:          "big",
			Steps:         []dag.Step{{ID: "a", Type: dag.StepNoop}, {ID: "b", Type: dag.StepNoop}},
			Edges:         edges,
		}
		issues, err := dag.Validate(def)
		if err == nil {
			t.Fatal("Validate: want error, got nil")
		}
		wantIssues(t, issues, []issueRef{{dag.CodeLimitExceeded, "edges"}}, nil)
	})
}

// TestDecodeSizeLimit verifies the 1 MiB gate at the top of Decode: an
// oversized document is rejected before parsing, reported alone, with the
// limit_exceeded code.
func TestDecodeSizeLimit(t *testing.T) {
	t.Parallel()

	data := make([]byte, dag.MaxDefinitionBytes+1)
	for i := range data {
		data[i] = ' '
	}
	def, err := dag.Decode(data)
	if err == nil {
		t.Fatal("Decode: want error, got nil")
	}
	if def != nil {
		t.Errorf("Decode: want nil definition on error, got %+v", def)
	}
	var issue *dag.ValidationIssue
	if !errors.As(err, &issue) {
		t.Fatalf("error does not unwrap to *dag.ValidationIssue: %v", err)
	}
	if issue.Code != dag.CodeLimitExceeded {
		t.Errorf("issue code = %q, want %q", issue.Code, dag.CodeLimitExceeded)
	}
}

// TestValidateErrorShape verifies the machine-readable contract: the
// joined error unwraps to individual issues via errors.As, and rendered
// messages carry path, message, and code.
func TestValidateErrorShape(t *testing.T) {
	t.Parallel()

	def := decodeFixture(t, "invalid_structural", "duplicate_step_id.json")
	_, err := dag.Validate(def)
	if err == nil {
		t.Fatal("Validate: want error, got nil")
	}
	var issue *dag.ValidationIssue
	if !errors.As(err, &issue) {
		t.Fatalf("error does not unwrap to *dag.ValidationIssue: %v", err)
	}
	for _, want := range []string{"steps[1].id: ", "(duplicate_step_id)", "invalid workflow definition:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q; full error:\n%v", want, err)
		}
	}
}

func TestValidateNilDefinition(t *testing.T) {
	t.Parallel()

	if _, err := dag.Validate(nil); err == nil {
		t.Error("Validate(nil): want error, got nil")
	}
}
