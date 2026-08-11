package api

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

func TestDefinitionIssuesFromDecode(t *testing.T) {
	t.Parallel()

	// Two codec-level problems in one document: Decode reports both,
	// joined; the flattener must surface each with its path.
	_, err := dag.Decode([]byte(`{
		"schema_version": 1,
		"name": "bad",
		"steps": [{"id": "a", "type": "noop", "junk": true}],
		"edges": "not an array"
	}`))
	if err == nil {
		t.Fatal("Decode: want error, got nil")
	}
	issues := DefinitionIssues(err)
	if len(issues) < 2 {
		t.Fatalf("issues = %+v, want at least 2", issues)
	}
	paths := map[string]bool{}
	for _, i := range issues {
		if i.Severity != string(dag.SeverityError) {
			t.Errorf("issue %+v severity = %q, want error", i, i.Severity)
		}
		if i.Path == "" {
			t.Errorf("issue %+v lost its path", i)
		}
		paths[i.Path] = true
	}
	if !paths["steps[0].junk"] || !paths["edges"] {
		t.Errorf("issue paths = %v, want steps[0].junk and edges", paths)
	}
}

func TestDefinitionIssuesFromValidate(t *testing.T) {
	t.Parallel()

	def := &dag.Definition{
		SchemaVersion: 1,
		Name:          "bad",
		Steps:         []dag.Step{{ID: "a", Type: dag.StepNoop}},
		Edges:         []dag.Edge{{From: "a", To: "ghost"}},
	}
	found, err := dag.Validate(def)
	if err == nil {
		t.Fatal("Validate: want error, got nil")
	}

	// Both entry points agree on the wire shape: flattening the joined
	// error and converting the findings list yield the same issues.
	fromErr := DefinitionIssues(err)
	fromList := ValidationIssues(found)
	if len(fromErr) == 0 || len(fromList) == 0 {
		t.Fatalf("no issues: fromErr=%v fromList=%v", fromErr, fromList)
	}
	var hit bool
	for _, i := range fromList {
		if i.Code == string(dag.CodeUnknownEdgeEndpoint) && i.Path == "edges[0].to" {
			hit = true
		}
	}
	if !hit {
		t.Errorf("issues %+v lack the path-qualified unknown_edge_endpoint", fromList)
	}
}
