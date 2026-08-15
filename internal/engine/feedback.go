package engine

// This file is the semantic-retry engine's critique machinery (ticket 11.4,
// ADR-013): injecting a pending feedback critique into a claimed step's
// request before it runs, and building the next critique from a failing
// verdict. The routing itself (retry vs. dead-letter) lives in
// completeValidationFailure (validate.go); this file is the pure/derivable
// half it and the claim path call.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// injectFeedback folds a claimed step's pending semantic-retry critique into
// the executor's request (ADR-013, ticket 11.4). It reads the feedback the
// claim copied onto the step (run_steps.feedback, NULL for a first attempt or
// an un-validated step) and, when the executor implements FeedbackInjector,
// asks it to build the augmented config. It returns the config to run and the
// decoded feedback record (nil when there is no pending critique); the record's
// semantic-attempt number threads into the validation chain and the completion
// routing.
//
// Feedback is diagnostic, not load-bearing (unlike the verdict): a corrupt
// materialized record or an injector error is logged and the step re-attempts
// with its un-augmented config — the validate stage still judges the output —
// rather than failing the attempt.
func (e *Engine) injectFeedback(ctx context.Context, step gen.RunStep, executor exec.Executor, config json.RawMessage) (json.RawMessage, *exec.Feedback) {
	if len(step.Feedback) == 0 {
		return config, nil
	}
	logger := log.From(ctx)
	var fb exec.Feedback
	if err := json.Unmarshal(step.Feedback, &fb); err != nil {
		logger.WarnContext(ctx, "corrupt semantic-retry feedback on claim; re-attempting un-augmented",
			slog.Any("error", err))
		return config, nil
	}
	injector, ok := executor.(exec.FeedbackInjector)
	if !ok || strings.TrimSpace(fb.Text) == "" {
		// A step type whose executor injects no critique (or an empty critique)
		// still re-attempts — identically — under the semantic budget; the
		// record rides so the status API sees the depth.
		return config, &fb
	}
	sc := exec.StepContext{StepType: dag.StepType(step.StepType), Config: config}
	augmented, err := injector.WithFeedback(sc, fb)
	if err != nil {
		logger.WarnContext(ctx, "building feedback-augmented request failed; re-attempting un-augmented",
			slog.Any("error", err))
		return config, &fb
	}
	logger.InfoContext(ctx, "semantic retry: injected feedback into request",
		slog.Int("semantic_attempt", fb.SemanticAttempt),
		slog.Int("max_attempts", fb.MaxAttempts))
	return augmented, &fb
}

// buildFeedback constructs the critique the next semantic attempt will carry
// from a failing verdict (ADR-013, ticket 11.4). nextSemanticAttempt is the
// attempt number the critique is for (the failing attempt's semantic number
// plus one); priorAttempt is the durable attempt number of the rejected try.
// For a step type whose executor cannot inject text the record still carries
// the bookkeeping (an empty Text), so the semantic budget and the status API
// stay consistent across step types.
func buildFeedback(policy *dag.ValidationPolicy, stepType dag.StepType, out exec.Output, verdict validate.Verdict, nextSemanticAttempt, priorAttempt int) exec.Feedback {
	maxAttempts := policy.EffectiveMaxAttempts()
	text := ""
	if stepType.IsLLMFamily() {
		var fp *dag.FeedbackPolicy
		if policy != nil {
			fp = policy.Feedback
		}
		text = dag.RenderFeedback(fp, dag.FeedbackData{
			PriorOutput: priorOutputText(stepType, out),
			Issues:      formatVerdictIssues(verdict.Issues),
			Attempt:     nextSemanticAttempt,
			MaxAttempts: maxAttempts,
		})
	}
	return exec.Feedback{
		SchemaVersion:   1,
		SemanticAttempt: nextSemanticAttempt,
		MaxAttempts:     maxAttempts,
		PriorAttempt:    priorAttempt,
		Text:            text,
	}
}

// priorOutputText reduces a rejected output to the text a model produced: the
// llm-family `/text` answer (the model's actual completion, the same sub-tree
// the default validation target scrutinizes), or the compact JSON of the whole
// output for any other step type. Structure/content only — the same payload
// the verdict already exposes.
func priorOutputText(stepType dag.StepType, out exec.Output) string {
	if stepType.IsLLMFamily() && len(out.Data) > 0 {
		var envelope struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(out.Data, &envelope); err == nil && envelope.Text != "" {
			return envelope.Text
		}
	}
	return string(out.Data)
}

// formatVerdictIssues renders a verdict's issues as a bullet list the feedback
// template splices under `${{ feedback.issues }}` — one line per issue,
// `[validator] code at path: message`, structure only (the Issue contract
// never carries instance values). An empty verdict (a target-not-found with no
// detail) yields a single generic line so the critique is never blank.
func formatVerdictIssues(issues []validate.Issue) string {
	if len(issues) == 0 {
		return "- the output did not pass validation"
	}
	var b strings.Builder
	for i, iss := range issues {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- [")
		b.WriteString(iss.Validator)
		b.WriteString("] ")
		b.WriteString(iss.Code)
		if iss.Path != "" {
			b.WriteString(" at ")
			b.WriteString(iss.Path)
		}
		if iss.Message != "" {
			b.WriteString(": ")
			b.WriteString(iss.Message)
		}
	}
	return b.String()
}

// decodeValidationPolicy decodes a step's materialized validation policy
// (ADR-013); nil bytes (no chain authored) → nil policy, nil error. A decode
// error is a corrupt materialized policy — the caller treats it as a
// no-semantic-retry (dead-letter) situation, since the budget cannot be read.
func decodeValidationPolicy(raw json.RawMessage) (*dag.ValidationPolicy, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var p dag.ValidationPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decoding materialized validation policy: %w", err)
	}
	return &p, nil
}

// verdictHistoryEntry is one row of the semantic-retry verdict history carried
// on the DLQ record (ticket 11.4): the attempt number, its semantic depth, and
// the verdict that rejected it. The full evidence trail an exhausted semantic
// retry leaves behind.
type verdictHistoryEntry struct {
	AttemptNo       int32           `json:"attempt_no"`
	SemanticAttempt int             `json:"semantic_attempt"`
	Verdict         json.RawMessage `json:"verdict"`
}

// collectVerdictHistory reads the step's prior validation_failed attempts'
// verdicts (past the requeue baseline is implicit — the current run's attempt
// rows) and appends the current one, so the exhausted-semantic-retry DLQ record
// carries every verdict (the ticket's acceptance criterion). currentVerdict is
// the failing verdict of the attempt being dead-lettered (whose row is not yet
// closed). A read error yields just the current verdict — the DLQ still records
// the terminal evidence.
func collectVerdictHistory(attempts []gen.StepAttempt, currentAttemptNo int32, currentVerdict json.RawMessage) []verdictHistoryEntry {
	var history []verdictHistoryEntry
	semantic := 0
	for _, a := range attempts {
		if a.Outcome == nil || *a.Outcome != "validation_failed" || len(a.Verdict) == 0 {
			continue
		}
		semantic++
		history = append(history, verdictHistoryEntry{
			AttemptNo: a.AttemptNo, SemanticAttempt: semantic, Verdict: a.Verdict,
		})
	}
	history = append(history, verdictHistoryEntry{
		AttemptNo: currentAttemptNo, SemanticAttempt: semantic + 1, Verdict: currentVerdict,
	})
	return history
}
