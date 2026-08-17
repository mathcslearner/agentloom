package dag_test

import (
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// TestMapUnknownOnItemFailureRejectedAtDecode: an unknown on_item_failure value
// is a codec-level error (a closed enum), reported before validation.
func TestMapUnknownOnItemFailureRejectedAtDecode(t *testing.T) {
	t.Parallel()
	doc := `{"schema_version":1,"name":"x",
		"templates":{"b":{"steps":[{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item }}","max_tokens":8}}]}},
		"steps":[
			{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
			{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b","on_item_failure":"retry_everything"}}],
		"edges":[{"from":"src","to":"m"}]}`
	_, err := dag.Decode([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "on_item_failure") {
		t.Fatalf("Decode error = %v, want one naming on_item_failure", err)
	}
}

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
			name: "collect_errors with single-step body is valid",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item }}","max_tokens":8}}]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b","on_item_failure":"collect_errors"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: nil,
		},
		{
			name: "collect_errors with multi-step body is rejected",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[
					{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item }}","max_tokens":8}},
					{"id":"c","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ steps.a.output.text }}","max_tokens":8}}],
					"edges":[{"from":"a","to":"c"}]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b","on_item_failure":"collect_errors"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: []issueRef{{dag.CodeConfigFieldInvalid, "steps[1].config.on_item_failure"}},
		},
		{
			name: "fail_fast with multi-step body is fine",
			doc: `{"schema_version":1,"name":"x",
				"templates":{"b":{"steps":[
					{"id":"a","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ item }}","max_tokens":8}},
					{"id":"c","type":"llm","config":{"model":"mock/sim-1","prompt":"${{ steps.a.output.text }}","max_tokens":8}}],
					"edges":[{"from":"a","to":"c"}]}},
				"steps":[
					{"id":"src","type":"echo","config":{"input":{"items":["a"]}}},
					{"id":"m","type":"map","config":{"items":"${{ steps.src.output.items }}","body":"b","on_item_failure":"fail_fast"}}],
				"edges":[{"from":"src","to":"m"}]}`,
			errs: nil,
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
