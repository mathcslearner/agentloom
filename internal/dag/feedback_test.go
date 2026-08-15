package dag_test

import (
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

func TestCheckFeedbackTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tmpl    string
		wantErr string
	}{
		{"all tokens", "a ${{ feedback.prior_output }} b ${{ feedback.issues }} ${{ feedback.attempt }}/${{ feedback.max_attempts }}", ""},
		{"no tokens", "just fix it", ""},
		{"tight spacing", "${{feedback.attempt}}", ""},
		{"empty", "   ", "must not be empty"},
		{"unknown token", "revise ${{ steps.a.output }}", "unknown feedback token"},
		{"unterminated", "revise ${{ feedback.issues", "unterminated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := dag.CheckFeedbackTemplate(tc.tmpl)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRenderFeedbackDefault(t *testing.T) {
	t.Parallel()
	got := dag.RenderFeedback(nil, dag.FeedbackData{
		PriorOutput: "the bad output",
		Issues:      "- [json_schema] required at /title: missing",
		Attempt:     2,
		MaxAttempts: 3,
	})
	for _, want := range []string{"attempt 2 of 3", "the bad output", "required at /title", "reply with the corrected output only"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered feedback missing %q; got:\n%s", want, got)
		}
	}
}

func TestRenderFeedbackCustomTemplateAndTruncation(t *testing.T) {
	t.Parallel()
	p := &dag.FeedbackPolicy{
		Template:       "attempt ${{ feedback.attempt }}: ${{ feedback.prior_output }}",
		MaxOutputChars: 5,
	}
	got := dag.RenderFeedback(p, dag.FeedbackData{
		PriorOutput: "0123456789",
		Attempt:     2,
		MaxAttempts: 4,
	})
	if !strings.HasPrefix(got, "attempt 2: 01234") {
		t.Fatalf("unexpected render: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker; got %q", got)
	}
}
