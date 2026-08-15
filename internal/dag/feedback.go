package dag

import (
	"fmt"
	"strconv"
	"strings"
)

// This file is the critique-prompt half of the semantic-retry contract
// (ADR-013, ticket 11.4): the token vocabulary a `validation.feedback`
// template may use, the default template, and the render step that folds a
// rejected output and its verdict issues into the next attempt's prompt.
//
// The template is deliberately NOT the 8.2 expression engine: a feedback
// fragment lives on the step envelope, is never step-config-rendered, and
// only ever substitutes four fixed tokens. Sharing one scanner between
// submit-time validation (CheckFeedbackTemplate) and retry-time rendering
// (RenderFeedback) keeps the vocabulary in one place, so a template the
// definition accepts can never reference a token the renderer does not know.

// The feedback-template delimiters — the same `${{ }}` spelling as the 8.2
// expression templates, so authors see one interpolation syntax, but a
// wholly separate (fixed-token) grammar.
const (
	feedbackLeftDelim  = "${{"
	feedbackRightDelim = "}}"
)

// DefaultFeedbackTemplate is the critique prompt used when a step's
// validation.feedback is nil or carries no template (ADR-013, ticket 11.4):
// it states which attempt failed, shows the rejected output (truncated) and
// the verdict issues, and instructs the model to revise.
const DefaultFeedbackTemplate = "This is attempt ${{ feedback.attempt }} of ${{ feedback.max_attempts }}. Your previous output did not pass validation.\n\n" +
	"Previous output:\n${{ feedback.prior_output }}\n\n" +
	"Problems found:\n${{ feedback.issues }}\n\n" +
	"Revise your answer to fix these problems and reply with the corrected output only."

// DefaultFeedbackMaxOutputChars bounds the rejected output spliced into the
// critique prompt when the policy sets no max_output_chars.
const DefaultFeedbackMaxOutputChars = 4000

// The token names a feedback template may reference. Any other `${{ ... }}`
// token is a definition error (CheckFeedbackTemplate) — the narrow surface
// that keeps a template from smuggling in arbitrary expression evaluation.
const (
	feedbackTokenPriorOutput = "feedback.prior_output"
	feedbackTokenIssues      = "feedback.issues"
	feedbackTokenAttempt     = "feedback.attempt"
	feedbackTokenMaxAttempts = "feedback.max_attempts"
)

var feedbackTokens = map[string]bool{
	feedbackTokenPriorOutput: true,
	feedbackTokenIssues:      true,
	feedbackTokenAttempt:     true,
	feedbackTokenMaxAttempts: true,
}

// FeedbackData is what a semantic retry knows about the attempt it is
// re-doing (ticket 11.4): the rejected output already reduced to the text a
// model produced (the llm-family `/text` answer, or compact JSON for other
// step types), a pre-formatted bullet list of the verdict's issues, and the
// attempt bookkeeping. The engine (which imports both dag and validate)
// formats Issues; dag owns only the token substitution so the vocabulary and
// the default template stay next to the validation that gates them.
type FeedbackData struct {
	// PriorOutput is the rejected output as text; truncated to the policy's
	// max_output_chars before splicing.
	PriorOutput string
	// Issues is the pre-formatted, human-readable list of what the verdict
	// rejected — one line per issue, no instance values.
	Issues string
	// Attempt is the semantic attempt number the feedback is for (the retry
	// being built: 2 for the first re-attempt).
	Attempt int
	// MaxAttempts is the step's semantic budget (ValidationPolicy.EffectiveMaxAttempts).
	MaxAttempts int
}

// feedbackSegment is one piece of a scanned template: literal text, or a
// token reference (Token set, Literal empty).
type feedbackSegment struct {
	Literal string
	Token   string
}

// scanFeedbackTemplate splits a template into literal and token segments,
// validating that every `${{ ... }}` names a known feedback token. It is the
// single grammar both CheckFeedbackTemplate and RenderFeedback use. An
// unterminated `${{` or an unknown token is an error.
func scanFeedbackTemplate(tmpl string) ([]feedbackSegment, error) {
	var segs []feedbackSegment
	for {
		open := strings.Index(tmpl, feedbackLeftDelim)
		if open < 0 {
			if tmpl != "" {
				segs = append(segs, feedbackSegment{Literal: tmpl})
			}
			return segs, nil
		}
		if open > 0 {
			segs = append(segs, feedbackSegment{Literal: tmpl[:open]})
		}
		rest := tmpl[open+len(feedbackLeftDelim):]
		end := strings.Index(rest, feedbackRightDelim)
		if end < 0 {
			return nil, fmt.Errorf("unterminated %q in feedback template", feedbackLeftDelim)
		}
		token := strings.TrimSpace(rest[:end])
		if !feedbackTokens[token] {
			return nil, fmt.Errorf("unknown feedback token %q (available: %s)", token, feedbackTokenList())
		}
		segs = append(segs, feedbackSegment{Token: token})
		tmpl = rest[end+len(feedbackRightDelim):]
	}
}

// feedbackTokenList renders the allowed tokens for an error message, in a
// stable order.
func feedbackTokenList() string {
	return strings.Join([]string{
		feedbackTokenPriorOutput, feedbackTokenIssues,
		feedbackTokenAttempt, feedbackTokenMaxAttempts,
	}, ", ")
}

// CheckFeedbackTemplate validates a feedback template at submit time
// (ADR-013, ticket 11.4): it must be non-empty when set and reference only
// known tokens. Called from Validate; the returned error's message is the
// issue detail.
func CheckFeedbackTemplate(tmpl string) error {
	if strings.TrimSpace(tmpl) == "" {
		return fmt.Errorf("must not be empty when set")
	}
	_, err := scanFeedbackTemplate(tmpl)
	return err
}

// RenderFeedback builds the critique-prompt fragment a semantic retry injects
// (ticket 11.4), substituting the policy's template (or the default) with the
// attempt's data. The policy may be nil (defaults for both template and
// truncation). The template was validated at submit time, so an unknown token
// here can only come from the default template (always valid) — a scan error
// falls back to the raw template rather than failing the retry.
func RenderFeedback(p *FeedbackPolicy, data FeedbackData) string {
	tmpl := DefaultFeedbackTemplate
	maxChars := DefaultFeedbackMaxOutputChars
	if p != nil {
		if p.Template != "" {
			tmpl = p.Template
		}
		if p.MaxOutputChars > 0 {
			maxChars = p.MaxOutputChars
		}
	}
	segs, err := scanFeedbackTemplate(tmpl)
	if err != nil {
		return tmpl
	}
	prior := truncateFeedback(data.PriorOutput, maxChars)
	var b strings.Builder
	for _, s := range segs {
		if s.Token == "" {
			b.WriteString(s.Literal)
			continue
		}
		switch s.Token {
		case feedbackTokenPriorOutput:
			b.WriteString(prior)
		case feedbackTokenIssues:
			b.WriteString(data.Issues)
		case feedbackTokenAttempt:
			b.WriteString(strconv.Itoa(data.Attempt))
		case feedbackTokenMaxAttempts:
			b.WriteString(strconv.Itoa(data.MaxAttempts))
		}
	}
	return b.String()
}

// truncateFeedback caps s at maxRunes runes, appending a marker when it cut.
// Rune-safe so a multibyte boundary is never split.
func truncateFeedback(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "… (truncated)"
}
