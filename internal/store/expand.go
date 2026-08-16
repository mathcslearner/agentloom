package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// ExpansionRejectedError reports a PlanOutput that ValidateExpansion refused
// (ticket 13.2, ADR-015). It carries the verdict so the engine (13.3) can route
// the two rejection classes: CapExceeded → permanent (expansion_cap_exceeded,
// a better plan cannot help), otherwise validation_failed (semantic-retry the
// planner). Unwraps to the joined issues, so errors.As reaches each
// *dag.ValidationIssue. The store returns it from inside the caller's
// transaction; the caller's error return rolls the whole expansion back, so a
// rejected plan mutates nothing.
type ExpansionRejectedError struct {
	Verdict dag.ExpansionVerdict
}

func (e *ExpansionRejectedError) Error() string {
	return fmt.Sprintf("store: expansion rejected: %v", e.Verdict.Err())
}

// CapExceeded reports whether the rejection is a run-guard cap exhaustion — the
// signal to fail the planner permanently rather than semantic-retry it.
func (e *ExpansionRejectedError) CapExceeded() bool { return e.Verdict.CapExceeded() }

func (e *ExpansionRejectedError) Unwrap() error { return e.Verdict.Err() }

// ExpandRunArgs are the inputs to ExpandRun.
type ExpandRunArgs struct {
	RunID uuid.UUID
	// Origin identifies the step whose completion carries this expansion. Kind
	// and StepID are required; Depth is read from the origin's row, not trusted
	// from the caller.
	Origin dag.ExpansionOrigin
	// Plan is the validated delta of new steps and edges to splice in.
	Plan dag.PlanOutput
	// MaxAddedSteps is the origin planner's per-expansion cap override (its
	// config max_added_steps); 0 means no override — the run's resolved cap
	// applies. The effective per-expansion cap is min(run cap, this) when this
	// is positive.
	MaxAddedSteps int
	// Trace is the enqueuing span's context, stamped onto the outbox rows of
	// zero-indegree injected steps this expansion readies (ticket 7.3). Zero
	// means no context.
	Trace TraceContext
	// Now is the injected current time. Required.
	Now time.Time
}

// ExpandRunResult reports what an expansion committed.
type ExpandRunResult struct {
	// GraphVersion is the run's new graph version (= expansion count + 1),
	// stamped on every row this expansion introduced.
	GraphVersion int32
	// Readied are the zero-indegree injected step ids this expansion made ready
	// (and outboxed) directly — a "before"/independent splice. "After"-spliced
	// steps stay pending here and are readied by the origin's fan-out, which the
	// caller runs after ExpandRun (ADR-015 completion-transaction order).
	Readied []string
	// Widened are existing pending anchors whose remaining_deps this expansion
	// grew — the "before" splice targets (new → existing-pending).
	Widened []string
}

// GraphExpandedEvent is the graph_expanded event payload (ticket 13.2,
// ADR-015): the origin, the version transition, and the full delta, so the 13.6
// introspection API can reconstruct any version's added steps/edges from the
// event log. Money-free; the delta is the verbatim PlanOutput.
type GraphExpandedEvent struct {
	OriginStep  string `json:"origin_step"`
	OriginKind  string `json:"origin_kind"`
	FromVersion int32  `json:"from_version"`
	ToVersion   int32  `json:"to_version"`
	// Depth is the nesting depth of the steps this expansion injected.
	Depth int32 `json:"depth"`
	// Delta is the verbatim validated plan (new steps + edges).
	Delta dag.PlanOutput `json:"delta"`
	// Readied / Widened mirror ExpandRunResult for the audit feed.
	Readied []string `json:"readied,omitempty"`
	Widened []string `json:"widened,omitempty"`
}

// ExpandRun applies a validated PlanOutput to a running graph atomically inside
// the origin step's completion transaction (ticket 13.2, ADR-015): it reads the
// graph under the run lock, revalidates the plan against the live graph shape
// plus the run's resolved caps (dag.ValidateExpansion) and the deferred
// cross-graph ref lint, inserts the new run_steps/run_edges stamped with
// graph_version = v+1 and the origin provenance, recomputes readiness for the
// spliced frontier, widens the remaining_deps of "before"-spliced pending
// anchors, bumps graph_version and steps_total, appends the graph_expanded
// event, and readies+outboxes the zero-indegree injected steps.
//
// It must be called inside a WithTx callback (ErrNoTx otherwise), after the
// origin's claim-fenced SucceedStep so a zombie completion — which never lands
// that CAS — never expands, and before the origin's fan-out so an "after"-
// spliced step is readied and outboxed by that same fan-out (ADR-015). A
// rejected plan is returned as *ExpansionRejectedError with nothing written;
// concurrent expansions serialize on the run-row lock, so their versions are
// strictly ordered.
func ExpandRun(ctx context.Context, q Querier, args ExpandRunArgs) (ExpandRunResult, error) {
	const op = "expand run"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return ExpandRunResult{}, err
	}
	if args.Origin.StepID == "" {
		return ExpandRunResult{}, fmt.Errorf("store: %s: empty origin step id", op)
	}
	if args.Origin.Kind == "" {
		return ExpandRunResult{}, fmt.Errorf("store: %s: empty origin kind", op)
	}

	// Run-row lock first (uniform ordering) + the status guard: a run that is
	// not advancing must not grow. running|parked are the only states an
	// expansion may commit into — the same set the rollups fire from.
	locked, err := lockRun(ctx, gq, op, args.RunID)
	if err != nil {
		return ExpandRunResult{}, err
	}
	if locked.Status != RunStatusRunning && locked.Status != RunStatusParked {
		te := &TransitionError{
			Entity: "run", RunID: args.RunID, From: locked.Status, To: locked.Status,
			Reason: ConflictRunNotRunning,
		}
		return ExpandRunResult{}, fmt.Errorf("store: %s: %w", op, te)
	}

	run, err := gq.GetRun(ctx, args.RunID)
	if err != nil {
		return ExpandRunResult{}, wrapErr(op, err)
	}
	origin, err := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.Origin.StepID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ExpandRunResult{}, fmt.Errorf("store: %s: origin step %q: %w", op, args.Origin.StepID, ErrNotFound)
	}
	if err != nil {
		return ExpandRunResult{}, wrapErr(op, err)
	}

	steps, err := gq.ListRunSteps(ctx, args.RunID)
	if err != nil {
		return ExpandRunResult{}, wrapErr(op, err)
	}
	edges, err := gq.ListRunEdges(ctx, args.RunID)
	if err != nil {
		return ExpandRunResult{}, wrapErr(op, err)
	}

	caps, err := resolveExpansionCaps(run.ExpansionCaps)
	if err != nil {
		return ExpandRunResult{}, fmt.Errorf("store: %s: %w", op, err)
	}
	perExpansionCap := caps.MaxAddedStepsPerExpansion
	if args.MaxAddedSteps > 0 && args.MaxAddedSteps < perExpansionCap {
		perExpansionCap = args.MaxAddedSteps
	}

	in := dag.ExpansionInput{
		Plan:             args.Plan,
		Origin:           dag.ExpansionOrigin{Kind: args.Origin.Kind, StepID: args.Origin.StepID, Depth: int(origin.Depth)},
		Existing:         buildAnchors(steps, args.Origin.StepID),
		ExistingEdges:    toDagEdges(edges),
		RunParams:        run.Params,
		PerExpansionCap:  perExpansionCap,
		Caps:             caps,
		CurrentStepCount: len(steps),
		ExpansionsSoFar:  int(run.GraphVersion) - 1,
	}
	verdict := dag.ValidateExpansion(in)
	if !verdict.OK() {
		// The rejection rolls the whole transaction back — nothing below runs.
		return ExpandRunResult{}, &ExpansionRejectedError{Verdict: verdict}
	}

	newVersion := run.GraphVersion + 1
	injectedDepth := origin.Depth + 1
	originStep := args.Origin.StepID
	originKind := string(args.Origin.Kind)

	// Readiness of the spliced frontier: an injected step's remaining_deps is
	// its incoming normal-edge count from the plan (existing edges are frozen
	// and never target a delta step). A plan normal-edge to a still-pending
	// existing anchor is a "before" splice — the anchor gains one dependency.
	// Loop edges never count toward readiness (ADR-004).
	deltaIndegree := make(map[string]int, len(args.Plan.Steps))
	widenExisting := make(map[string]int)
	deltaSet := make(map[string]bool, len(args.Plan.Steps))
	for _, s := range args.Plan.Steps {
		deltaSet[s.ID] = true
	}
	for _, e := range args.Plan.Edges {
		if e.IsLoop() {
			continue
		}
		if deltaSet[e.To] {
			deltaIndegree[e.To]++
		} else {
			widenExisting[e.To]++
		}
	}

	stepRows := make([]gen.CreateRunStepsParams, len(args.Plan.Steps))
	var readied []string
	for i, step := range args.Plan.Steps {
		remaining := deltaIndegree[step.ID]
		status := StepStatusPending
		if remaining == 0 {
			// Zero-indegree injected step: created ready, exactly as an entry
			// step is at instantiation (ADR-004 side condition).
			status = StepStatusReady
			readied = append(readied, step.ID)
		}
		stepRows[i], err = stepRowParams(run.ID, step, stepPlacement{
			Status:        status,
			RemainingDeps: int32(remaining), //nolint:gosec // validation-bounded
			GraphVersion:  newVersion,
			Depth:         injectedDepth,
			OriginStep:    &originStep,
			OriginKind:    &originKind,
		}, args.Now)
		if err != nil {
			return ExpandRunResult{}, fmt.Errorf("store: %s: %w", op, err)
		}
	}
	if _, err := q.Steps().CreateBatch(ctx, stepRows); err != nil {
		return ExpandRunResult{}, wrapErr(op+": insert steps", err)
	}

	maxOrdinal, err := gq.MaxRunEdgeOrdinal(ctx, args.RunID)
	if err != nil {
		return ExpandRunResult{}, wrapErr(op, err)
	}
	edgeRows := make([]gen.CreateRunEdgesParams, len(args.Plan.Edges))
	for i, e := range args.Plan.Edges {
		edgeRows[i] = edgeRowParams(run.ID, e, maxOrdinal+1+int32(i), newVersion, &originStep, &originKind) //nolint:gosec // validation-bounded
	}
	if len(edgeRows) > 0 {
		if _, err := q.Steps().CreateEdgeBatch(ctx, edgeRows); err != nil {
			return ExpandRunResult{}, wrapErr(op+": insert edges", err)
		}
	}

	// Widen the "before"-spliced pending anchors. The anchor was validated
	// pending under this same lock, so a zero-row update is a graph-integrity
	// error, never a race.
	widened := make([]string, 0, len(widenExisting))
	for id := range widenExisting {
		widened = append(widened, id)
	}
	sort.Strings(widened)
	for _, id := range widened {
		if _, err := gq.AddRunStepRemainingDeps(ctx, gen.AddRunStepRemainingDepsParams{
			RunID: args.RunID, StepID: id, Delta: int32(widenExisting[id]), Now: args.Now, //nolint:gosec // validation-bounded
		}); errors.Is(err, pgx.ErrNoRows) {
			return ExpandRunResult{}, fmt.Errorf(
				"store: %s: widen anchor %q: not pending — graph bookkeeping corrupted (validated pending under this lock)", op, id)
		} else if err != nil {
			return ExpandRunResult{}, wrapErr(op+": widen anchor", err)
		}
	}

	// Commit the run-row side: graph_version++ and steps_total += injected.
	// The expected-version CAS can only fail under a lost run lock, which
	// cannot happen — so a zero-row result is an integrity error.
	bumped, err := gq.ExpandRunGraph(ctx, gen.ExpandRunGraphParams{
		RunID: args.RunID, Added: int32(len(args.Plan.Steps)), ExpectedVersion: run.GraphVersion, //nolint:gosec // validation-bounded
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ExpandRunResult{}, fmt.Errorf(
			"store: %s: graph_version moved from %d under the run lock — impossible without a lost lock", op, run.GraphVersion)
	}
	if err != nil {
		return ExpandRunResult{}, wrapErr(op+": bump graph version", err)
	}

	// graph_expanded, then a step_ready per zero-indegree injected step —
	// mirroring instantiation's run_created + step_ready ordering.
	if err := appendEvent(ctx, gq, op, args.RunID, EventGraphExpanded, GraphExpandedEvent{
		OriginStep:  originStep,
		OriginKind:  originKind,
		FromVersion: run.GraphVersion,
		ToVersion:   bumped.GraphVersion,
		Depth:       injectedDepth,
		Delta:       args.Plan,
		Readied:     readied,
		Widened:     widened,
	}); err != nil {
		return ExpandRunResult{}, err
	}
	for _, id := range readied {
		if err := appendEvent(ctx, gq, op, args.RunID, EventStepReady, stepIDPayload{StepID: id}); err != nil {
			return ExpandRunResult{}, err
		}
	}
	// Outbox the readied steps so a worker claims them; "after"-spliced steps
	// are outboxed by the origin's fan-out (the caller runs it after ExpandRun).
	for _, id := range readied {
		if _, err := q.Outbox().CreateTraced(ctx, args.RunID, id, OutboxReasonStepReady, args.Trace); err != nil {
			return ExpandRunResult{}, wrapErr(op+": outbox", err)
		}
	}

	log.From(ctx).InfoContext(ctx, "run graph expanded",
		log.RunID(args.RunID.String()), log.StepID(originStep),
		slog.String("origin_kind", originKind),
		slog.Int("graph_version", int(bumped.GraphVersion)),
		slog.Int("steps_added", len(args.Plan.Steps)),
		slog.Int("edges_added", len(args.Plan.Edges)),
		slog.Int("readied", len(readied)),
		slog.Int("widened", len(widened)))
	return ExpandRunResult{GraphVersion: bumped.GraphVersion, Readied: readied, Widened: widened}, nil
}

// resolveExpansionCaps decodes the run's materialized caps, falling back to the
// compiled defaults for a NULL column (a pre-13.2 row).
func resolveExpansionCaps(raw json.RawMessage) (dag.ExpansionCaps, error) {
	if len(raw) == 0 {
		return (*dag.ExpansionPolicy)(nil).Resolve(), nil
	}
	var caps dag.ExpansionCaps
	if err := json.Unmarshal(raw, &caps); err != nil {
		return dag.ExpansionCaps{}, fmt.Errorf("decoding expansion caps: %w", err)
	}
	return caps, nil
}

// buildAnchors maps the run's current steps onto the expansion-relevant anchor
// state ValidateExpansion needs. The origin is forced AnchorActive regardless
// of its row status: its completion transaction has run SucceedStep (so the row
// reads succeeded) but not yet its fan-out (which resolves its out-edges), so
// an "after" splice edge from it is still valid — the ADR-015 "origin is a
// valid from" rule. Succeeded (output materialized) is read from the true row
// status so the cross-graph ref lint sees the origin's output as available.
func buildAnchors(steps []gen.RunStep, originStepID string) map[string]dag.ExpansionAnchor {
	m := make(map[string]dag.ExpansionAnchor, len(steps))
	for _, s := range steps {
		status := anchorStatus(s.Status)
		if s.StepID == originStepID {
			status = dag.AnchorActive
		}
		m[s.StepID] = dag.ExpansionAnchor{
			Type:      dag.StepType(s.StepType),
			Status:    status,
			Succeeded: s.Status == StepStatusSucceeded,
		}
	}
	return m
}

// anchorStatus maps a run_steps status onto the three anchor classes ADR-015's
// splice rules reason about.
func anchorStatus(status string) dag.ExpansionAnchorStatus {
	switch status {
	case StepStatusPending:
		return dag.AnchorPending
	case StepStatusReady, StepStatusRunning, StepStatusRetrying:
		return dag.AnchorActive
	default: // succeeded, skipped, dead_lettered, cancelled, (retired) failed
		return dag.AnchorTerminal
	}
}

// toDagEdges maps the run's edge rows onto dag.Edge for the merged-graph
// acyclicity / loop-ancestry checks.
func toDagEdges(edges []gen.RunEdge) []dag.Edge {
	out := make([]dag.Edge, len(edges))
	for i, e := range edges {
		de := dag.Edge{From: e.FromStep, To: e.ToStep, Type: dag.EdgeType(e.EdgeType)}
		if e.WhenExpr != nil {
			de.When = *e.WhenExpr
		}
		if e.Condition != nil {
			de.Condition = *e.Condition
		}
		if e.MaxIterations != nil {
			de.MaxIterations = int(*e.MaxIterations)
		}
		out[i] = de
	}
	return out
}
