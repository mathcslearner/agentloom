package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
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
	// loopNoProgress: condition true and iteration < max, but the loop's opt-in
	// no-progress guard detected two consecutive iterations producing an
	// identical output hash (ticket 14.4). A loop_no_progress event is recorded;
	// the guard's policy then proceeds (ordinary fan-out) or fails the run —
	// exactly like loopExhaust, one termination class earlier.
	loopNoProgress
)

// loopOutcome is a loop source's completion decision.
type loopOutcome struct {
	kind loopKind
	// plan is the next-iteration delta, set only for loopContinue.
	plan *dag.PlanOutput
	// event is the loop_exhausted payload, set only for loopExhaust.
	event store.LoopExhaustedEvent
	// noProgress is the loop_no_progress payload, set only for loopNoProgress.
	noProgress store.LoopNoProgressEvent
	// fail is true for loopExhaust/loopNoProgress under a fail policy.
	fail bool
}

// loopDecision decides what a loop source's completion does (ticket 14.3). It
// is pure given the run definition snapshot and the step output: it finds the
// completing step's authored loop edge, evaluates its condition against the
// output, and compares the completing instance's iteration to max_iterations.
// A non-loop-source step returns loopNone. A malformed condition (a CEL error,
// as in planEdges) is a deterministic step failure the caller routes permanent.
func (e *Engine) loopDecision(ctx context.Context, step gen.RunStep, run gen.Run, out exec.Output, params map[string]any) (loopOutcome, error) {
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

	// No-progress detection (ticket 14.4): before minting the next iteration,
	// the loop's opt-in guard hashes the compared step's output at iteration k
	// and k-1; two identical hashes mean the loop is spinning without making
	// forward progress, so it terminates early. Only from the second iteration
	// on (k >= 1: there is a prior iteration to compare against). The guard is
	// purely additive — a missing pointer or an unreadable prior instance skips
	// the check (the loop simply continues), never a new failure.
	if loopEdge.NoProgress != nil && iteration >= 1 {
		if np := e.checkNoProgress(ctx, step, loopEdge, out, iteration); np != nil {
			fail := loopEdge.NoProgress.Policy == dag.ExhaustFail
			return loopOutcome{kind: loopNoProgress, fail: fail, noProgress: *np}, nil
		}
	}

	plan, err := dag.GenerateLoopExpansion(def, loopEdge, step.StepID, iteration+1)
	if err != nil {
		return loopOutcome{}, fmt.Errorf("generating loop iteration %d for %q: %w", iteration+1, authored, err)
	}
	return loopOutcome{kind: loopContinue, plan: &plan}, nil
}

// checkNoProgress evaluates a loop's no-progress guard for the completing
// iteration (ticket 14.4). It hashes the compared step's output at iteration k
// (the current completion, when the compared step is the loop source itself, or
// a stored instance) and k-1 (a stored instance); when the two hashes match it
// returns the loop_no_progress event, else nil. It is deliberately additive: any
// obstacle to comparing — an unresolvable pointer, an unreadable/absent prior
// instance, a store error — skips the check (returns nil) with a log line rather
// than failing the run, since no-progress is an opt-in early exit, never a new
// failure mode. The loop is bounded by max_iterations regardless.
func (e *Engine) checkNoProgress(ctx context.Context, step gen.RunStep, loopEdge dag.Edge, out exec.Output, iteration int) *store.LoopNoProgressEvent {
	logger := log.From(ctx)
	guardStep := loopEdge.NoProgress.Step
	if guardStep == "" {
		guardStep = loopEdge.From
	}
	path := loopEdge.NoProgress.Path

	curInstance := loopInstanceID(guardStep, iteration)
	prevInstance := loopInstanceID(guardStep, iteration-1)

	// The current side is the completing output directly when the compared step
	// is the loop source (the completing instance); otherwise it is a stored
	// body-member instance of this iteration (which completed upstream of the
	// loop source and so is already durable).
	var curData []byte
	need := []string{prevInstance}
	if guardStep == loopEdge.From {
		curInstance = step.StepID
		curData = out.Data
	} else {
		need = append(need, curInstance)
	}
	rows, err := e.store.Steps().ListByIDs(ctx, step.RunID, need)
	if err != nil {
		logger.WarnContext(ctx, "no-progress guard: reading compared instances failed; skipping the check",
			slog.String("compared_step", guardStep), slog.Any("error", err))
		return nil
	}
	outputs := make(map[string][]byte, len(rows))
	for _, r := range rows {
		outputs[r.StepID] = r.Output
	}
	if curData == nil {
		d, ok := outputs[curInstance]
		if !ok {
			return nil // the current instance is not yet durable — skip
		}
		curData = d
	}
	prevData, ok := outputs[prevInstance]
	if !ok {
		return nil // no prior iteration to compare against — skip
	}

	curHash, cerr := outputHash(curData, path)
	prevHash, perr := outputHash(prevData, path)
	if cerr != nil || perr != nil {
		logger.WarnContext(ctx, "no-progress guard: output pointer did not resolve; skipping the check",
			slog.String("compared_step", guardStep), slog.String("path", path))
		return nil
	}
	if curHash != prevHash {
		return nil // progress: the outputs differ
	}

	policy := loopEdge.NoProgress.Policy
	if policy == "" {
		policy = dag.ExhaustProceed
	}
	action := "proceed"
	if policy == dag.ExhaustFail {
		action = "fail"
	}
	return &store.LoopNoProgressEvent{
		LoopSourceStep:     authoredStepID(step.StepID),
		LoopSourceInstance: step.StepID,
		ComparedStep:       guardStep,
		Path:               path,
		Iteration:          iteration,
		PrevInstance:       prevInstance,
		CurInstance:        curInstance,
		Hash:               curHash,
		Policy:             string(policy),
		Action:             action,
	}
}

// loopInstanceID maps an authored step id and a loop iteration to the instance
// id that iteration carries: iteration 0 is the authored id itself (the initial
// run body), and iteration k >= 1 is "<id>#k" (the unroller's minted instance).
func loopInstanceID(authored string, iter int) string {
	if iter <= 0 {
		return authored
	}
	return authored + "#" + strconv.Itoa(iter)
}

// outputHash resolves the RFC 6901 pointer into a step output (empty pointer =
// whole output) and returns the hex SHA-256 of its canonical JSON, so two
// logically-equal outputs hash identically regardless of object key order or
// whitespace (ticket 14.4). It reuses the blackboard pointer resolver, then
// canonicalizes value-wise by decoding to a Go value and re-encoding (Go sorts
// map keys), which the raw byte compaction does not do.
func outputHash(data []byte, pointer string) (string, error) {
	resolved, err := blackboard.ResolvePointer(data, pointer)
	if err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal(resolved, &v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
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

// recordLoopNoProgress appends the loop_no_progress event in its own short
// transaction (ticket 14.4). Used only by the no_progress: fail path, where the
// loop source is then dead-lettered through completeFailure (a separate
// transaction); the proceed path records the event inside the completion
// transaction instead — the same split as recordLoopExhausted.
func (e *Engine) recordLoopNoProgress(ctx context.Context, runID uuid.UUID, event store.LoopNoProgressEvent) error {
	now := e.now()
	return e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		return store.RecordLoopNoProgress(ctx, q, runID, event, now)
	})
}
