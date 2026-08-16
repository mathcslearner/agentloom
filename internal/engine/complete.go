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
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	obstrace "github.com/mathcslearner/agentloom/internal/obs/trace"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/tokens"
	"github.com/mathcslearner/agentloom/internal/validate"
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
// semanticAttempt is this attempt's 1-based semantic-retry number (ticket
// 11.6), recorded as the terminal depth of a validated step's semantic loop.
func (e *Engine) completeSuccess(ctx context.Context, step gen.RunStep, out exec.Output, verdict *validate.Verdict, semanticAttempt int, counter tokens.Counter) error {
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
			// now is corrupt stored state — deterministic, so a permanent
			// step failure (ADR-006 taxonomy row 7), not a redelivery loop.
			return e.completeFailure(ctx, step, exec.Output{}, fmt.Errorf("decoding run params: %w", err), dag.ClassPermanent, store.TraceFromRun(run))
		}
	}
	verdicts, err := planEdges(step.StepType, outEdges, out.Data, params)
	if err != nil {
		// Deterministic content failure (ADR-003: evaluation errors are
		// recorded as a step-level failure of the completing step;
		// ADR-006 row 6: force-classified permanent).
		logger.WarnContext(ctx, "edge predicate evaluation failed; recording step failure",
			slog.Any("error", err))
		return e.completeFailure(ctx, step, exec.Output{}, err, dag.ClassPermanent, store.TraceFromRun(run))
	}

	// Declarative blackboard writes (ticket 12.2, ADR-014): planned pre-
	// transaction (pure — resolve each write's From pointer into this step's
	// output), applied in-transaction after the success CAS. Gated on a wired
	// board so test layers that don't opt in see no blackboard behavior. A
	// pointer that does not resolve is a deterministic data error → permanent
	// step failure, carrying the output so its productive cost still ledgers.
	var bbWrites []plannedBBWrite
	if e.blackboard != nil {
		bbWrites, err = planBlackboardWrites(step, out.Data)
		if err != nil {
			logger.WarnContext(ctx, "declarative blackboard write planning failed; recording step failure",
				slog.Any("error", err))
			return e.completeFailure(ctx, step, out, err, dag.ClassPermanent, store.TraceFromRun(run))
		}
	}

	// The attempt's token usage (ticket 8.6), set only by metered
	// executors (the llm step); marshaled here so it lands on the attempt
	// row inside the completion transaction for M10's cost ledger. A
	// marshal failure on this fixed two-int struct is not worth failing a
	// succeeded step over — log it and store NULL.
	var usage json.RawMessage
	if out.Usage != nil {
		if u, merr := json.Marshal(out.Usage); merr != nil {
			logger.WarnContext(ctx, "marshaling attempt usage failed; recording no usage",
				slog.Any("error", merr))
		} else {
			usage = u
		}
	}

	// The passing validation verdict (ticket 11.1, ADR-013), when the step
	// carried a chain: persisted on the attempt row so the run-status API
	// surfaces it and 11.6's metrics can read it. Nil when the step had no
	// chain. A marshal failure of this fixed struct is not worth failing a
	// succeeded step over — log and store NULL (unlike the failing path,
	// where the verdict is the load-bearing evidence).
	var verdictJSON json.RawMessage
	if verdict != nil {
		if vb, merr := verdict.Marshal(); merr != nil {
			logger.WarnContext(ctx, "marshaling validation verdict failed; recording no verdict",
				slog.Any("error", merr))
		} else {
			verdictJSON = vb
		}
	}

	// The structured-output provenance (ticket 11.3), set only by an llm step
	// with an output_format; persisted on the attempt row so the run-status
	// API and 11.6's metrics can read how the output was shaped. A marshal
	// failure of this fixed struct is not worth failing a succeeded step over
	// — log and store NULL.
	repairJSON := marshalRepair(ctx, logger, out.Repair)

	now := e.now()
	// The attempt's cost row (ticket 10.2, ADR-012), priced before the
	// transaction — pure, no database reads. Nil when the attempt ledgers
	// nothing (pricing disabled, not cost-bearing, unpriced tool, no usage).
	// A cache hit prices its counterfactual "saved" figure here too, since it
	// completes through this same path with Usage.CacheHit set.
	costRow := e.priceAttempt(ctx, step, out, now)
	// Judge overhead rows (ticket 11.5, ADR-012 rule 4): each llm_judge in the
	// step's validation chain made a provider call that bills to the serving
	// step, flagged overhead. Priced pure before the transaction from the
	// verdict's per-validator usage; nil when the chain had no cost-bearing
	// validator (or pricing is off). Even a cache HIT re-judges, so its judge
	// spend is real (the productive row is $0 saved, but the overhead rows are
	// genuine spend).
	overheadRows := e.priceOverheads(ctx, step, verdict, now)
	var fanned fanOutResult
	var fenced *store.TransitionError
	var terminalRun *gen.Run
	cancelling := false
	txCtx, txSpan := e.tracer.Start(ctx, "step.completion")
	txErr := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		// The run-status check (ticket 5.6): serialized against CancelRun
		// by the run lock every transition takes first. On a cancelling
		// run the success is still honored — the work is done, and
		// discarding it would waste budget and re-run side effects — but
		// fan-out is skipped: the successors were already cancelled by the
		// request's sweep, and the run must quiesce, not advance.
		status, err := store.LockRunStatus(ctx, q, step.RunID, now)
		if err != nil {
			return err
		}
		cancelling = status == store.RunStatusCancelling
		if _, err := store.SucceedStep(ctx, q, store.SucceedStepArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
			Output: out.Data, Usage: usage, Verdict: verdictJSON, Repair: repairJSON, Now: now,
		}); err != nil {
			// A typed conflict on the terminal CAS is the fence firing:
			// this worker's claim is no longer current (zombie write,
			// ADR-005). Recorded for the post-tx abandon; the error return
			// still rolls the transaction back.
			errors.As(err, &fenced)
			return err
		}
		// The cost ledger row + run-aggregate bump (ticket 10.2), atomic with
		// the success CAS: it runs only after SucceedStep landed (a fenced
		// zombie completion returns above, so it never ledgers), and under the
		// run lock SucceedStep already holds, so the aggregate stays exactly
		// the sum of the rows even under concurrent completions.
		if costRow != nil {
			if _, err := store.ApplyAttemptCost(ctx, q, *costRow); err != nil {
				return err
			}
		}
		// The judge overhead rows, under the same run lock and atomic with the
		// success CAS (ticket 11.5): each is a distinct entry on this attempt.
		for _, oh := range overheadRows {
			if _, err := store.ApplyAttemptCost(ctx, q, oh); err != nil {
				return err
			}
		}
		// Declarative blackboard writes (ticket 12.2), atomic with the success
		// CAS and under the run lock it holds: each write appends a versioned,
		// token-counted, step-attributed entry. No fence — the success CAS
		// already fenced this completion.
		if len(bbWrites) > 0 {
			if err := e.applyBlackboardWrites(ctx, q, step, bbWrites, counter, now); err != nil {
				return err
			}
		}
		if err := failpoint(stageAfterStepTransition); err != nil {
			return err
		}
		if !cancelling {
			var ferr error
			fanned, ferr = fanOut(ctx, q, step.RunID, now, terminalSource{stepID: step.StepID, verdicts: verdicts})
			if ferr != nil {
				return ferr
			}
			if err := failpoint(stageAfterFanOut); err != nil {
				return err
			}
			// The outbox rows carry this completion span's context (ticket
			// 7.3): the dispatcher injects it into the envelope, so each
			// successor's attempt span parents under the span that made it
			// ready. Empty when tracing is off — NULL columns, run-row
			// fallback at drain.
			tp, ts := obstrace.Inject(ctx)
			enqTrace := store.TraceContext{Parent: tp, State: ts}
			for _, id := range fanned.readied {
				if _, err := q.Outbox().CreateTraced(ctx, step.RunID, id, store.OutboxReasonStepReady, enqTrace); err != nil {
					return err
				}
			}
			if err := failpoint(stageAfterOutbox); err != nil {
				return err
			}
		}
		var rerr error
		terminalRun, rerr = attemptRunRollup(ctx, q, step.RunID, now)
		return rerr
	})
	endTxSpan(txSpan, txErr)
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
		// a dead-letter completion over the same corrupt rows is not a
		// safer outcome than surfacing on the poison path.
		var de *dag.DecodeError
		if errors.As(txErr, &de) {
			logger.WarnContext(ctx, "corrupt step config discovered during fan-out; recording step failure",
				slog.Any("error", txErr))
			return e.completeFailure(ctx, step, exec.Output{}, txErr, dag.ClassPermanent, store.TraceFromRun(run))
		}
		logger.ErrorContext(ctx, "completion transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return txErr
	}

	// Cost metrics (ticket 10.5): recorded post-commit from the priced row, so
	// a fenced or rolled-back completion (which returned above) records nothing
	// — the metric counters track only charges that actually landed in the
	// ledger. A cache hit records saved (its cost is $0); a productive call
	// records spend and its token usage. Labeled by the resolved resource
	// (bounded to the catalog), never by run/step.
	if costRow != nil {
		e.recordCost(*costRow)
	}
	for _, oh := range overheadRows {
		e.recordCost(oh)
	}
	// Output-quality signals (ticket 11.6, ADR-013): the passing verdict, its
	// per-validator breakdown, any judge scores, the structured-output
	// provenance (excluding cache hits, whose provenance was counted at the
	// miss), and the terminal semantic-retry depth. Recorded post-commit like
	// the cost metrics, so a fenced completion (which returned above) records
	// nothing. A step with no chain (verdict nil) records only its repair
	// provenance, if any.
	e.recordVerdictQuality(step, out, verdict)
	e.recordRepairQuality(out)
	if verdict != nil {
		e.metrics.SemanticRetryDepth(store.StepStatusSucceeded, semanticAttempt)
	}
	logger.InfoContext(ctx, "step succeeded",
		slog.Int("edges_resolved", len(verdicts)),
		slog.Int("steps_readied", len(fanned.readied)),
		slog.Int("steps_skipped", len(fanned.skipped)),
		slog.Bool("run_cancelling", cancelling),
		slog.Bool("run_terminal", terminalRun != nil))
	if terminalRun != nil {
		e.recordRunCompleted(terminalRun.Status, terminalRun.StartedAt, now)
	}
	if len(fanned.readied) > 0 && e.nudge != nil {
		e.nudge()
	}
	return nil
}

// attemptRunRollup tries the terminal rollups and drops the conflicts
// when no guard (yet) holds — the completion transaction simply attempts
// them after its step lands. The three guards are disjoint by run status
// and pass at most once each: SucceedRun on the transaction that
// terminalizes the last step of a fully-successful run; FailRunRollup
// (ticket 5.4) on the one that terminalizes the last step of a
// continue_independent_branches run carrying a dead-lettered step —
// partial success is still a failed run, with the per-step statuses
// carrying the nuance (ADR-006); CancelRunRollup (ticket 5.6) on the one
// that settles the last in-flight step of a cancelling run.
//
// Returns the terminal run row when a rollup landed (its Status and
// CreatedAt feed the 7.2 run-completion metric), nil when no guard held.
func attemptRunRollup(ctx context.Context, q store.Querier, runID uuid.UUID, now time.Time) (*gen.Run, error) {
	run, err := store.SucceedRun(ctx, q, store.SucceedRunArgs{RunID: runID, Now: now})
	if err == nil {
		return &run, nil
	}
	var te *store.TransitionError
	if !errors.As(err, &te) {
		return nil, err
	}
	run, err = store.FailRunRollup(ctx, q, store.FailRunArgs{RunID: runID, Now: now})
	if err == nil {
		return &run, nil
	}
	if !errors.As(err, &te) {
		return nil, err
	}
	return attemptCancelRollup(ctx, q, runID, now)
}

// completeFailure is the failure router (tickets 5.2 + 5.4, ADR-006): one
// transaction that records the classed attempt failure and routes the
// step by the materialized policy — running → retrying (the budget admits
// another attempt of a retryable class, next_attempt_at stamped) or
// running → dead_lettered with its dead_letters record plus the run
// disposition per the workflow failure policy (fail_fast fails the run;
// continue_independent_branches writes off the newly-impossible
// descendants and attempts the all-terminal rollup). On the retry route
// the delayed re-dispatch is scheduled post-commit, best-effort: the
// envelope (reason retry, no EnqueuedAt so the delayed member dedups)
// fires at next_attempt_at, and a crash or Schedule failure after the
// commit is healed by the reconciler's overdue-retrying scan — either way
// the entry is consumed (nil return, ACK), because the durable row now
// carries the retry.
//
// runTrace is the run's durable root trace context (ticket 7.3), observed
// by the claim (or the success path's run read): the retry envelope's
// parent, because the delayed re-dispatch descends from no live span —
// the link back to this failed attempt is restored from
// run_steps.trace_span at the next claim instead.
func (e *Engine) completeFailure(ctx context.Context, step gen.RunStep, out exec.Output, execErr error, class dag.ErrorClass, runTrace store.TraceContext) error {
	logger := log.From(ctx)
	if declared, ok := misdeclaredClass(execErr); ok {
		logger.ErrorContext(ctx, "executor declared a class it may not use; defaulting to transient",
			slog.String("declared_class", string(declared)),
			slog.Any("error", execErr))
	}
	payload, err := json.Marshal(map[string]string{"message": execErr.Error(), "class": string(class)})
	if err != nil {
		payload = json.RawMessage(`{"message": "unencodable executor error", "class": "` + string(class) + `"}`)
	}

	// Metering the productive spend on a validation-stage transport failure
	// (ticket 11.5 gap fix): almost every failure path is a mechanical failure
	// whose provider call never billed (out.Usage nil ⇒ everything below is a
	// no-op). The exception is a step whose executor SUCCEEDED and billed but
	// whose output-validation chain then errored (an llm_judge under
	// `on_error: fail`): the money was spent, so the failing attempt records
	// its usage and ledgers a cost row — even though the step retries or
	// dead-letters. The judge's own call, when it billed before erroring (a
	// malformed answer), is metered as overhead off the *validate.Error.
	now := e.now()
	var usage json.RawMessage
	if out.Usage != nil {
		if u, merr := json.Marshal(out.Usage); merr != nil {
			logger.WarnContext(ctx, "marshaling failed-attempt usage failed; recording no usage",
				slog.Any("error", merr))
		} else {
			usage = u
		}
	}
	costRow := e.priceAttempt(ctx, step, out, now)
	var overheadRows []store.AttemptCostArgs
	if judgeUsage := validatorErrorUsage(execErr); judgeUsage != nil {
		if row := e.priceOverhead(ctx, step, store.JudgeErrorEntry, *judgeUsage, now); row != nil {
			overheadRows = append(overheadRows, *row)
		}
	}

	policy, perr := decodeRetryPolicy(step.RetryPolicy)
	if perr != nil {
		// Corrupt materialized policy — deterministic stored-state failure
		// (taxonomy row 7 in spirit): the retry decision cannot be made, so
		// the failure lands terminal with its judged class intact.
		logger.ErrorContext(ctx, "corrupt materialized retry policy; failing step without retry",
			slog.Any("error", perr))
	}

	runFailed := false
	cancelSettled := false
	var terminalRun *gen.Run
	var dlqSource string
	var run gen.Run
	var cancelledSteps []string
	var fireAt time.Time
	scheduled := false
	var fenced *store.TransitionError
	txCtx, txSpan := e.tracer.Start(ctx, "step.completion")
	txErr := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		// The run-status check (ticket 5.6): on a cancelling run the
		// failure is not judged at all — no retry, no DLQ (ADR-006 row 8);
		// the step settles as cancelled with the executor's error preserved
		// on the attempt, and the transaction attempts the finalization.
		status, serr := store.LockRunStatus(ctx, q, step.RunID, now)
		if serr != nil {
			return serr
		}
		if status == store.RunStatusCancelling {
			if _, cerr := store.CancelRunningStep(ctx, q, store.CancelRunningStepArgs{
				RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
				Reason: store.CancelReasonRunCancelled, Error: payload, Now: now,
			}); cerr != nil {
				// The fence firing on the cancel path — see completeSuccess.
				errors.As(cerr, &fenced)
				return cerr
			}
			cancelSettled = true
			var rerr error
			terminalRun, rerr = attemptCancelRollup(ctx, q, step.RunID, now)
			return rerr
		}
		retry := perr == nil && policy.Retries(class)
		exhausted := false
		if retry {
			prior, err := q.Attempts().CountCountedFailures(ctx, step.RunID, step.StepID)
			if err != nil {
				return err
			}
			// This failure is counted failure n; lost closures never count
			// (ADR-006 — the budget is judged attempts only), and after a
			// requeue the count starts at the dead_letters baseline.
			n := int(prior) + 1
			if n >= policy.MaxAttempts {
				retry = false // budget exhausted: this was the last attempt
				exhausted = true
			} else {
				delay, derr := retryDelay(policy, n, e.jitterRand)
				if derr != nil {
					// Corrupt materialized durations; terminal, like perr.
					logger.ErrorContext(ctx, "corrupt materialized backoff; failing step without retry",
						slog.Any("error", derr))
					retry = false
				} else {
					fireAt = now.Add(delay)
				}
			}
		}
		if retry {
			if _, err := store.RetryStep(ctx, q, store.RetryStepArgs{
				RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
				Outcome: string(class), Error: payload, Usage: usage, NextAttemptAt: fireAt, Now: now,
			}); err != nil {
				// The fence firing on the retry path — see completeSuccess.
				errors.As(err, &fenced)
				return err
			}
			// Meter the productive spend + judge overhead on a validation-stage
			// transport failure (ticket 11.5): nil rows on every mechanical
			// failure, so this is a no-op there.
			if err := applyCostRows(ctx, q, costRow, overheadRows); err != nil {
				return err
			}
			scheduled = true
			return nil
		}
		// The terminal path (ticket 5.4): dead-letter with the source the
		// disposition records — retries_exhausted iff a retryable class ran
		// out of budget; permanent otherwise (a never-retryable class, a
		// class outside retry_on, or a corrupt policy).
		dlqSource = store.DeadLetterSourcePermanent
		if exhausted {
			dlqSource = store.DeadLetterSourceRetriesExhausted
		}
		if _, err := store.DeadLetterStep(ctx, q, store.DeadLetterStepArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
			Source: dlqSource, Outcome: string(class), Error: payload, Usage: usage, Now: now,
		}); err != nil {
			// The fence firing on the failure path — see completeSuccess.
			errors.As(err, &fenced)
			return err
		}
		// Meter the productive spend + judge overhead on a terminal validation-
		// stage transport failure (ticket 11.5); no-op on mechanical failures.
		if err := applyCostRows(ctx, q, costRow, overheadRows); err != nil {
			return err
		}
		if err := failpoint(stageAfterStepTransition); err != nil {
			return err
		}
		// The failure policy is immutable run state; reading it under the
		// run lock DeadLetterStep took keeps the transaction's one read
		// consistent with its writes.
		var err error
		run, err = q.Runs().Get(ctx, step.RunID)
		if err != nil {
			return err
		}
		var derr error
		cancelledSteps, runFailed, derr = deadLetterDisposition(ctx, q, run.OnFailure, step.RunID, now)
		return derr
	})
	endTxSpan(txSpan, txErr)
	if txErr != nil {
		if fenced != nil {
			return e.abandonFenced(ctx, step, fenced, txErr)
		}
		logger.ErrorContext(ctx, "failure-completion transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return txErr
	}

	if cancelSettled {
		logger.InfoContext(ctx, "step cancelled: run is cancelling — failure not judged",
			slog.Any("error", execErr),
			slog.Bool("run_cancelled", terminalRun != nil))
		if terminalRun != nil {
			e.recordRunCompleted(terminalRun.Status, terminalRun.StartedAt, now)
		}
		return nil
	}
	if scheduled {
		e.metrics.RetryScheduled(string(class))
		e.recordCostRows(costRow, overheadRows)
		// Structured-output provenance (ticket 11.6): a validation-stage
		// transport error (a judge under on_error:fail) can carry a shaped
		// output from a successful executor call — count its repair provenance.
		// A mechanical executor failure carries an empty output (no-op).
		e.recordRepairQuality(out)
		e.scheduleRetry(ctx, step, fireAt, runTrace)
		logger.WarnContext(ctx, "step attempt failed; retry scheduled",
			slog.Any("error", execErr),
			slog.String("class", string(class)),
			slog.Time("next_attempt_at", fireAt))
		return nil
	}
	e.metrics.DeadLetter(dlqSource)
	e.recordCostRows(costRow, overheadRows)
	e.recordRepairQuality(out)
	if runFailed {
		e.recordRunCompleted(store.RunStatusFailed, run.StartedAt, now)
	}
	logger.WarnContext(ctx, "step dead-lettered",
		slog.Any("error", execErr),
		slog.String("class", string(class)),
		slog.Int("steps_cancelled", len(cancelledSteps)),
		slog.Bool("run_failed", runFailed))
	return nil
}

// scheduleRetry performs the post-commit half of the retry route: the
// delayed-ZSET write due at fireAt. Failures (and a nil scheduler) only
// log — the committed row already carries next_attempt_at, so the
// reconciler's overdue-retrying scan re-dispatches; nothing here may turn
// into a handler error, which would un-ACK an already-consumed delivery.
//
// The envelope's trace context is the run's durable root (ticket 7.3) —
// constant per run, so identical retries still encode to byte-identical
// delayed members and ZADD's move-the-fire-time dedup holds (ADR-005).
// The failed attempt is linked, not parented: the next claim restores the
// link from run_steps.trace_span.
func (e *Engine) scheduleRetry(ctx context.Context, step gen.RunStep, fireAt time.Time, runTrace store.TraceContext) {
	logger := log.From(ctx)
	if e.scheduler == nil {
		logger.WarnContext(ctx, "no retry scheduler configured; reconciler will re-dispatch the retry")
		return
	}
	env := queue.Envelope{
		RunID: step.RunID, StepID: step.StepID, Reason: queue.ReasonRetry,
		TraceParent: runTrace.Parent, TraceState: runTrace.State,
	}
	if err := e.scheduler.Schedule(ctx, env, fireAt); err != nil {
		logger.ErrorContext(ctx, "delayed retry schedule failed; reconciler will re-dispatch",
			slog.Time("fire_at", fireAt),
			slog.Any("error", err))
	}
}

// completeThrottle is the backpressure route (ticket 9.2, ADR-010): a
// fleet-wide rate-limit denial defers a step without failing it. It computes
// the requeue delay (clamp(retry_after) + additive jitter), routes the step
// running → retrying via ThrottleStep — the administrative `throttled`
// outcome, no steps_failed bump, no counted failure, so the retry budget is
// untouched — and schedules the delayed re-dispatch (reason throttle)
// post-commit. The worker slot is released the moment this returns nil:
// nothing waits in-process for tokens. On a cancelling run the throttle is
// abandoned and the step settles as cancelled instead (the run must quiesce,
// not wait for capacity). The return follows the Handle/ACK contract: nil =
// committed, ACK; non-nil = redeliver.
//
// runTrace is the run's durable root trace context (ticket 7.3): the delayed
// throttle envelope's parent, exactly as the retry route uses it — the
// re-dispatch descends from no live span, and the next claim restores the
// attempt link from run_steps.trace_span.
func (e *Engine) completeThrottle(ctx context.Context, step gen.RunStep, dec resource.Decision, runTrace store.TraceContext) error {
	logger := log.From(ctx)
	delay := throttleDelay(dec.RetryAfter, e.throttleFloor, e.throttleCap, e.throttleJitterFrac, e.jitterRand)
	now := e.now()
	fireAt := now.Add(delay)
	cancelSettled := false
	var terminalRun *gen.Run
	var fenced *store.TransitionError
	txCtx, txSpan := e.tracer.Start(ctx, "step.completion")
	txErr := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		// The run-status check (ticket 5.6), as on the success/failure paths:
		// a throttle on a cancelling run is dropped — the successors are
		// already cancelled and the run must converge, not wait for tokens.
		status, serr := store.LockRunStatus(ctx, q, step.RunID, now)
		if serr != nil {
			return serr
		}
		if status == store.RunStatusCancelling {
			if _, cerr := store.CancelRunningStep(ctx, q, store.CancelRunningStepArgs{
				RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
				Reason: store.CancelReasonRunCancelled, Error: nil, Now: now,
			}); cerr != nil {
				errors.As(cerr, &fenced)
				return cerr
			}
			cancelSettled = true
			var rerr error
			terminalRun, rerr = attemptCancelRollup(ctx, q, step.RunID, now)
			return rerr
		}
		if _, err := store.ThrottleStep(ctx, q, store.ThrottleStepArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
			Resource: dec.Resource, Bucket: dec.Bucket, RetryAfter: dec.RetryAfter.String(),
			NextAttemptAt: fireAt, Now: now,
		}); err != nil {
			// The fence firing on the throttle path — see completeSuccess.
			errors.As(err, &fenced)
			return err
		}
		return nil
	})
	endTxSpan(txSpan, txErr)
	if txErr != nil {
		if fenced != nil {
			return e.abandonFenced(ctx, step, fenced, txErr)
		}
		logger.ErrorContext(ctx, "throttle-completion transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return txErr
	}
	if cancelSettled {
		logger.InfoContext(ctx, "step throttled on a cancelling run — settled as cancelled",
			slog.String("resource", dec.Resource),
			slog.Bool("run_cancelled", terminalRun != nil))
		if terminalRun != nil {
			e.recordRunCompleted(terminalRun.Status, terminalRun.StartedAt, now)
		}
		return nil
	}
	e.metrics.Throttled(dec.Resource, dec.Bucket)
	e.metrics.ThrottleWait(dec.Resource, delay)
	e.scheduleThrottle(ctx, step, fireAt, runTrace)
	logger.InfoContext(ctx, "step throttled; re-dispatch scheduled",
		slog.String("resource", dec.Resource),
		slog.String("bucket", dec.Bucket),
		slog.Duration("retry_after", dec.RetryAfter),
		slog.Duration("delay", delay),
		slog.Time("next_attempt_at", fireAt))
	return nil
}

// scheduleThrottle performs the post-commit half of the throttle route: the
// delayed-ZSET write due at fireAt, under reason throttle. Failures (and a
// nil scheduler) only log — the committed row already carries
// next_attempt_at, so the reconciler's overdue-retrying scan re-dispatches;
// nothing here may turn into a handler error, which would un-ACK an
// already-consumed delivery. The envelope carries no EnqueuedAt, so a
// fan-out of siblings throttled against one resource dedups to one pending
// re-dispatch per step (ADR-005's byte-identical ZADD member).
func (e *Engine) scheduleThrottle(ctx context.Context, step gen.RunStep, fireAt time.Time, runTrace store.TraceContext) {
	logger := log.From(ctx)
	if e.scheduler == nil {
		logger.WarnContext(ctx, "no scheduler configured; reconciler will re-dispatch the throttled step")
		return
	}
	env := queue.Envelope{
		RunID: step.RunID, StepID: step.StepID, Reason: queue.ReasonThrottle,
		TraceParent: runTrace.Parent, TraceState: runTrace.State,
	}
	if err := e.scheduler.Schedule(ctx, env, fireAt); err != nil {
		logger.ErrorContext(ctx, "delayed throttle schedule failed; reconciler will re-dispatch",
			slog.Time("fire_at", fireAt),
			slog.Any("error", err))
	}
}

// endTxSpan closes a completion-transaction span with the transaction's
// outcome. A rolled-back transaction is an errored span; the engine-level
// routing of that error (redeliver, reroute, abandon) is the caller's.
func endTxSpan(span oteltrace.Span, txErr error) {
	if txErr != nil {
		span.RecordError(txErr)
		span.SetStatus(codes.Error, "transaction rolled back")
	}
	span.End()
}

// decodeRetryPolicy parses the effective policy materialized onto the
// run_steps row at instantiation. The column is NOT NULL and written from
// dag.ResolveRetryPolicy, so a decode failure means corrupt stored state.
func decodeRetryPolicy(raw json.RawMessage) (dag.ResolvedRetryPolicy, error) {
	var p dag.ResolvedRetryPolicy
	if len(raw) == 0 {
		return p, errors.New("engine: run step carries no materialized retry policy")
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("engine: decoding materialized retry policy: %w", err)
	}
	if p.MaxAttempts < 1 {
		return p, fmt.Errorf("engine: materialized retry policy has max_attempts %d", p.MaxAttempts)
	}
	return p, nil
}

// recordRunCompleted records one run-completion latency observation:
// terminal-transition time minus the run's started_at — both stamps come
// from the injected clock (runs.created_at is a DB default and would mix
// clocks). A run without started_at (impossible for instantiated runs,
// which start running) is skipped rather than recorded as garbage.
func (e *Engine) recordRunCompleted(status string, startedAt *time.Time, now time.Time) {
	if startedAt == nil {
		return
	}
	e.metrics.RunCompleted(status, now.Sub(*startedAt))
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
	e.metrics.FencingRejection()
	log.From(ctx).ErrorContext(ctx, "completion fenced: claim no longer current — abandoning without ACK",
		slog.String("claim_id_caller", claimIDString(step)),
		slog.String("claim_id_current", claimIDRefString(te.CurrentClaimID)),
		slog.String("step_status", te.From),
		slog.String("conflict_reason", string(te.Reason)),
		slog.Any("error", cause))
	return fmt.Errorf("%w: %w", errFencedCompletion, cause)
}
