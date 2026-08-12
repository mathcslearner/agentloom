package dag

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
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
	CodeConfigFieldInvalid      ValidationCode = "config_field_invalid"
	CodeBranchNoOutEdges        ValidationCode = "branch_no_out_edges"
	CodeBranchEdgeUnconditioned ValidationCode = "branch_edge_unconditioned"
	CodeLoopFieldRequired       ValidationCode = "loop_field_required"
	CodeLoopFieldForbidden      ValidationCode = "loop_field_forbidden"
	CodeRetryFieldRequired      ValidationCode = "retry_field_required"
	CodeRetryFieldInvalid       ValidationCode = "retry_field_invalid"
	CodeTimeoutFieldInvalid     ValidationCode = "timeout_field_invalid"
	CodeMaxWallClockInvalid     ValidationCode = "max_wall_clock_field_invalid"
	CodeLimitExceeded           ValidationCode = "limit_exceeded"
	CodeCycle                   ValidationCode = "cycle_detected"
	CodeLoopEdgeNotAncestor     ValidationCode = "loop_edge_not_ancestor"
	CodeExprInvalid             ValidationCode = "invalid_expression"
	CodeExprNotBool             ValidationCode = "expression_not_boolean"
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
// rules, CEL predicate compilation (`when`/`condition`), entry-step
// existence, and the definition limits. It reports every violation in one
// pass.
//
// issues holds all findings, warnings included, in deterministic order;
// err joins the error-severity issues (nil when the definition is valid,
// even with warnings) and unwraps to the individual *ValidationIssue
// values via errors.As / errors.Join.
//
// Validate assumes codec-level integrity (Decode's job) but tolerates
// hand-built definitions: a nil or wrongly-typed Config is reported as
// missing required fields, an unregistered step type as unknown_step_type.
// The graph-semantic rules (ticket 1.4) — no cycles except marked loop
// edges, loop-edge ancestry — run last and only when the graph is
// well-formed (no duplicate IDs, no unknown edge endpoints), so a broken
// endpoint does not cascade into spurious cycle reports.
func Validate(def *Definition) (issues []*ValidationIssue, err error) {
	if def == nil {
		return nil, errors.New("dag: Validate called with nil definition")
	}
	v := &validator{}

	v.checkLimits(def)
	v.checkMaxWallClock(def.MaxWallClock)
	stepIndex := v.checkSteps(def)
	v.checkEdges(def, stepIndex)
	v.checkGraph(def, stepIndex)
	if !v.has(CodeDuplicateStepID, CodeUnknownEdgeEndpoint) {
		v.checkGraphSemantics(def)
	}

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

// has reports whether any recorded issue carries one of the given codes.
func (v *validator) has(codes ...ValidationCode) bool {
	for _, i := range v.issues {
		for _, c := range codes {
			if i.Code == c {
				return true
			}
		}
	}
	return false
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
		v.checkRetry(path, s.Retry)
		v.checkTimeout(path, s.Timeout)
	}
	return index
}

// checkTimeout enforces the per-attempt execution timeout bounds (ticket
// 5.3): parseable, positive, at most MaxStepTimeout. An empty string means
// the key was absent — no timeout, nothing to check.
func (v *validator) checkTimeout(path, val string) {
	if val == "" {
		return
	}
	path += ".timeout"
	d, err := time.ParseDuration(val)
	switch {
	case err != nil:
		v.add(CodeTimeoutFieldInvalid, path, "not a Go duration string: %v", err)
	case d <= 0:
		v.add(CodeTimeoutFieldInvalid, path, "must be positive, got %q", val)
	case d > MaxStepTimeout:
		v.add(CodeTimeoutFieldInvalid, path, "must be at most %s, got %q", MaxStepTimeout, val)
	}
}

// checkMaxWallClock enforces the run wall-clock deadline bounds (ticket
// 5.6): parseable, positive, at most MaxRunWallClock. An empty string
// means the key was absent — no deadline, nothing to check.
func (v *validator) checkMaxWallClock(val string) {
	if val == "" {
		return
	}
	const path = "max_wall_clock"
	d, err := time.ParseDuration(val)
	switch {
	case err != nil:
		v.add(CodeMaxWallClockInvalid, path, "not a Go duration string: %v", err)
	case d <= 0:
		v.add(CodeMaxWallClockInvalid, path, "must be positive, got %q", val)
	case d > MaxRunWallClock:
		v.add(CodeMaxWallClockInvalid, path, "must be at most %s, got %q", MaxRunWallClock, val)
	}
}

// checkRetry enforces ADR-006's retry-policy bounds. The codec already
// rejected unknown fields and unknown enum spellings; here the numeric
// and duration bounds apply, plus which classes `retry_on` may name —
// only the retryable subset (transient, timeout): permanent and
// cancelled are never retryable by construction, and validation_failed
// is reserved until M11 wires the semantic-retry loop. Unknown class
// strings are re-checked so hand-built definitions (which never pass
// the codec) report cleanly.
func (v *validator) checkRetry(path string, rp *RetryPolicy) {
	if rp == nil {
		return
	}
	path += ".retry"
	if rp.MaxAttempts < 0 || rp.MaxAttempts > MaxRetryAttempts {
		v.add(CodeRetryFieldInvalid, path+".max_attempts", "must be between 1 and %d, got %d", MaxRetryAttempts, rp.MaxAttempts)
	}
	if rp.Jitter != "" && !slices.Contains(jitterModes, rp.Jitter) {
		v.add(CodeRetryFieldInvalid, path+".jitter", "unknown jitter mode %q", string(rp.Jitter))
	}
	if b := rp.Backoff; b != nil {
		initial := v.checkBackoffDuration(path+".backoff.initial", b.Initial, MaxBackoffInitial)
		ceiling := v.checkBackoffDuration(path+".backoff.cap", b.Cap, MaxBackoffCap)
		if initial > 0 && ceiling > 0 && ceiling < initial {
			v.add(CodeRetryFieldInvalid, path+".backoff.cap", "must be at least backoff.initial (%s), got %s", b.Initial, b.Cap)
		}
		if b.Multiplier != 0 && (b.Multiplier < 1 || b.Multiplier > MaxBackoffMultiplier) {
			v.add(CodeRetryFieldInvalid, path+".backoff.multiplier", "must be between 1 and %d, got %v", MaxBackoffMultiplier, b.Multiplier)
		}
	}
	if rp.RetryOn != nil && len(rp.RetryOn) == 0 {
		v.add(CodeRetryFieldInvalid, path+".retry_on", "must not be empty when present (omit the key for the engine default)")
	}
	seen := make(map[ErrorClass]bool, len(rp.RetryOn))
	for i, c := range rp.RetryOn {
		entry := fmt.Sprintf("%s.retry_on[%d]", path, i)
		if seen[c] {
			v.add(CodeRetryFieldInvalid, entry, "duplicate error class %q", string(c))
			continue
		}
		seen[c] = true
		switch c {
		case ClassTransient, ClassTimeout:
		case ClassValidationFailed:
			v.add(CodeRetryFieldInvalid, entry, "%q is reserved for semantic retries (M11, ADR-013) and cannot be used yet", string(c))
		case ClassPermanent, ClassCancelled:
			v.add(CodeRetryFieldInvalid, entry, "%q is never retryable", string(c))
		default:
			v.add(CodeRetryFieldInvalid, entry, "unknown error class %q", string(c))
		}
	}
}

// checkBackoffDuration enforces one backoff duration field: required
// (both initial and cap must be explicit when a backoff block is
// present — ADR-006), parseable, positive, bounded. Returns the parsed
// duration, or 0 when invalid (the caller's cap ≥ initial check runs
// only on valid pairs).
func (v *validator) checkBackoffDuration(path, val string, bound time.Duration) time.Duration {
	if val == "" {
		v.add(CodeRetryFieldRequired, path, "required field is missing")
		return 0
	}
	d, err := time.ParseDuration(val)
	switch {
	case err != nil:
		v.add(CodeRetryFieldInvalid, path, "not a Go duration string: %v", err)
		return 0
	case d <= 0:
		v.add(CodeRetryFieldInvalid, path, "must be positive, got %q", val)
		return 0
	case d > bound:
		v.add(CodeRetryFieldInvalid, path, "must be at most %s, got %q", bound, val)
		return 0
	}
	return d
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
	case StepSleep:
		// Parseability is checked here, not at runtime, for the same reason
		// CEL predicates compile at validation time: a definition the engine
		// accepts must not explode mid-run on a malformed literal.
		if c := cfg[SleepConfig](s); c.Duration == "" {
			v.add(CodeConfigFieldRequired, path+".config.duration", "required field is missing")
		} else if d, err := time.ParseDuration(c.Duration); err != nil {
			v.add(CodeConfigFieldInvalid, path+".config.duration", "not a Go duration string: %v", err)
		} else if d <= 0 {
			v.add(CodeConfigFieldInvalid, path+".config.duration", "must be positive, got %q", c.Duration)
		}
	case StepFailNTimes:
		// Zero means absent (the key cannot be distinguished from an explicit
		// 0, mirroring Edge.MaxIterations); n == 0 would be noop anyway.
		switch c := cfg[FailNTimesConfig](s); {
		case c.N == 0:
			v.add(CodeConfigFieldRequired, path+".config.n", "required field is missing")
		case c.N < 0:
			v.add(CodeConfigFieldInvalid, path+".config.n", "must be at least 1, got %d", c.N)
		}
	case StepCounter:
		if cfg[CounterConfig](s).Path == "" {
			v.add(CodeConfigFieldRequired, path+".config.path", "required field is missing")
		}
	case StepEffectfulEcho:
		switch c := cfg[EffectfulEchoConfig](s); {
		case c.Path == "":
			v.add(CodeConfigFieldRequired, path+".config.path", "required field is missing")
		case c.FailTimes < 0:
			v.add(CodeConfigFieldInvalid, path+".config.fail_times", "must not be negative, got %d", c.FailTimes)
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
// length limit and must compile (ticket 1.5) — only the semantically
// meaningful predicate of each edge kind is compiled, and over-length
// expressions are not (they are already rejected, and the length cap
// bounds compile work).
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
			switch {
			case e.Condition == "":
				v.add(CodeLoopFieldRequired, path+".condition", "required on a loop edge")
			case len(e.Condition) > MaxExprLen:
				v.add(CodeLimitExceeded, path+".condition", "expression is %d bytes (max %d)", len(e.Condition), MaxExprLen)
			default:
				v.checkExpr(path+".condition", e.Condition)
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
			switch {
			case len(e.When) > MaxExprLen:
				v.add(CodeLimitExceeded, path+".when", "expression is %d bytes (max %d)", len(e.When), MaxExprLen)
			case e.When != "":
				v.checkExpr(path+".when", e.When)
			}
		}
	}
}

// checkExpr compiles a `when`/`condition` predicate (ticket 1.5) and
// reports compile failures — one issue per CEL error, message prefixed
// with the 1-based line:col position — and non-boolean result types.
func (v *validator) checkExpr(path, src string) {
	_, err := CompileExpr(src)
	if err == nil {
		return
	}
	var notBool *ExprNotBoolError
	if errors.As(err, &notBool) {
		v.add(CodeExprNotBool, path, "%s", notBool.Error())
		return
	}
	for _, sub := range flatten(err) {
		var ce *ExprError
		if errors.As(sub, &ce) {
			v.add(CodeExprInvalid, path, "%s", ce.Error())
		} else {
			v.add(CodeExprInvalid, path, "%v", sub)
		}
	}
}

// flatten expands a joined error into its parts, or returns it as-is.
func flatten(err error) []error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

// checkGraph runs the degree-based graph rules: at least one entry step,
// isolated-step warnings, and the branch out-edge firing-rule shape.
// Loop edges never count toward readiness degrees (ADR-003), but any edge
// — loop included — connects a step for the isolated-step check. An edge
// with an unresolved endpoint (already reported as unknown_edge_endpoint)
// does not exist for the degree rules, so a ghost endpoint cannot cascade
// into a spurious no_entry_step or suppress an isolated-step warning; the
// branch firing-rule shape still covers every declared out-edge, since it
// concerns `when` placement, not graph connectivity.
func (v *validator) checkGraph(def *Definition, stepIndex map[string]int) {
	if len(def.Steps) == 0 {
		return // no_steps already reported; nothing graph-shaped to check
	}
	inDegree := make(map[string]int)   // resolved normal edges only
	touched := make(map[string]bool)   // any resolved edge, loop included
	outEdges := make(map[string][]int) // normal out-edge indices per source, declaration order
	for i, e := range def.Edges {
		_, fromOK := stepIndex[e.From]
		_, toOK := stepIndex[e.To]
		if fromOK && toOK {
			touched[e.From] = true
			touched[e.To] = true
		}
		if e.IsLoop() {
			continue
		}
		if fromOK {
			outEdges[e.From] = append(outEdges[e.From], i)
		}
		if fromOK && toOK {
			inDegree[e.To]++
		}
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

// checkGraphSemantics runs the ticket 1.4 rules from ADR-003 "Loop edges":
// the graph with all loop edges removed must be acyclic (marked loop edges
// are the only sanctioned cycles), and each loop edge's `to` must be an
// ancestor of its `from` in that acyclic graph — the `to`→`from` paths are
// the loop body M14 clones per iteration. Only called once the graph is
// well-formed (unique IDs, endpoints resolve), so NewGraph cannot fail.
func (v *validator) checkGraphSemantics(def *Definition) {
	g, err := NewGraph(def)
	if err != nil {
		return // unreachable given the gate in Validate; stay defensive
	}
	for _, c := range g.findCycles() {
		v.add(CodeCycle, fmt.Sprintf("edges[%d]", c.edgeIdx),
			"edge closes the cycle %s (mark a loop edge instead: only loop edges may form cycles)", c.pathString())
	}
	for _, ei := range g.loopEdges {
		e := def.Edges[ei]
		if !g.reaches(g.index[e.To], g.index[e.From]) {
			v.add(CodeLoopEdgeNotAncestor, fmt.Sprintf("edges[%d]", ei),
				"loop edge target %q is not an ancestor of source %q (no loop body to iterate)", e.To, e.From)
		}
	}
}
