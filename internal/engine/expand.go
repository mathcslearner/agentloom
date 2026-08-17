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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
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

// mapPlanFromOutput generates the fan-out delta a map step's completion applies
// (ticket 13.4, ADR-015): it reads the map's body sub-template from the run's
// definition snapshot and the resolved item list from the completion's output
// (the map executor emitted `{items, indices, count}`), then generates one
// sub-template instance per item plus a gather join
// (dag.GenerateMapExpansion) — the same PlanOutput shape a planner produces,
// but engine-generated. Any failure here is deterministic (a corrupt
// body/config, or a list that is not an array), so the caller fails the step
// permanently rather than redelivering — there is no model to re-prompt.
func mapPlanFromOutput(step gen.RunStep, run gen.Run, out exec.Output) (*dag.PlanOutput, error) {
	cfg, err := dag.DecodeStepConfig(dag.StepMap, step.Config)
	if err != nil {
		return nil, fmt.Errorf("decoding map config: %w", err)
	}
	mc, ok := cfg.(*dag.MapConfig)
	if !ok || mc == nil {
		return nil, fmt.Errorf("map step %q carries no config", step.StepID)
	}
	def, err := dag.Decode(run.Definition)
	if err != nil {
		return nil, fmt.Errorf("decoding run definition snapshot: %w", err)
	}
	tmpl, ok := def.Templates[mc.Body]
	if !ok || tmpl == nil {
		return nil, fmt.Errorf("map body %q names no template in the definition", mc.Body)
	}
	var mo struct {
		Items []json.RawMessage `json:"items"`
	}
	if len(out.Data) > 0 {
		if err := json.Unmarshal(out.Data, &mo); err != nil {
			return nil, fmt.Errorf("decoding map output: %w", err)
		}
	}
	plan, err := dag.GenerateMapExpansion(tmpl, mo.Items, step.StepID)
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

// isCollectErrorsInstance reports whether step is a map instance whose origin
// map declared on_item_failure=collect_errors (ticket 13.4b, ADR-015): the
// signal that its terminal failure should be tolerated (settled `collected`,
// firing the gather) rather than dead-lettered. A non-instance step, a non-map
// origin, or an unreadable/undecodable origin config falls back to false — the
// step then dead-letters normally (fail-fast), never a panic.
func (e *Engine) isCollectErrorsInstance(ctx context.Context, step gen.RunStep) bool {
	if step.OriginStep == nil || step.OriginKind == nil || *step.OriginKind != string(dag.OriginMap) {
		return false
	}
	origin, err := e.store.Steps().Get(ctx, step.RunID, *step.OriginStep)
	if err != nil {
		log.From(ctx).WarnContext(ctx, "collect-errors check: origin map step unreadable; treating as fail-fast",
			slog.String("origin_step", *step.OriginStep), slog.Any("error", err))
		return false
	}
	cfg, err := dag.DecodeStepConfig(dag.StepMap, origin.Config)
	if err != nil {
		return false
	}
	mc, ok := cfg.(*dag.MapConfig)
	return ok && mc != nil && mc.OnItemFailure == dag.ItemCollectErrors
}

// collectErrorMarker is the engine-synthesized output stored on a collected map
// instance (ticket 13.4b): a structural marker the gather collects into its
// ordered result array — the failure class only, never the raw error text
// (which lives on the step's error field for audit), honoring the 11.x output-
// hygiene convention. class is a bounded ADR-006 enum value, safe to embed.
func collectErrorMarker(class dag.ErrorClass) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"map_item_failed":true,"class":%q}`, string(class)))
}

// routeMapRejection routes a rejected map expansion (ticket 13.4, ADR-015): the
// delta is engine-generated, so no re-prompt can fix it — every rejection is
// permanent. A run-guard cap exhaustion (too many instances for the run's
// per-expansion / total-step caps) dead-letters `expansion_cap_exceeded`;
// anything else (an internal bookkeeping error the pure validator caught)
// dead-letters `map_expansion_invalid`. out carries the productive spend, so a
// cost-bearing body's metering is unaffected by the routing.
func (e *Engine) routeMapRejection(ctx context.Context, step gen.RunStep, out exec.Output, rej *store.ExpansionRejectedError, runTrace store.TraceContext) error {
	reason := "map_expansion_invalid"
	if rej.CapExceeded() {
		reason = "expansion_cap_exceeded"
	}
	log.From(ctx).ErrorContext(ctx, "map expansion rejected; failing permanently",
		slog.String("reason", reason), slog.Any("error", rej))
	return e.completeFailure(ctx, step, out, fmt.Errorf("%s: %w", reason, rej), dag.ClassPermanent, runTrace)
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
