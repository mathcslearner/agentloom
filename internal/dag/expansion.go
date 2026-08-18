package dag

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Dynamic-expansion caps (ADR-015). Compiled-in defaults, overridable per
// run through the top-level `expansion` block; making the defaults
// site-configurable is deferred until a concrete need appears, matching the
// definition limits stance (ADR-003).
const (
	// DefaultMaxAddedStepsPerExpansion bounds how many steps a single
	// expansion may inject. A planner may tighten it via its config
	// (PlannerConfig.MaxAddedSteps); it may never loosen it.
	DefaultMaxAddedStepsPerExpansion = 32
	// DefaultMaxExpansions bounds how many expansions one run may perform.
	// A run's expansion count is graph_version - 1 (ADR-004), so this needs
	// no separate counter.
	DefaultMaxExpansions = 100
	// DefaultMaxDepth bounds expansion nesting: a definition-authored planner
	// is depth 0, the steps it injects are depth 1, and a planner injected at
	// depth d produces depth-(d+1) steps. The deepest injected step may not
	// exceed this.
	DefaultMaxDepth = 4
	// DefaultMaxTotalSteps mirrors MaxSteps, the authored-definition ceiling:
	// a run's graph, however it grows at runtime, never exceeds the ceiling a
	// definition itself may declare.
	DefaultMaxTotalSteps = MaxSteps

	// MaxPlanSteps / MaxPlanEdges bound a single PlanOutput document at the
	// decode layer — the structural safety ceiling that stops a runaway
	// planner emitting a huge plan before any graph work. The per-expansion
	// cap (max_added_steps) is the smaller semantic bound checked later.
	MaxPlanSteps = 1000
	MaxPlanEdges = 2000

	// PlanSchemaVersion is the PlanOutput format version this engine reads,
	// versioned independently of the workflow-definition schema so the plan
	// contract can evolve without a definition bump (ADR-015).
	PlanSchemaVersion = 1
)

// ExpansionPolicy is the run's optional dynamic-expansion caps (ADR-015). All
// fields are pointers so an absent key (nil) resolves to the compiled default,
// distinct from an explicit value; an explicit value may only tighten a
// default, never loosen it (Validate enforces the bounds). A run with no
// planner/map/loop steps carries no expansion policy and is unaffected.
type ExpansionPolicy struct {
	// MaxAddedSteps is the run-wide default cap on steps added per expansion;
	// a planner may tighten it further via its own config. Nil = the compiled
	// DefaultMaxAddedStepsPerExpansion.
	MaxAddedSteps *int `json:"max_added_steps,omitempty"`
	// MaxTotalSteps caps the run's whole graph size (initial + all injected).
	// Nil = DefaultMaxTotalSteps (the definition ceiling).
	MaxTotalSteps *int `json:"max_total_steps,omitempty"`
	// MaxExpansions caps how many expansions the run may perform. Nil =
	// DefaultMaxExpansions.
	MaxExpansions *int `json:"max_expansions,omitempty"`
	// MaxDepth caps expansion nesting. Nil = DefaultMaxDepth.
	MaxDepth *int `json:"max_depth,omitempty"`
}

// ExpansionCaps is the resolved (defaults-applied) form of an ExpansionPolicy,
// the shape ValidateExpansion and the engine reason about.
type ExpansionCaps struct {
	MaxAddedStepsPerExpansion int
	MaxTotalSteps             int
	MaxExpansions             int
	MaxDepth                  int
}

// Resolve applies the compiled defaults to an ExpansionPolicy (nil policy =
// all defaults), yielding the caps the engine enforces at expansion time.
func (p *ExpansionPolicy) Resolve() ExpansionCaps {
	caps := ExpansionCaps{
		MaxAddedStepsPerExpansion: DefaultMaxAddedStepsPerExpansion,
		MaxTotalSteps:             DefaultMaxTotalSteps,
		MaxExpansions:             DefaultMaxExpansions,
		MaxDepth:                  DefaultMaxDepth,
	}
	if p == nil {
		return caps
	}
	if p.MaxAddedSteps != nil {
		caps.MaxAddedStepsPerExpansion = *p.MaxAddedSteps
	}
	if p.MaxTotalSteps != nil {
		caps.MaxTotalSteps = *p.MaxTotalSteps
	}
	if p.MaxExpansions != nil {
		caps.MaxExpansions = *p.MaxExpansions
	}
	if p.MaxDepth != nil {
		caps.MaxDepth = *p.MaxDepth
	}
	return caps
}

// PlanOutput is a planner step's validated output (ADR-015): the delta of new
// steps and edges to splice into the running graph. Steps and Edges reuse the
// definition's own types verbatim, so an injected step carries a full config
// and every envelope block (retry, timeout, validation, context, ...) exactly
// as an authored step does. A plan is additive-only: it never deletes or
// mutates an existing step or edge.
type PlanOutput struct {
	SchemaVersion int    `json:"schema_version"`
	Steps         []Step `json:"steps"`
	Edges         []Edge `json:"edges,omitempty"`
}

// DecodePlanOutput parses a PlanOutput document with the same strictness as
// Decode: unknown fields, wrong types, unknown enum values, and missing
// required keys are all reported with the JSON path of the offender, joined
// into one error. It enforces the codec-level contract and the structural
// plan ceilings (MaxPlanSteps/MaxPlanEdges); the graph-semantic rules (id
// syntax, endpoint/anchor resolution, cycles, caps) are ValidateExpansion's
// job, exactly as Decode and Validate split for a whole definition.
func DecodePlanOutput(data []byte) (*PlanOutput, error) {
	var errs errList
	if len(data) > MaxDefinitionBytes {
		errs.errs = append(errs.errs, &ValidationIssue{
			Code: CodeLimitExceeded, Severity: SeverityError,
			Msg: fmt.Sprintf("plan is %d bytes (max %d)", len(data), MaxDefinitionBytes),
		})
		return nil, errs.join()
	}
	if !json.Valid(data) {
		var v any
		err := json.Unmarshal(data, &v)
		errs.add("", "invalid JSON: %v", err)
		return nil, errs.join()
	}
	if jt := jsonTypeOf(data); jt != "object" {
		errs.add("", "expected object, got %s", jt)
		return nil, errs.join()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		errs.add("", "%v", err)
		return nil, errs.join()
	}

	verRaw, ok := fields["schema_version"]
	if !ok {
		errs.add("schema_version", "required field is missing")
		return nil, errs.join()
	}
	ver, ok := decodeInt(verRaw, "schema_version", &errs)
	if !ok {
		return nil, errs.join()
	}
	if ver != PlanSchemaVersion {
		errs.add("schema_version", "unsupported plan schema_version %d (this engine supports %d)", ver, PlanSchemaVersion)
		return nil, errs.join()
	}

	plan := &PlanOutput{SchemaVersion: ver}
	if raw, ok := fields["steps"]; ok {
		plan.Steps = decodeSteps(raw, &errs)
	} else {
		errs.add("steps", "required field is missing")
	}
	if raw, ok := fields["edges"]; ok {
		plan.Edges = decodeEdges(raw, &errs)
	}

	topLevel := map[string]bool{"schema_version": true, "steps": true, "edges": true}
	for _, k := range sortedKeys(fields) {
		if !topLevel[k] {
			errs.add(k, "unknown field")
		}
	}

	if len(plan.Steps) > MaxPlanSteps {
		errs.add("steps", "plan has %d steps (max %d)", len(plan.Steps), MaxPlanSteps)
	}
	if len(plan.Edges) > MaxPlanEdges {
		errs.add("edges", "plan has %d edges (max %d)", len(plan.Edges), MaxPlanEdges)
	}

	if err := errs.join(); err != nil {
		return nil, err
	}
	return plan, nil
}

// ExpansionOriginKind classifies what produced an expansion (ADR-015).
type ExpansionOriginKind string

// Expansion origins. Only planner is a runtime-authored delta in M13.1; map
// (13.4) and loop (14.3) are engine-generated deltas through the same
// primitive.
const (
	OriginPlanner ExpansionOriginKind = "planner"
	OriginMap     ExpansionOriginKind = "map"
	OriginLoop    ExpansionOriginKind = "loop"
)

// ExpansionOrigin identifies the step whose completion carries an expansion
// and its nesting depth (a definition-authored step is depth 0).
type ExpansionOrigin struct {
	Kind   ExpansionOriginKind
	StepID string
	Depth  int
}

// ExpansionAnchorStatus is the run-status information ValidateExpansion needs
// about an existing step to decide whether a new edge may anchor to it. The
// store maps its richer run-step statuses onto these three at expansion time.
type ExpansionAnchorStatus string

// Anchor statuses.
const (
	// AnchorPending: the step has not yet been dispatched (remaining_deps > 0
	// or unfired). A new incoming edge may still gate it.
	AnchorPending ExpansionAnchorStatus = "pending"
	// AnchorActive: the step is ready, running, retrying, or throttled —
	// dispatched but not terminal. A new outgoing edge from it is fine (the
	// origin itself is active); a new incoming edge is not (it is past
	// readiness).
	AnchorActive ExpansionAnchorStatus = "active"
	// AnchorTerminal: succeeded, failed, skipped, dead-lettered, or cancelled.
	// Its out-edges are already resolved, so a new edge from it would dangle
	// unresolved forever.
	AnchorTerminal ExpansionAnchorStatus = "terminal"
)

// ExpansionAnchor is an existing run step's expansion-relevant state.
type ExpansionAnchor struct {
	Type   StepType
	Status ExpansionAnchorStatus
	// Succeeded reports whether the step has completed successfully, so its
	// output is materialized and immutable. An injected step may reference a
	// succeeded step's output (`${{ steps.x.output }}`) even when x is not a
	// normal-edge ancestor — the additive-only relaxation the store's in-tx
	// ref lint (13.2) applies against materialized run rows.
	Succeeded bool
}

// ExpansionInput is the whole-graph context ValidateExpansion needs. It is
// pure data: the caller (the store's in-tx revalidation, 13.2) reads it under
// the run lock and hands it here, so the validator does no IO.
type ExpansionInput struct {
	Plan   PlanOutput
	Origin ExpansionOrigin

	// Existing is the current run graph's steps by id (definition ids and any
	// runtime instance ids like `plan#0`), for id-collision and anchor checks.
	Existing map[string]ExpansionAnchor
	// ExistingEdges is the current run graph's edges, merged with the delta
	// for the acyclicity and loop-ancestry checks.
	ExistingEdges []Edge
	// RunParams is the run's submitted parameters (the run row's params JSON),
	// what the template renderer resolves `${{ run.params.<key> }}` against at
	// runtime. An injected step referencing an undeclared parameter is a
	// plan-attributable rejection. Empty means no params. Optional: the store
	// (13.2) supplies it for the cross-graph ref lint; leaving it nil disables
	// only that lint (14.x engine-generated deltas carry no templated params).
	RunParams json.RawMessage

	// PerExpansionCap is the effective cap for THIS expansion — the caller has
	// already taken min(run default, planner override). Exceeding it is a
	// plan-attributable rejection.
	PerExpansionCap int
	// Caps are the resolved run-level caps for the total-size / count / depth
	// guards, whose exhaustion is a permanent (non-retryable) rejection.
	Caps ExpansionCaps
	// CurrentStepCount is the run graph's step count before this expansion.
	CurrentStepCount int
	// ExpansionsSoFar is graph_version - 1: the count of expansions already
	// committed on this run.
	ExpansionsSoFar int
}

// CapBreach is a structured record of one run-guard cap a rejected expansion
// crossed (ticket 14.4): the limit name, the value the expansion would have
// reached (Current), and the configured ceiling (Cap). The engine renders it
// into a guard_tripped event's "which limit, current value, configured cap"
// contract, instead of parsing the free-text issue message. Populated only for
// the CodeExpansionCapExceeded issues; empty when the rejection is plan-
// attributable (a bad plan shape, not a cap).
type CapBreach struct {
	// Limit is the guard name: max_added_steps | max_total_steps |
	// max_expansions | max_depth.
	Limit string
	// Current is the value the expansion would have reached.
	Current int
	// Cap is the configured ceiling for that guard.
	Cap int
}

// ExpansionVerdict is the outcome of validating an expansion (ADR-015). It
// separates the two rejection classes so the engine routes correctly:
// plan-attributable failures become validation_failed (semantic-retry the
// planner), whereas exhausted run guards are permanent (a better plan cannot
// fix them).
type ExpansionVerdict struct {
	Issues []*ValidationIssue
	// Breaches is the structured form of the run-guard cap issues (ticket
	// 14.4), one per CodeExpansionCapExceeded issue, in the same order.
	Breaches []CapBreach
}

// OK reports whether the expansion is admissible (no error-severity issues).
func (v ExpansionVerdict) OK() bool {
	for _, i := range v.Issues {
		if i.Severity == SeverityError {
			return false
		}
	}
	return true
}

// CapExceeded reports whether any rejection is a run-guard cap
// (expansion_cap_exceeded) — the signal to fail the step permanently rather
// than route it through a semantic retry.
func (v ExpansionVerdict) CapExceeded() bool {
	for _, i := range v.Issues {
		if i.Code == CodeExpansionCapExceeded {
			return true
		}
	}
	return false
}

// Err joins the error-severity issues into one error (nil when admissible),
// unwrapping to the individual *ValidationIssue values, exactly like
// Validate's err().
func (v ExpansionVerdict) Err() error {
	var errs []error
	for _, i := range v.Issues {
		if i.Severity == SeverityError {
			errs = append(errs, i)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid expansion:\n%w", errors.Join(errs...))
}

// ValidateExpansion checks a planner's PlanOutput against the running graph
// under ADR-015: shape and per-step config, injected-id syntax/uniqueness and
// collision with existing ids, edge endpoint resolution against the plan or an
// existing anchor, anchor-status splice rules, whole-merged-graph acyclicity
// and loop-edge ancestry, and the run caps. It is pure and deterministic given
// its input.
//
// It deliberately does not re-lint cross-graph template/context-source
// ancestry (`${{ steps.x }}` references from an injected step to an existing
// one): those resolve against materialized run rows and are revalidated in the
// store's expansion transaction (13.2/13.3), where the resolved outputs exist.
func ValidateExpansion(in ExpansionInput) ExpansionVerdict {
	v := &validator{}
	plan := in.Plan
	stepPath := func(i int) string { return fmt.Sprintf("plan.steps[%d]", i) }
	edgePath := func(i int) string { return fmt.Sprintf("plan.edges[%d]", i) }

	if plan.SchemaVersion != PlanSchemaVersion {
		v.add(CodeExpansionFieldInvalid, "plan.schema_version",
			"unsupported plan schema_version %d (this engine supports %d)", plan.SchemaVersion, PlanSchemaVersion)
	}
	if len(plan.Steps) == 0 {
		v.add(CodeExpansionFieldInvalid, "plan.steps", "a plan must add at least one step")
	}

	// Run-guard caps (permanent on exhaustion): checked on counts alone, so
	// they hold even if the plan's shape is otherwise bad. Each crossing is also
	// recorded structurally (ticket 14.4) so the engine reports the exact limit,
	// current value, and cap in the guard event without parsing the message.
	var breaches []CapBreach
	if n := len(plan.Steps); in.PerExpansionCap > 0 && n > in.PerExpansionCap {
		v.add(CodeExpansionCapExceeded, "plan.steps",
			"plan adds %d steps, exceeding the per-expansion cap of %d", n, in.PerExpansionCap)
		breaches = append(breaches, CapBreach{Limit: "max_added_steps", Current: n, Cap: in.PerExpansionCap})
	}
	if total := in.CurrentStepCount + len(plan.Steps); total > in.Caps.MaxTotalSteps {
		v.add(CodeExpansionCapExceeded, "plan.steps",
			"expansion would bring the run to %d steps, exceeding max_total_steps %d", total, in.Caps.MaxTotalSteps)
		breaches = append(breaches, CapBreach{Limit: "max_total_steps", Current: total, Cap: in.Caps.MaxTotalSteps})
	}
	if in.ExpansionsSoFar+1 > in.Caps.MaxExpansions {
		v.add(CodeExpansionCapExceeded, "plan",
			"this is expansion %d, exceeding max_expansions %d", in.ExpansionsSoFar+1, in.Caps.MaxExpansions)
		breaches = append(breaches, CapBreach{Limit: "max_expansions", Current: in.ExpansionsSoFar + 1, Cap: in.Caps.MaxExpansions})
	}
	if in.Origin.Depth+1 > in.Caps.MaxDepth {
		v.add(CodeExpansionCapExceeded, "plan",
			"injected steps would be at depth %d, exceeding max_depth %d", in.Origin.Depth+1, in.Caps.MaxDepth)
		breaches = append(breaches, CapBreach{Limit: "max_depth", Current: in.Origin.Depth + 1, Cap: in.Caps.MaxDepth})
	}

	// Engine-generated expansions (map fan-out, loop unrolling) mint ids in the
	// reserved `#`-suffixed instance space; a planner injects into the authored
	// id space only (ADR-015).
	allowInstanceIDs := plan.SchemaVersion == PlanSchemaVersion &&
		(in.Origin.Kind == OriginMap || in.Origin.Kind == OriginLoop)

	// Injected step identity + envelope/config validation.
	deltaIDs := make(map[string]int, len(plan.Steps))
	for i, s := range plan.Steps {
		p := stepPath(i)
		if !(stepIDRe.MatchString(s.ID) || (allowInstanceIDs && instanceStepIDRe.MatchString(s.ID))) {
			v.add(CodeInvalidStepID, p+".id", "step ID %q does not match %s", s.ID, stepIDRe)
		}
		if _, exists := in.Existing[s.ID]; exists {
			v.add(CodeExpansionAnchorInvalid, p+".id",
				"injected step ID %q collides with an existing step in the run graph", s.ID)
		}
		if first, dup := deltaIDs[s.ID]; dup {
			v.add(CodeDuplicateStepID, p+".id", "duplicate injected step ID %q (first at plan.steps[%d])", s.ID, first)
		} else {
			deltaIDs[s.ID] = i
		}
		if _, registered := stepConfigTypes[s.Type]; !registered {
			v.add(CodeUnknownStepType, p+".type", "unknown step type %q", string(s.Type))
		}
		v.checkStepConfig(p, s)
		v.checkOutputFormat(p, s)
		v.checkRetry(p, s.Retry)
		v.checkTimeout(p, s.Timeout)
		v.checkCache(p, s.Cache)
		v.checkStepBudget(p, s.Type, s.Budget)
		v.checkValidation(p, s)
		v.checkBlackboard(p, s.Blackboard)
		v.checkContext(p, s)
	}

	// Delta edges: endpoint resolution, additive-only rule, anchor status,
	// and the loop/normal field rules (mirroring checkEdges).
	for i, e := range plan.Edges {
		p := edgePath(i)
		_, fromDelta := deltaIDs[e.From]
		fromAnchor, fromExisting := in.Existing[e.From]
		_, toDelta := deltaIDs[e.To]
		toAnchor, toExisting := in.Existing[e.To]

		if !fromDelta && !fromExisting {
			v.add(CodeUnknownEdgeEndpoint, p+".from",
				"edge names unknown step %q (not in the plan or the run graph)", e.From)
		}
		if !toDelta && !toExisting {
			v.add(CodeUnknownEdgeEndpoint, p+".to",
				"edge names unknown step %q (not in the plan or the run graph)", e.To)
		}
		// Additive-only: an edge between two pre-existing steps would mutate
		// the frozen graph.
		if fromExisting && !fromDelta && toExisting && !toDelta {
			v.add(CodeExpansionAnchorInvalid, p,
				"an expansion edge must touch an injected step; %q → %q are both existing", e.From, e.To)
		}
		// Anchor status: a new edge FROM an existing step is fine while it is
		// non-terminal (the origin itself is active); TO an existing step only
		// while it is still pending.
		if fromExisting && !fromDelta && fromAnchor.Status == AnchorTerminal {
			v.add(CodeExpansionAnchorInvalid, p+".from",
				"cannot splice an edge from %q: it is already terminal (its out-edges are resolved)", e.From)
		}
		if toExisting && !toDelta && toAnchor.Status != AnchorPending {
			v.add(CodeExpansionAnchorInvalid, p+".to",
				"cannot splice an edge to %q: it is %s, not pending (a new dependency would never resolve)", e.To, toAnchor.Status)
		}
		v.checkExpansionEdgeFields(p, e)
	}

	// Whole-merged-graph acyclicity + loop-edge ancestry, plus the cross-graph
	// template/context ref lint. Only attempted when ids are unique and
	// endpoints resolve, so NewGraph cannot fail and a broken endpoint does not
	// cascade into a spurious cycle report — the same gate Validate uses. The
	// merged graph is built once and shared by both checks.
	if !v.has(CodeDuplicateStepID, CodeUnknownEdgeEndpoint, CodeExpansionAnchorInvalid, CodeInvalidStepID) {
		if merged, g := buildMergedGraph(in); g != nil {
			v.checkMergedGraph(in, merged, g, deltaIDs)
			v.checkExpansionRefs(in, g, deltaIDs)
		}
	}

	return ExpansionVerdict{Issues: v.issues, Breaches: breaches}
}

// checkExpansionEdgeFields applies the loop/normal edge-field rules to a plan
// edge, mirroring checkEdges so an injected loop or conditioned edge is held
// to the same contract as an authored one.
func (v *validator) checkExpansionEdgeFields(path string, e Edge) {
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
		// An empty on_exhausted is the default (proceed); a decoded plan edge is
		// normalized by decodeEdge, but an in-memory delta may leave it blank.
		switch e.OnExhausted {
		case "", ExhaustProceed, ExhaustFail:
		default:
			v.add(CodeConfigFieldInvalid, path+".on_exhausted", "must be %q or %q, got %q", ExhaustProceed, ExhaustFail, string(e.OnExhausted))
		}
		// The no-progress guard's policy/path (ticket 14.4), mirroring checkEdges;
		// body-membership of Step on an injected loop degrades safely (the guard
		// simply never fires) so it is not re-checked against the merged graph.
		if np := e.NoProgress; np != nil {
			switch np.Policy {
			case "", ExhaustProceed, ExhaustFail:
			default:
				v.add(CodeConfigFieldInvalid, path+".no_progress.policy", "must be %q or %q, got %q", ExhaustProceed, ExhaustFail, string(np.Policy))
			}
			if np.Path != "" {
				if err := checkJSONPointer(np.Path); err != nil {
					v.add(CodeConfigFieldInvalid, path+".no_progress.path", "invalid JSON pointer: %v", err)
				}
			}
		}
		// A decision marker is normal-edge-only (ADR-017), mirroring checkEdges.
		if e.Decision != "" {
			v.add(CodeApprovalEdgeInvalid, path+".decision", "not valid on a loop edge")
		}
		return
	}
	if e.Condition != "" {
		v.add(CodeLoopFieldForbidden, path+".condition", "only valid on loop edges")
	}
	if e.MaxIterations != 0 {
		v.add(CodeLoopFieldForbidden, path+".max_iterations", "only valid on loop edges")
	}
	if e.OnExhausted != "" {
		v.add(CodeLoopFieldForbidden, path+".on_exhausted", "only valid on loop edges")
	}
	if e.NoProgress != nil {
		v.add(CodeLoopFieldForbidden, path+".no_progress", "only valid on loop edges")
	}
	// A decision marker's enum is a codec/syntactic check; that it leaves a
	// human_approval step degrades safely on an injected edge (a marker on a
	// non-approval source simply never matches at routing time), so it is not
	// re-checked against the merged graph — the checkExpansionRefs precedent.
	if e.Decision != "" && !slices.Contains(approvalDecisions, e.Decision) {
		v.add(CodeApprovalEdgeInvalid, path+".decision", "unknown decision %q", string(e.Decision))
	}
	switch {
	case len(e.When) > MaxExprLen:
		v.add(CodeLimitExceeded, path+".when", "expression is %d bytes (max %d)", len(e.When), MaxExprLen)
	case e.When != "":
		v.checkExpr(path+".when", e.When)
	}
}

// buildMergedGraph assembles the post-expansion graph (existing nodes/edges +
// delta) and compiles it, returning both the merged definition (for edge index
// lookups) and the graph (for ancestry). Returns (nil, nil) if compilation
// fails, which is unreachable when the caller has gated on unique ids and
// resolved endpoints. Existing-node order is sorted so cycle reporting is
// deterministic despite map iteration.
func buildMergedGraph(in ExpansionInput) (*Definition, *Graph) {
	merged := &Definition{}
	merged.Steps = make([]Step, 0, len(in.Existing)+len(in.Plan.Steps))
	for id, a := range in.Existing {
		merged.Steps = append(merged.Steps, Step{ID: id, Type: a.Type})
	}
	slices.SortFunc(merged.Steps, func(a, b Step) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	merged.Steps = append(merged.Steps, in.Plan.Steps...)
	merged.Edges = append(append([]Edge(nil), in.ExistingEdges...), in.Plan.Edges...)

	g, err := NewGraph(merged)
	if err != nil {
		return nil, nil
	}
	return merged, g
}

// checkMergedGraph reports normal-edge cycles and loop edges whose target is
// not a normal-edge ancestor of their source — the acyclic-instance-graph
// invariant every durability mechanism depends on (ADR-003/004). Cycle paths
// and loop-edge indices are reported against the plan document where they touch
// the delta.
func (v *validator) checkMergedGraph(in ExpansionInput, merged *Definition, g *Graph, deltaIDs map[string]int) {
	touchesDelta := func(e Edge) bool {
		_, from := deltaIDs[e.From]
		_, to := deltaIDs[e.To]
		return from || to
	}
	for _, c := range g.findCycles() {
		e := merged.Edges[c.edgeIdx]
		if !touchesDelta(e) {
			// A pre-existing cycle would already have been rejected at submit;
			// only cycles the delta introduces are the expansion's fault.
			continue
		}
		v.add(CodeCycle, "plan.edges",
			"the plan would create a cycle in the run graph: %s", c.pathString())
	}
	for i, e := range in.Plan.Edges {
		if !e.IsLoop() {
			continue
		}
		fromIdx, okF := g.index[e.From]
		toIdx, okT := g.index[e.To]
		if !okF || !okT {
			continue
		}
		if !g.reaches(toIdx, fromIdx) {
			v.add(CodeLoopEdgeNotAncestor, fmt.Sprintf("plan.edges[%d]", i),
				"loop edge target %q is not a normal-edge ancestor of source %q", e.To, e.From)
		}
	}
}

// checkExpansionRefs is the cross-graph ref lint ADR-015 defers from the pure
// 13.1 validator to the store's in-tx revalidation: an injected step's
// `${{ steps.x.output }}` template references and step_output context sources
// must resolve against the merged graph — either x is a normal-edge ancestor of
// the injected step (its output guaranteed recorded before the injected step
// readies, the authored-template rule) or x is an existing step that has
// already succeeded (its output is materialized and immutable). A
// `${{ run.params.<key> }}` reference must name a submitted run parameter.
// These depend on materialized run-row state (succeeded outputs, the run's
// params), which is why they live here and not in the pure shape validator.
// Reuses classifyUpstreamRef and the same codes as the authored-template lint
// (checkTemplates / checkContextGraph) so an injected step is held to exactly
// the same reference contract as an authored one.
func (v *validator) checkExpansionRefs(in ExpansionInput, g *Graph, deltaIDs map[string]int) {
	var params map[string]json.RawMessage
	paramsDecoded := false
	paramDeclared := func(key string) bool {
		if !paramsDecoded {
			paramsDecoded = true
			if len(in.RunParams) > 0 {
				_ = json.Unmarshal(in.RunParams, &params) // non-object params → no keys declared
			}
		}
		_, ok := params[key]
		return ok
	}
	// classify resolves a step reference from an injected step against the
	// merged graph plus the succeeded-anchor relaxation.
	classify := func(fromStepID, refStepID string, ancestors map[string]bool) refStatus {
		st := classifyUpstreamRef(g, fromStepID, refStepID, ancestors)
		if st == refNotAncestor {
			if a, ok := in.Existing[refStepID]; ok && a.Succeeded {
				return refOK // materialized, immutable output
			}
		}
		return st
	}

	for i, s := range in.Plan.Steps {
		ancestors, _ := g.Ancestors(s.ID) // one BFS per injected step, over the merged graph; s.ID is a known node
		base := fmt.Sprintf("plan.steps[%d].config", i)

		// Template references in the config's `${{ }}` expressions.
		if s.Config != nil {
			if raw, err := marshalNoEscape(s.Config); err == nil && HasTemplate(string(raw)) {
				if ct, perr := ParseConfigTemplates(raw); perr == nil {
					for _, r := range ct.Refs() {
						if r.Lenient {
							continue
						}
						path := tmplIssuePath(base, r.ConfigPath)
						switch {
						case r.StepID != "":
							switch classify(s.ID, r.StepID, ancestors) {
							case refUnknownStep:
								v.add(CodeTemplateRefUnknownStep, path, "reference %q names unknown step %q", r.Raw, r.StepID)
							case refSelf:
								v.add(CodeTemplateRefNotUpstream, path, "reference %q: a step cannot reference its own output", r.Raw)
							case refNotAncestor:
								v.add(CodeTemplateRefNotUpstream, path, "reference %q: step %q is neither upstream of %q nor already succeeded", r.Raw, r.StepID, s.ID)
							case refOK:
							}
						case r.ParamKey != "":
							if !paramDeclared(r.ParamKey) {
								v.add(CodeTemplateRefUnknownParam, path, "reference %q names undeclared run parameter %q", r.Raw, r.ParamKey)
							}
						}
					}
				}
				// A template parse error was already reported by the per-step
				// config validation above (checkStepConfig does not parse
				// templates, but the plan's shape validation reused from Validate
				// does not either — a malformed template surfaces at render, and
				// the pure validator deliberately does not re-lint it here).
			}
		}

		// step_output context sources (mirroring checkContextGraph).
		if s.Context != nil {
			for j, src := range s.Context.Sources {
				if src.Kind != SourceStepOutput || src.Step == "" {
					continue
				}
				path := fmt.Sprintf("plan.steps[%d].context.sources[%d].step", i, j)
				switch classify(s.ID, src.Step, ancestors) {
				case refUnknownStep:
					v.add(CodeContextFieldInvalid, path, "step_output source names unknown step %q", src.Step)
				case refSelf:
					v.add(CodeContextFieldInvalid, path, "step_output source: a step cannot reference its own output")
				case refNotAncestor:
					v.add(CodeContextFieldInvalid, path, "step_output source: step %q is neither upstream of %q nor already succeeded", src.Step, s.ID)
				case refOK:
				}
			}
		}
	}
}
