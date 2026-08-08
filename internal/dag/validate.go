package dag

import (
	"errors"
	"fmt"
	"regexp"
)

// Definition limits (ADR-003 "Limits"). Compiled in; making them
// configurable is deferred until a concrete need appears.
// MaxDefinitionBytes is enforced at the top of Decode — the only place the
// raw bytes exist — and reported alone; the rest are enforced by Validate
// together with every other structural rule.
const (
	MaxSteps           = 10000
	MaxEdges           = 20000
	MaxDefinitionBytes = 1 << 20 // 1 MiB
	MaxNameLen         = 128
	MaxExprLen         = 1024 // when / condition, in bytes
	MaxLoopIterations  = 100
)

// stepIDRe is the step ID rule from ADR-003: `#` and `.` are excluded by
// construction, reserved for M13/M14 instance naming and CEL paths.
var stepIDRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Severity classifies a validation issue: errors reject the definition,
// warnings are surfaced but do not block acceptance (ADR-003).
type Severity string

// Issue severities.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// ValidationCode is a stable, machine-readable identifier for one class of
// structural violation, for programmatic consumers (the builder UI, M17).
// Codes are part of the contract: renaming one is a breaking change.
type ValidationCode string

// The validation code registry.
const (
	CodeNoSteps                 ValidationCode = "no_steps"
	CodeDuplicateStepID         ValidationCode = "duplicate_step_id"
	CodeInvalidStepID           ValidationCode = "invalid_step_id"
	CodeUnknownStepType         ValidationCode = "unknown_step_type"
	CodeUnknownEdgeEndpoint     ValidationCode = "unknown_edge_endpoint"
	CodeNoEntryStep             ValidationCode = "no_entry_step"
	CodeIsolatedStep            ValidationCode = "isolated_step"
	CodeConfigFieldRequired     ValidationCode = "config_field_required"
	CodeConfigFieldConflict     ValidationCode = "config_field_conflict"
	CodeBranchNoOutEdges        ValidationCode = "branch_no_out_edges"
	CodeBranchEdgeUnconditioned ValidationCode = "branch_edge_unconditioned"
	CodeLoopFieldRequired       ValidationCode = "loop_field_required"
	CodeLoopFieldForbidden      ValidationCode = "loop_field_forbidden"
	CodeLimitExceeded           ValidationCode = "limit_exceeded"
)

// ValidationIssue is one structural problem in a definition, qualified by
// the JSON path of the offender (same convention as DecodeError; an empty
// Path refers to the document itself).
type ValidationIssue struct {
	Code     ValidationCode
	Severity Severity
	Path     string
	Msg      string
}

func (i *ValidationIssue) Error() string {
	if i.Path == "" {
		return i.Msg + " (" + string(i.Code) + ")"
	}
	return i.Path + ": " + i.Msg + " (" + string(i.Code) + ")"
}

// Validate enforces ADR-003's structural rules on a decoded (or
// programmatically built) definition: ID uniqueness and syntax, edge
// endpoint existence, per-type required config, loop- and branch-edge
// rules, entry-step existence, and the definition limits. It reports every
// violation in one pass.
//
// issues holds all findings, warnings included, in deterministic order;
// err joins the error-severity issues (nil when the definition is valid,
// even with warnings) and unwraps to the individual *ValidationIssue
// values via errors.As / errors.Join.
//
// Validate assumes codec-level integrity (Decode's job) but tolerates
// hand-built definitions: a nil or wrongly-typed Config is reported as
// missing required fields, an unregistered step type as unknown_step_type.
// Graph-semantic rules — cycles, loop-edge ancestry, reachability — are
// ticket 1.4.
func Validate(def *Definition) (issues []*ValidationIssue, err error) {
	if def == nil {
		return nil, errors.New("dag: Validate called with nil definition")
	}
	v := &validator{}

	v.checkLimits(def)
	stepIndex := v.checkSteps(def)
	v.checkEdges(def, stepIndex)
	v.checkGraph(def)

	return v.issues, v.err()
}

// validator accumulates issues across the validation pass.
type validator struct {
	issues []*ValidationIssue
}

// add records an error-severity issue.
func (v *validator) add(code ValidationCode, path, format string, args ...any) {
	v.issues = append(v.issues, &ValidationIssue{
		Code: code, Severity: SeverityError, Path: path, Msg: fmt.Sprintf(format, args...),
	})
}

// warn records a warning-severity issue.
func (v *validator) warn(code ValidationCode, path, format string, args ...any) {
	v.issues = append(v.issues, &ValidationIssue{
		Code: code, Severity: SeverityWarning, Path: path, Msg: fmt.Sprintf(format, args...),
	})
}

// err joins the error-severity issues into one error, or nil if there are
// none (warnings never block).
func (v *validator) err() error {
	var errs []error
	for _, i := range v.issues {
		if i.Severity == SeverityError {
			errs = append(errs, i)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid workflow definition:\n%w", errors.Join(errs...))
}

// checkLimits enforces the document-level entries of the limits table.
func (v *validator) checkLimits(def *Definition) {
	if len(def.Steps) > MaxSteps {
		v.add(CodeLimitExceeded, "steps", "definition has %d steps (max %d)", len(def.Steps), MaxSteps)
	}
	if len(def.Edges) > MaxEdges {
		v.add(CodeLimitExceeded, "edges", "definition has %d edges (max %d)", len(def.Edges), MaxEdges)
	}
	if len(def.Name) > MaxNameLen {
		v.add(CodeLimitExceeded, "name", "name is %d bytes (max %d)", len(def.Name), MaxNameLen)
	}
}

// checkSteps validates step identity, type registration, and per-type
// config, and returns the ID→index map (first occurrence) used to resolve
// edge endpoints. Syntactically invalid IDs still resolve endpoints, so
// one bad ID does not cascade into spurious unknown-endpoint errors.
func (v *validator) checkSteps(def *Definition) map[string]int {
	if len(def.Steps) == 0 {
		v.add(CodeNoSteps, "steps", "definition has no steps")
	}
	index := make(map[string]int, len(def.Steps))
	for i, s := range def.Steps {
		path := fmt.Sprintf("steps[%d]", i)
		if !stepIDRe.MatchString(s.ID) {
			v.add(CodeInvalidStepID, path+".id", "step ID %q does not match %s", s.ID, stepIDRe)
		}
		if first, dup := index[s.ID]; dup {
			v.add(CodeDuplicateStepID, path+".id", "duplicate step ID %q (first declared at steps[%d])", s.ID, first)
		} else {
			index[s.ID] = i
		}
		if _, registered := stepConfigTypes[s.Type]; !registered {
			v.add(CodeUnknownStepType, path+".type", "unknown step type %q", string(s.Type))
		}
		v.checkStepConfig(path, s)
	}
	return index
}

// cfg returns s.Config as *T, or a zero T when the config is absent or of
// another type, so required-field checks read uniformly.
func cfg[T any](s Step) *T {
	if c, ok := any(s.Config).(*T); ok && c != nil {
		return c
	}
	return new(T)
}

// checkStepConfig enforces the per-type required config fields from
// ADR-003's step-type catalog. Field types and unknown fields are the
// decoder's job; only presence and mutual exclusion are checked here.
func (v *validator) checkStepConfig(path string, s Step) {
	switch s.Type {
	case StepLLM:
		c := cfg[LLMConfig](s)
		v.checkLLMConfig(path, c.Model, c.Prompt, len(c.Messages))
	case StepPlanner:
		c := cfg[PlannerConfig](s)
		v.checkLLMConfig(path, c.Model, c.Prompt, len(c.Messages))
	case StepTool:
		if cfg[ToolConfig](s).Tool == "" {
			v.add(CodeConfigFieldRequired, path+".config.tool", "required field is missing")
		}
	case StepRetrieve:
		c := cfg[RetrieveConfig](s)
		if c.Retriever == "" {
			v.add(CodeConfigFieldRequired, path+".config.retriever", "required field is missing")
		}
		if c.Query == "" {
			v.add(CodeConfigFieldRequired, path+".config.query", "required field is missing")
		}
	case StepMap:
		c := cfg[MapConfig](s)
		if c.Items == "" {
			v.add(CodeConfigFieldRequired, path+".config.items", "required field is missing")
		}
		if c.Body == "" {
			v.add(CodeConfigFieldRequired, path+".config.body", "required field is missing")
		}
	case StepAgent:
		if cfg[AgentConfig](s).Agent == "" {
			v.add(CodeConfigFieldRequired, path+".config.agent", "required field is missing")
		}
	case StepHumanApproval:
		if cfg[HumanApprovalConfig](s).Prompt == "" {
			v.add(CodeConfigFieldRequired, path+".config.prompt", "required field is missing")
		}
	case StepJoin:
		if cfg[JoinConfig](s).Mode == "" {
			v.add(CodeConfigFieldRequired, path+".config.mode", "required field is missing")
		}
	case StepBranch, StepNoop, StepEcho:
		// No required config fields.
	}
}

// checkLLMConfig enforces the shared llm/planner requirement: model plus
// exactly one of prompt or messages.
func (v *validator) checkLLMConfig(path, model, prompt string, nMessages int) {
	if model == "" {
		v.add(CodeConfigFieldRequired, path+".config.model", "required field is missing")
	}
	hasPrompt, hasMessages := prompt != "", nMessages > 0
	switch {
	case hasPrompt && hasMessages:
		v.add(CodeConfigFieldConflict, path+".config", `"prompt" and "messages" are mutually exclusive`)
	case !hasPrompt && !hasMessages:
		v.add(CodeConfigFieldRequired, path+".config", `exactly one of "prompt" or "messages" is required`)
	}
}

// checkEdges validates edge endpoints and the per-edge field rules: loop
// edges require condition and a bounded max_iterations and reject when;
// normal edges reject the loop-only fields; expressions respect the
// length limit.
func (v *validator) checkEdges(def *Definition, stepIndex map[string]int) {
	for i, e := range def.Edges {
		path := fmt.Sprintf("edges[%d]", i)
		if _, ok := stepIndex[e.From]; !ok {
			v.add(CodeUnknownEdgeEndpoint, path+".from", "unknown step %q", e.From)
		}
		if _, ok := stepIndex[e.To]; !ok {
			v.add(CodeUnknownEdgeEndpoint, path+".to", "unknown step %q", e.To)
		}
		if e.IsLoop() {
			if e.When != "" {
				v.add(CodeLoopFieldForbidden, path+".when", `"when" is not valid on a loop edge (its predicate is "condition")`)
			}
			if e.Condition == "" {
				v.add(CodeLoopFieldRequired, path+".condition", "required on a loop edge")
			} else if len(e.Condition) > MaxExprLen {
				v.add(CodeLimitExceeded, path+".condition", "expression is %d bytes (max %d)", len(e.Condition), MaxExprLen)
			}
			switch {
			case e.MaxIterations == 0:
				v.add(CodeLoopFieldRequired, path+".max_iterations", "required on a loop edge")
			case e.MaxIterations < 1 || e.MaxIterations > MaxLoopIterations:
				v.add(CodeLimitExceeded, path+".max_iterations", "must be between 1 and %d, got %d", MaxLoopIterations, e.MaxIterations)
			}
		} else {
			if e.Condition != "" {
				v.add(CodeLoopFieldForbidden, path+".condition", "only valid on loop edges")
			}
			if e.MaxIterations != 0 {
				v.add(CodeLoopFieldForbidden, path+".max_iterations", "only valid on loop edges")
			}
			if len(e.When) > MaxExprLen {
				v.add(CodeLimitExceeded, path+".when", "expression is %d bytes (max %d)", len(e.When), MaxExprLen)
			}
		}
	}
}

// checkGraph runs the degree-based graph rules: at least one entry step,
// isolated-step warnings, and the branch out-edge firing-rule shape.
// Loop edges never count toward readiness degrees (ADR-003), but any edge
// — loop included — connects a step for the isolated-step check.
func (v *validator) checkGraph(def *Definition) {
	if len(def.Steps) == 0 {
		return // no_steps already reported; nothing graph-shaped to check
	}
	inDegree := make(map[string]int)   // normal edges only
	touched := make(map[string]bool)   // any edge, loop included
	outEdges := make(map[string][]int) // normal out-edge indices per source, declaration order
	for i, e := range def.Edges {
		touched[e.From] = true
		touched[e.To] = true
		if e.IsLoop() {
			continue
		}
		inDegree[e.To]++
		outEdges[e.From] = append(outEdges[e.From], i)
	}

	entry := false
	for _, s := range def.Steps {
		if inDegree[s.ID] == 0 {
			entry = true
			break
		}
	}
	if !entry {
		v.add(CodeNoEntryStep, "", "no entry step: every step has an incoming normal edge")
	}

	if len(def.Steps) > 1 {
		for i, s := range def.Steps {
			if !touched[s.ID] {
				v.warn(CodeIsolatedStep, fmt.Sprintf("steps[%d]", i),
					"step %q has no edges; it will run as an independent entry step", s.ID)
			}
		}
	}

	for i, s := range def.Steps {
		if s.Type != StepBranch {
			continue
		}
		outs := outEdges[s.ID]
		if len(outs) == 0 {
			v.add(CodeBranchNoOutEdges, fmt.Sprintf("steps[%d]", i), "branch step %q has no outgoing edges", s.ID)
			continue
		}
		// ADR-003 firing rule: every out-edge carries `when` except at most
		// one trailing default — so any unconditioned edge that is not the
		// last out-edge is a violation, which also catches multiple defaults.
		for pos, ei := range outs {
			if def.Edges[ei].When == "" && pos != len(outs)-1 {
				v.add(CodeBranchEdgeUnconditioned, fmt.Sprintf("edges[%d]", ei),
					"unconditioned out-edge of branch %q must be the single trailing default", s.ID)
			}
		}
	}
}
