package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// This file is the loop-edge runtime (ticket 14.3, ADR-016): the completion-
// path logic that turns M1's marked loop edges into iterations by unrolling the
// loop body through store.ExpandRun. It composes the same primitive maps and
// planners use — only the trigger (a loop edge whose condition holds at
// completion, not a step type) and the delta (dag.GenerateLoopExpansion) differ.
//
// A loop is edge-driven: any completing step whose authored node (the id with
// its `#k` instance suffix stripped) has an outgoing marked loop edge is a loop
// source. The loop condition is evaluated against the completing step's output;
// the iteration is the completing instance's `#k` (0 for an authored step).

// loopKind classifies a loop source's completion.
type loopKind int

const (
	// loopNone: the completing step is not a loop source (the common case).
	loopNone loopKind = iota
	// loopContinue: condition true and iteration < max — expand the next
	// iteration (a loop delta rides the completion's ExpandRun).
	loopContinue
	// loopExit: condition false — stop iterating; the ordinary fan-out fires
	// the loop source's normal (non-loop) outgoing edges.
	loopExit
	// loopExhaust: condition true but iteration == max — the cap bit. A
	// loop_exhausted event is recorded; the on_exhausted policy then either
	// proceeds (ordinary fan-out) or fails the run.
	loopExhaust
)

// loopOutcome is a loop source's completion decision.
type loopOutcome struct {
	kind loopKind
	// plan is the next-iteration delta, set only for loopContinue.
	plan *dag.PlanOutput
	// event is the loop_exhausted payload, set only for loopExhaust.
	event store.LoopExhaustedEvent
	// fail is true for loopExhaust under on_exhausted: fail.
	fail bool
}

// loopDecision decides what a loop source's completion does (ticket 14.3). It
// is pure given the run definition snapshot and the step output: it finds the
// completing step's authored loop edge, evaluates its condition against the
// output, and compares the completing instance's iteration to max_iterations.
// A non-loop-source step returns loopNone. A malformed condition (a CEL error,
// as in planEdges) is a deterministic step failure the caller routes permanent.
func (e *Engine) loopDecision(step gen.RunStep, run gen.Run, out exec.Output, params map[string]any) (loopOutcome, error) {
	def, err := dag.Decode(run.Definition)
	if err != nil {
		return loopOutcome{}, fmt.Errorf("decoding run definition snapshot: %w", err)
	}
	authored := authoredStepID(step.StepID)
	loopEdge, ok := findLoopEdge(def, authored)
	if !ok {
		return loopOutcome{kind: loopNone}, nil
	}

	var output any
	if len(out.Data) > 0 {
		if err := json.Unmarshal(out.Data, &output); err != nil {
			return loopOutcome{}, fmt.Errorf("decoding loop source output for condition: %w", err)
		}
	}
	expr, err := dag.CompileExpr(loopEdge.Condition)
	if err != nil {
		return loopOutcome{}, fmt.Errorf("compiling loop condition of %q: %w", authored, err)
	}
	iterate, err := expr.Eval(output, params)
	if err != nil {
		return loopOutcome{}, fmt.Errorf("evaluating loop condition of %q: %w", authored, err)
	}
	if !iterate {
		return loopOutcome{kind: loopExit}, nil
	}

	iteration := loopIteration(step.StepID)
	if iteration >= loopEdge.MaxIterations {
		policy := loopEdge.OnExhausted
		if policy == "" {
			policy = dag.ExhaustProceed
		}
		action := "proceed"
		if policy == dag.ExhaustFail {
			action = "fail"
		}
		return loopOutcome{
			kind: loopExhaust,
			fail: policy == dag.ExhaustFail,
			event: store.LoopExhaustedEvent{
				LoopSourceStep:     authored,
				LoopSourceInstance: step.StepID,
				BodyEntry:          loopEdge.To,
				Iteration:          iteration,
				MaxIterations:      loopEdge.MaxIterations,
				Condition:          loopEdge.Condition,
				Policy:             string(policy),
				Action:             action,
			},
		}, nil
	}

	plan, err := dag.GenerateLoopExpansion(def, loopEdge, step.StepID, iteration+1)
	if err != nil {
		return loopOutcome{}, fmt.Errorf("generating loop iteration %d for %q: %w", iteration+1, authored, err)
	}
	return loopOutcome{kind: loopContinue, plan: &plan}, nil
}

// authoredStepID strips the `#k` instance suffix(es) from a step id, yielding
// the definition-authored node id (e.g. "critique#2" -> "critique"). Authored
// ids never contain `#` (validate.go stepIDRe), so the first `#` delimits the
// instance space.
func authoredStepID(stepID string) string {
	if i := strings.IndexByte(stepID, '#'); i >= 0 {
		return stepID[:i]
	}
	return stepID
}

// loopIteration is the iteration a step instance carries: the number in its
// last `#k` segment (0 for an authored step). It mirrors threadIteration (14.2)
// so a thread turn's iteration and a loop instance's iteration agree.
func loopIteration(stepID string) int { return threadIteration(stepID) }

// loopAuthoredDepth returns the depth of the loop's authored source, given the
// completing instance's row (ticket 14.3). Every loop iteration's body is
// injected at authored_depth+1, so a loop-injected completing instance's own
// depth is authored_depth+1 — subtract 1 to recover the authored depth; an
// authored (non-loop-injected) completing step is already at the authored depth.
func loopAuthoredDepth(step gen.RunStep) int {
	if step.OriginKind != nil && *step.OriginKind == string(dag.OriginLoop) {
		return int(step.Depth) - 1
	}
	return int(step.Depth)
}

// findLoopEdge returns the outgoing marked loop edge of the authored step, if
// any. A definition has at most one loop edge per source in practice; the first
// is returned deterministically (definition order).
func findLoopEdge(def *dag.Definition, authoredID string) (dag.Edge, bool) {
	for _, e := range def.Edges {
		if e.IsLoop() && e.From == authoredID {
			return e, true
		}
	}
	return dag.Edge{}, false
}

// recordLoopExhausted appends the loop_exhausted event in its own short
// transaction. Used only by the on_exhausted: fail path, where the loop source
// is then dead-lettered through completeFailure (a separate transaction); the
// proceed path records the event inside the completion transaction instead.
func (e *Engine) recordLoopExhausted(ctx context.Context, runID uuid.UUID, event store.LoopExhaustedEvent) error {
	now := e.now()
	return e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		return store.RecordLoopExhausted(ctx, q, runID, event, now)
	})
}
