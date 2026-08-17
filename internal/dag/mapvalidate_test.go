package dag_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// validateJSON decodes and validates a definition document, returning the
// issues (the document must be codec-valid).
func validateJSON(t *testing.T, doc string) []*dag.ValidationIssue {
	t.Helper()
	def, err := dag.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("Decode (fixture must be codec-valid): %v", err)
	}
	issues, _ := dag.Validate(def)
	return issues
}

// TestMapValidationTable covers the map fan-out validation rules (ADR-015,
// ticket 13.4): a well-formed map + template passes; a body naming no template,
// a multi-sink body, an item root on a non-template step, a map step referencing
// a local template step, and a template step referencing a non-local step each
// fail with the expected code.
func TestMapValidationTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		doc  string
		errs []issueRef
	}{
		{
			name: "valid single-step body",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"do ${{ item }}","max_tokens":8}}]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: nil,
		},
		{
			name: "valid multi-step body with item_index and internal ref",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[
					{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item_index }} ${{ item }}","max_tokens":8}},
					{"id":"c","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ steps.a.output.text }}","max_tokens":8}}],
					"edges":[{"from":"a","to":"c"}]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: nil,
		},
		{
			name: "body names no template",
			doc: `{"schema_version":1,"name":"x",
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"missing"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: []issueRef{{dag.CodeMapBodyUnknown, "steps[1].config.body"}},
		},
		{
			name: "multi-sink body",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[
					{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item }}","max_tokens":8}},
					{"id":"c","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item }}","max_tokens":8}}]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: []issueRef{{dag.CodeTemplateSectionInvalid, "templates.b"}},
		},
		{
			name: "item root on a non-template step",
			doc: `{"schema_version":1,"name":"x",
				"steps":[
					{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item }}","max_tokens":8}}],
				"edges":[]}`,
			errs: []issueRef{{dag.CodeTemplateRefInvalid, "steps[0].config.prompt"}},
		},
		{
			name: "template step references a non-local step",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[
					{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ steps.src.output.text }}","max_tokens":8}}]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: []issueRef{{dag.CodeTemplateRefUnknownStep, "templates.b.steps[0].config.prompt"}},
		},
		{
			name: "empty template",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: []issueRef{{dag.CodeTemplateSectionInvalid, "templates.b.steps"}},
		},
		{
			name: "negative max_items",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item }}","max_tokens":8}}]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b","max_items":-1}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: []issueRef{{dag.CodeConfigFieldInvalid, "steps[1].config.max_items"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wantIssues(t, validateJSON(t, tc.doc), tc.errs, nil)
		})
	}
}
