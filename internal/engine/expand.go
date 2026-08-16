package engine

// The planner-expansion helpers for the completion transaction (ticket 13.3,
// ADR-015). completeSuccess (complete.go) composes store.ExpandRun into the
// origin planner's completion transaction — after the claim-fenced SucceedStep,
// before the out-edge fan-out — so the graph mutation is atomic with the
// planner's completion and a zombie completion (fenced at SucceedStep) never
// expands. This file holds the pure pieces: decoding the plan from the
// completion output, reading the planner's per-expansion cap, and synthesizing
// the fail verdict a plan rejection routes through the semantic-retry loop.

import (
	"encoding/json"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// expansionValidatorName labels the synthetic "expansion" validator whose
// issues a rejected plan carries into the semantic-retry feedback and the DLQ
// verdict history — so a plan rejection reads exactly like any other
// validation failure (ADR-015: plan-attributable rejections reuse the
// validation_failed route).
const expansionValidatorName = "expansion"

// planFromOutput extracts and decodes the PlanOutput from a planner
// completion's output.json (ticket 13.3). The implicit json_schema validator
// already gated the plan's JSON shape before this step reached completeSuccess,
// so output.json is present and schema-conforming; a decode failure here is a
// residual plan defect routed through the semantic-retry loop, not a crash.
func planFromOutput(out exec.Output) (*dag.PlanOutput, error) {
	var o struct {
		JSON json.RawMessage `json:"json"`
	}
	if len(out.Data) > 0 {
		if err := json.Unmarshal(out.Data, &o); err != nil {
			return nil, fmt.Errorf("decoding planner output envelope: %w", err)
		}
	}
	if len(o.JSON) == 0 {
		return nil, fmt.Errorf("planner output carries no json plan")
	}
	return dag.DecodePlanOutput(o.JSON)
}

// plannerMaxAddedSteps reads the origin planner's per-expansion cap override
// (its config max_added_steps) off the materialized run_steps.config. Zero
// (absent, or a decode miss on a config the executor already accepted) means
// no override — the run's resolved per-expansion cap applies (ExpandRun
// enforces min(run cap, this)).
func plannerMaxAddedSteps(step gen.RunStep) int {
	if len(step.Config) == 0 {
		return 0
	}
	cfg, err := dag.DecodeStepConfig(dag.StepPlanner, step.Config)
	if err != nil {
		return 0
	}
	pc, ok := cfg.(*dag.PlannerConfig)
	if !ok || pc == nil {
		return 0
	}
	return pc.MaxAddedSteps
}

// expansionVerdict synthesizes the fail Verdict a plan-attributable rejection
// carries into completeValidationFailure (ticket 13.3): the passing chain
// verdict's per-validator results (provenance) plus one "expansion" validator
// result whose issues are the rejection's error-severity issues. 11.4's
// feedback builder renders those issues into the re-prompt and the terminal
// dead-letter records them as verdict history, so a rejected plan self-heals
// through the same machinery as any other validation failure — no bespoke
// planner feedback path (ADR-015 × ADR-013).
func expansionVerdict(base *validate.Verdict, issues []validate.Issue) validate.Verdict {
	v := validate.Verdict{SchemaVersion: validate.VerdictSchemaVersion, Status: validate.StatusFail}
	if base != nil {
		v.Results = append(v.Results, base.Results...)
		v.Score = base.Score
	}
	v.Issues = issues
	v.Results = append(v.Results, validate.ValidatorResult{
		Name:       expansionValidatorName,
		Status:     validate.StatusFail,
		IssueCount: len(issues),
	})
	return v
}

// expansionIssues maps a dag.ExpansionVerdict's error-severity issues onto
// validate.Issues attributed to the expansion validator — structure only
// (code/path/message), never plan instance values, keeping the verdict and its
// feedback free of output content (secret hygiene, the 11.x convention).
func expansionIssues(ev dag.ExpansionVerdict) []validate.Issue {
	var out []validate.Issue
	for _, iss := range ev.Issues {
		if iss == nil || iss.Severity != dag.SeverityError {
			continue
		}
		out = append(out, validate.Issue{
			Validator: expansionValidatorName,
			Code:      string(iss.Code),
			Path:      iss.Path,
			Message:   iss.Msg,
		})
	}
	return out
}

// planDecodeIssue renders a plan decode failure (the residual defect
// planFromOutput may surface) as a single expansion-validator issue.
func planDecodeIssue(err error) []validate.Issue {
	return []validate.Issue{{
		Validator: expansionValidatorName,
		Code:      string(dag.CodeExpansionFieldInvalid),
		Message:   err.Error(),
	}}
}
