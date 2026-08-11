package engine

// The execute-and-complete pipeline (ticket 4.3). After the executor
// returns, one transaction settles everything the result implies: the
// claim-fenced terminal CAS, edge resolution (CEL verdicts precomputed —
// they are pure), skip propagation, readiness fan-out with outbox rows for
// newly-ready steps, and the run rollup attempt. The ACK (a nil Handle
// return) happens only after this transaction commits, closing ADR-005's
// "completion/failure transition committed → ACK" row; a redelivery after
// the commit bounces off the claim CAS as a duplicate of finished work.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Failpoint stages, in transaction order. Test-only: the completion
// transaction consults completeFailpoint after each phase so the
// single-transaction test can abort it anywhere and assert nothing leaked.
const (
	stageAfterStepTransition = "after_step_transition"
	stageAfterFanOut         = "after_fan_out"
	stageAfterOutbox         = "after_outbox"
)

// completeFailpoint mirrors store's instantiateFailpoint: nil in
// production, installed by tests via export_test.go. An armed test must not
// run in parallel with other completion transactions.
var completeFailpoint atomic.Pointer[func(stage string) error]

func failpoint(stage string) error {
	if fn := completeFailpoint.Load(); fn != nil {
		return (*fn)(stage)
	}
	return nil
}

// edgeVerdict is one normal out-edge's resolution decision: the edge at
// Ordinal fired (its target's fired_deps increments) or skipped.
type edgeVerdict struct {
	ordinal int32
	fired   bool
}

// planEdges computes the resolution verdicts for a succeeded step's normal
// out-edges — the pure half of the completion transaction. edges must be
// the step's outgoing run_edges rows in ordinal order (the order the branch
// first-match rule is defined over); loop edges are excluded from the plan
// (they never resolve in v1 — iteration accounting is expansion's, M14).
//
// Non-branch sources fire every edge whose `when` is absent or true — the
// all-matching rule. A branch source fires only the first edge whose `when`
// is true, in ordinal order; a `when`-less edge (validation guarantees at
// most one, trailing) is the default and fires only if nothing before it
// matched; everything else skips, including all edges when nothing matches.
//
// Any CEL failure — compile (corrupt stored expression; validation
// guaranteed compilability at submit), evaluation error, non-bool result —
// is returned as an error the caller must record as a step-level failure of
// the completing step (ADR-003: never coerced to false).
func planEdges(stepType string, edges []gen.RunEdge, output json.RawMessage, params map[string]any) ([]edgeVerdict, error) {
	var out any
	if len(output) > 0 {
		if err := json.Unmarshal(output, &out); err != nil {
			return nil, fmt.Errorf("engine: decoding step output for edge predicates: %w", err)
		}
	}
	isBranch := stepType == string(dag.StepBranch)
	verdicts := make([]edgeVerdict, 0, len(edges))
	matched := false
	for _, edge := range edges {
		if edge.EdgeType != store.EdgeTypeNormal {
			continue
		}
		fired := false
		switch {
		case edge.WhenExpr == nil:
			// Unconditioned: always fires — except the branch default,
			// which fires only when no earlier edge matched.
			fired = !isBranch || !matched
		case isBranch && matched:
			// First-match already selected an earlier edge; the predicate
			// is not evaluated (its errors could not matter).
			fired = false
		default:
			expr, err := dag.CompileExpr(*edge.WhenExpr)
			if err != nil {
				return nil, fmt.Errorf("engine: compiling `when` of edge %d: %w", edge.Ordinal, err)
			}
			fired, err = expr.Eval(out, params)
			if err != nil {
				return nil, fmt.Errorf("engine: edge %d: %w", edge.Ordinal, err)
			}
		}
		if fired && isBranch {
			matched = true
		}
		verdicts = append(verdicts, edgeVerdict{ordinal: edge.Ordinal, fired: fired})
	}
	return verdicts, nil
}

// allSkippedVerdicts is planEdges for a skipped source: every normal
// out-edge resolves skipped, no predicate evaluated (ADR-003 — out-edges
// of skipped steps skip unconditionally, propagating).
func allSkippedVerdicts(edges []gen.RunEdge) []edgeVerdict {
	verdicts := make([]edgeVerdict, 0, len(edges))
	for _, edge := range edges {
		if edge.EdgeType != store.EdgeTypeNormal {
			continue
		}
		verdicts = append(verdicts, edgeVerdict{ordinal: edge.Ordinal, fired: false})
	}
	return verdicts
}

// isJoinAny reports whether a target step readies on its first fired edge
// (a `join` step with mode `any`). A join with absent config or mode gets
// the stricter all-shaped rule, mirroring dag.ReadySteps. A config that no
// longer decodes is corrupt stored state — an error, not a default.
func isJoinAny(step gen.RunStep) (bool, error) {
	if step.StepType != string(dag.StepJoin) {
		return false, nil
	}
	cfg, err := dag.DecodeStepConfig(dag.StepType(step.StepType), step.Config)
	if err != nil {
		return false, fmt.Errorf("engine: decoding join config of step %q: %w", step.StepID, err)
	}
	jc, ok := cfg.(*dag.JoinConfig)
	if !ok || jc == nil {
		return false, nil
	}
	return jc.Mode == dag.JoinAny, nil
}

// terminalSource is one worklist item of the fan-out: a step that just
// reached a terminal state, with the verdicts for its normal out-edges.
type terminalSource struct {
	stepID   string
	verdicts []edgeVerdict
}

// fanOutResult is what the fan-out worklist produced, for outbox writes
// and logging.
type fanOutResult struct {
	readied []string
	skipped []string
}

// fanOut resolves edges from the seed source and chases the consequences
// to the fixed point: targets whose counters satisfy the ADR-004 readiness
// guard transition pending → ready; targets with every incoming edge
// resolved and none fired transition pending → skipped and their own
// out-edges join the worklist, all-skipped (skip propagation). Terminates
// because the instance graph is acyclic and each edge resolves at most
// once. Runs inside the caller's completion transaction.
func fanOut(ctx context.Context, q store.Querier, runID uuid.UUID, now time.Time, seed terminalSource) (fanOutResult, error) {
	var res fanOutResult
	work := []terminalSource{seed}
	for len(work) > 0 {
		src := work[0]
		work = work[1:]
		for _, v := range src.verdicts {
			resolved, err := store.ResolveEdge(ctx, q, store.ResolveEdgeArgs{
				RunID: runID, Ordinal: v.ordinal, Fired: v.fired, Now: now,
			})
			if err != nil {
				return fanOutResult{}, err
			}
			if !resolved.Resolved {
				// Idempotent re-resolution: counters already applied.
				continue
			}
			target := resolved.ToStep
			if target.Status != store.StepStatusPending {
				// Join-any late-firing absorption: the target already
				// readied (or further) on an earlier fired edge. The edge
				// resolution above still recorded this verdict; there is
				// deliberately no second dispatch.
				continue
			}
			joinAny, err := isJoinAny(target)
			if err != nil {
				return fanOutResult{}, err
			}
			switch {
			case target.FiredDeps >= 1 && (target.RemainingDeps == 0 || joinAny):
				if _, err := store.ReadyStep(ctx, q, store.ReadyStepArgs{
					RunID: runID, StepID: target.StepID, JoinAny: joinAny, Now: now,
				}); err != nil {
					return fanOutResult{}, err
				}
				res.readied = append(res.readied, target.StepID)
			case target.RemainingDeps == 0 && target.FiredDeps == 0:
				if _, err := store.SkipStep(ctx, q, store.SkipStepArgs{
					RunID: runID, StepID: target.StepID, Now: now,
				}); err != nil {
					return fanOutResult{}, err
				}
				res.skipped = append(res.skipped, target.StepID)
				outEdges, err := q.Steps().ListEdgesFromStep(ctx, runID, target.StepID)
				if err != nil {
					return fanOutResult{}, err
				}
				work = append(work, terminalSource{stepID: target.StepID, verdicts: allSkippedVerdicts(outEdges)})
			}
		}
	}
	return res, nil
}

// completeSuccess runs the success completion: precompute the edge plan
// (pure), then one transaction — claim-fenced running → succeeded, edge
// resolution + readiness fan-out, outbox rows for newly-ready steps, run
// rollup attempt — then, post-commit, the dispatch nudge. A CEL failure in
// the plan reroutes to completeFailure per ADR-003 (a step-level failure,
// never a false predicate). The returned error follows the Handle/ACK
// contract: nil = committed, ACK; non-nil = nothing decided, redeliver.
func (e *Engine) completeSuccess(ctx context.Context, step gen.RunStep, out exec.Output) error {
	logger := log.From(ctx)

	// The pre-transaction reads: run params and the out-edge rows are
	// immutable for the life of the run in v1 (expansion is M13), so
	// reading them outside the transaction is safe and keeps it short.
	run, err := e.store.Runs().Get(ctx, step.RunID)
	if err != nil {
		return err
	}
	outEdges, err := e.store.Steps().ListEdgesFromStep(ctx, step.RunID, step.StepID)
	if err != nil {
		return err
	}
	var params map[string]any
	if len(run.Params) > 0 {
		if err := json.Unmarshal(run.Params, &params); err != nil {
			// Params were validated JSON at submission; failing to decode
			// now is corrupt stored state — deterministic, so a step
			// failure, not a redelivery loop.
			return e.completeFailure(ctx, step, fmt.Errorf("decoding run params: %w", err))
		}
	}
	verdicts, err := planEdges(step.StepType, outEdges, out.Data, params)
	if err != nil {
		// Deterministic content failure (ADR-003: evaluation errors are
		// recorded as a step-level failure of the completing step).
		logger.WarnContext(ctx, "edge predicate evaluation failed; recording step failure",
			slog.Any("error", err))
		return e.completeFailure(ctx, step, err)
	}

	now := e.now()
	var fanned fanOutResult
	var fenced *store.TransitionError
	runDone := false
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if _, err := store.SucceedStep(ctx, q, store.SucceedStepArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
			Output: out.Data, Now: now,
		}); err != nil {
			// A typed conflict on the terminal CAS is the fence firing:
			// this worker's claim is no longer current (zombie write,
			// ADR-005). Recorded for the post-tx abandon; the error return
			// still rolls the transaction back.
			errors.As(err, &fenced)
			return err
		}
		if err := failpoint(stageAfterStepTransition); err != nil {
			return err
		}
		var ferr error
		fanned, ferr = fanOut(ctx, q, step.RunID, now, terminalSource{stepID: step.StepID, verdicts: verdicts})
		if ferr != nil {
			return ferr
		}
		if err := failpoint(stageAfterFanOut); err != nil {
			return err
		}
		for _, id := range fanned.readied {
			if _, err := q.Outbox().Create(ctx, step.RunID, id, store.OutboxReasonStepReady); err != nil {
				return err
			}
		}
		if err := failpoint(stageAfterOutbox); err != nil {
			return err
		}
		var rerr error
		runDone, rerr = attemptRunRollup(ctx, q, step.RunID, now)
		return rerr
	})
	if txErr != nil {
		if fenced != nil {
			return e.abandonFenced(ctx, step, fenced, txErr)
		}
		// A dag decode error surfacing from the transaction is isJoinAny
		// hitting a join target whose stored config no longer decodes —
		// deterministic corrupt content, like the pre-transaction decode
		// failures above. The transaction rolled back; record a real step
		// failure instead of redelivering into the same error forever
		// (post-M4 audit: since 4.5 a redelivery of a running step takes
		// over and re-executes, so a deterministic loop now re-runs the
		// executor once per delivery until the poison threshold).
		// ResolveEdge's graph-integrity errors deliberately stay on the
		// redeliver path: they mean the run's bookkeeping is corrupt, and
		// a FailStep over the same corrupt rows is not a safer outcome
		// than surfacing on the poison path.
		var de *dag.DecodeError
		if errors.As(txErr, &de) {
			logger.WarnContext(ctx, "corrupt step config discovered during fan-out; recording step failure",
				slog.Any("error", txErr))
			return e.completeFailure(ctx, step, txErr)
		}
		logger.ErrorContext(ctx, "completion transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return txErr
	}

	logger.InfoContext(ctx, "step succeeded",
		slog.Int("edges_resolved", len(verdicts)),
		slog.Int("steps_readied", len(fanned.readied)),
		slog.Int("steps_skipped", len(fanned.skipped)),
		slog.Bool("run_succeeded", runDone))
	if len(fanned.readied) > 0 && e.nudge != nil {
		e.nudge()
	}
	return nil
}

// attemptRunRollup tries running → succeeded and drops the conflict when
// the guard does not (yet) hold — the completion transaction simply
// attempts it after its step lands; the guard passes exactly once, on the
// transaction that terminalizes the last step of a fully-successful run.
func attemptRunRollup(ctx context.Context, q store.Querier, runID uuid.UUID, now time.Time) (bool, error) {
	_, err := store.SucceedRun(ctx, q, store.SucceedRunArgs{RunID: runID, Now: now})
	if err == nil {
		return true, nil
	}
	var te *store.TransitionError
	if errors.As(err, &te) {
		return false, nil
	}
	return false, err
}

// completeFailure runs the failure completion: one transaction with the
// claim-fenced running → failed transition (error recorded on step and
// attempt) and the v1-minimum run rollup running → failed (ADR-006 owns
// *when* a step failure halts a run; until then a failed step fails its
// run). The failed step's out-edges stay unresolved (ADR-004), so
// dependents block — retry, DLQ, and failure policy are M5.
func (e *Engine) completeFailure(ctx context.Context, step gen.RunStep, execErr error) error {
	logger := log.From(ctx)
	payload, err := json.Marshal(map[string]string{"message": execErr.Error()})
	if err != nil {
		payload = json.RawMessage(`{"message": "unencodable executor error"}`)
	}

	now := e.now()
	runFailed := false
	var fenced *store.TransitionError
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if _, err := store.FailStep(ctx, q, store.FailStepArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
			Error: payload, Now: now,
		}); err != nil {
			// The fence firing on the failure path — see completeSuccess.
			errors.As(err, &fenced)
			return err
		}
		if err := failpoint(stageAfterStepTransition); err != nil {
			return err
		}
		_, err := store.FailRun(ctx, q, store.FailRunArgs{RunID: step.RunID, Now: now})
		if err == nil {
			runFailed = true
			return nil
		}
		// Wrong status (a parallel branch already failed the run) is the
		// only conflict the guard can produce here; drop it.
		var te *store.TransitionError
		if errors.As(err, &te) {
			return nil
		}
		return err
	})
	if txErr != nil {
		if fenced != nil {
			return e.abandonFenced(ctx, step, fenced, txErr)
		}
		logger.ErrorContext(ctx, "failure-completion transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return txErr
	}

	logger.WarnContext(ctx, "step failed",
		slog.Any("error", execErr),
		slog.Bool("run_failed", runFailed))
	return nil
}

// errFencedCompletion marks a completion abandoned because its terminal CAS
// was rejected — this worker's claim is no longer current. Returned (never
// nil) so the consumer does not ACK: for a reclaimed entry the ACK would
// delete the new holder's lease mid-execution. The abandoned entry heals on
// its own — the new holder ACKs it after completing, or (a false-positive
// takeover of a live worker) this worker's own entry goes stale, redelivers,
// and bounces off the claim CAS.
var errFencedCompletion = errors.New("engine: completion fenced by a lost claim — abandoned")

// abandonFenced is the zombie-write rejection (ticket 4.5, ADR-005's
// "log both claim IDs, abandon, no ACK"): one distinct error log carrying
// the claim this worker presented and the one currently on the row, then
// the typed abandon error. Reached from any terminal-CAS conflict —
// claim_mismatch (the step was taken over and re-claimed), or wrong_status
// from a terminal state (the new holder already completed) or from ready
// (taken over, not yet re-claimed).
func (e *Engine) abandonFenced(ctx context.Context, step gen.RunStep, te *store.TransitionError, cause error) error {
	log.From(ctx).ErrorContext(ctx, "completion fenced: claim no longer current — abandoning without ACK",
		slog.String("claim_id_caller", claimIDString(step)),
		slog.String("claim_id_current", claimIDRefString(te.CurrentClaimID)),
		slog.String("step_status", te.From),
		slog.String("conflict_reason", string(te.Reason)),
		slog.Any("error", cause))
	return fmt.Errorf("%w: %w", errFencedCompletion, cause)
}
