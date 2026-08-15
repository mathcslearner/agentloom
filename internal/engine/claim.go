package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	obstrace "github.com/mathcslearner/agentloom/internal/obs/trace"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Handle is the worker's queue.Handler: attempt the claim CAS for the
// delivered step and act per ADR-005's ACK discipline. A nil return acks
// the entry; an error leaves it in the PEL to redeliver via reclaim.
//
// On a won claim the executor runs and its result feeds the completion
// pipeline (complete.go): one transaction persisting the outcome, fencing
// the terminal CAS on claim_id, resolving out-edges, and fanning readiness
// out to successors. The ACK happens only after that transaction commits —
// ADR-005's "completion/failure transition committed → ACK" row — so a
// crash anywhere before commit redelivers, and the redelivery bounces off
// the claim CAS as a duplicate of finished work when the commit did land.
//
// A reclaimed delivery of a running step — the holder went silent past the
// lease TTL — takes the third path: the lease-expiry takeover (ticket 4.5).
// The takeover CAS (running → ready, clearing the holder's claim_id — the
// moment the zombie loses its fence) and a fresh claim run in one
// transaction, then the step executes normally.
func (e *Engine) Handle(ctx context.Context, d queue.Delivery) error {
	// The consumer already stamped entry_id, delivery_count, run_id,
	// step_id, and reason into the log context — and started the attempt
	// span this delivery runs inside (ticket 7.3), which the claim below
	// records its own span context under (run_steps.trace_span).
	ctx = log.With(ctx, log.WorkerID(e.workerID))

	now := e.now()
	var step gen.RunStep
	var origin store.ClaimOrigin
	// run_steps.trace_span records the *attempt* span (the consumer's,
	// active on ctx here) — captured before the claim child span starts,
	// so later links point at the attempt, not its claim child.
	attemptSpan := obstrace.SpanContext(oteltrace.SpanContextFromContext(ctx))
	claimCtx, claimSpan := e.tracer.Start(ctx, "step.claim")
	err := e.store.WithTx(claimCtx, func(ctx context.Context, q store.Querier) error {
		var err error
		step, origin, err = store.ClaimStepWithOrigin(ctx, q, store.ClaimStepArgs{
			RunID:     d.Envelope.RunID,
			StepID:    d.Envelope.StepID,
			Now:       now,
			TraceSpan: attemptSpan,
		})
		return err
	})
	claimSpan.End()
	if err != nil {
		dec := classifyClaimFailure(err, d.DeliveryCount)
		e.metrics.ClaimDecision(dec.action.String())
		log.From(ctx).LogAttrs(ctx, dec.level, "claim rejected: "+dec.reason,
			slog.String("action", dec.action.String()),
			slog.Any("error", err))
		switch dec.action {
		case claimAckDrop:
			return nil
		case claimTakeover:
			return e.takeoverAndClaim(ctx, d, *dec.holderClaim)
		default:
			return err
		}
	}
	e.metrics.ClaimDecision(claimResultWon)
	if origin.Known && origin.FromStatus == store.StepStatusReady {
		// The scheduling-latency histogram (ticket 7.2): time from the step
		// turning ready to this claim winning. Retrying origins are skipped —
		// their interval includes the deliberate backoff, not scheduling.
		e.metrics.SchedulingLatency(now.Sub(origin.ChangedAt))
	}

	return e.execute(ctx, step, origin)
}

// claimResultWon is the successful-claim value of the metrics `result`
// label; the failure values are the claimAction strings.
const claimResultWon = "won"

// takeoverAndClaim is the lease-expiry takeover (ADR-005): one transaction
// composing TakeoverStep (running → ready, fenced on the observed holder's
// claim, closing its attempt as lost) with a fresh ClaimStep, then normal
// execution. One transaction removes the intermediate ready-with-no-entry
// state; both primitives take the run lock first, so the pair is race-free.
// If the reconciler's takeover won in between (step already ready), the
// takeover conflict is swallowed — a rejected transition writes nothing —
// and the claim proceeds on the entry already in hand.
func (e *Engine) takeoverAndClaim(ctx context.Context, d queue.Delivery, holderClaim uuid.UUID) error {
	now := e.now()
	var step gen.RunStep
	var origin store.ClaimOrigin
	// As in Handle: the recorded trace_span is the attempt span, not the
	// claim child span.
	attemptSpan := obstrace.SpanContext(oteltrace.SpanContextFromContext(ctx))
	claimCtx, claimSpan := e.tracer.Start(ctx, "step.claim")
	err := e.store.WithTx(claimCtx, func(ctx context.Context, q store.Querier) error {
		_, terr := store.TakeoverStep(ctx, q, store.TakeoverStepArgs{
			RunID: d.Envelope.RunID, StepID: d.Envelope.StepID,
			ClaimID: holderClaim, Now: now,
		})
		if terr != nil {
			var te *store.TransitionError
			already := errors.As(terr, &te) &&
				te.Reason == store.ConflictWrongStatus && te.From == store.StepStatusReady
			if !already {
				return terr
			}
			// Another takeover (the reconciler's) landed since this entry
			// was reclaimed; claim the ready step directly.
		}
		// The origin pre-read sees the displaced holder's trace_span (the
		// takeover does not clear it), so the new attempt span links to the
		// lost attempt exactly like a retry links to the failed one.
		var cerr error
		step, origin, cerr = store.ClaimStepWithOrigin(ctx, q, store.ClaimStepArgs{
			RunID: d.Envelope.RunID, StepID: d.Envelope.StepID, Now: now,
			TraceSpan: attemptSpan,
		})
		return cerr
	})
	claimSpan.End()
	if err != nil {
		dec := classifyTakeoverFailure(err)
		if dec.fenced {
			// The observed claim was stale — the fence protected a newer live
			// holder from this takeover (ticket 7.2's fencing counter).
			e.metrics.FencingRejection()
		}
		log.From(ctx).LogAttrs(ctx, dec.level, "takeover rejected: "+dec.reason,
			slog.String("action", dec.action.String()),
			slog.String("holder_claim_id", holderClaim.String()),
			slog.Any("error", err))
		if dec.action == claimAckDrop {
			return nil
		}
		return err
	}

	e.metrics.Takeover()
	log.From(ctx).InfoContext(ctx, "lease-expiry takeover: step reclaimed from silent holder",
		slog.String("displaced_claim_id", holderClaim.String()),
		slog.String("claim_id", claimIDString(step)))
	return e.execute(ctx, step, origin)
}

// execute runs the claimed step's executor and settles the result through
// the completion pipeline (complete.go). The returned error follows the
// Handle/ACK contract: nil means a completion transaction committed (ACK);
// non-nil means nothing was decided and the entry must redeliver.
//
// origin is the winning claim's pre-read observation: PrevTraceSpan links
// this attempt span to the attempt it re-executes (retry, takeover), and
// RunTrace rides to scheduleRetry as the re-dispatch envelope's parent.
func (e *Engine) execute(ctx context.Context, step gen.RunStep, origin store.ClaimOrigin) error {
	ctx = log.With(ctx, log.Attempt(int(step.AttemptCount)))
	logger := log.From(ctx)
	logger.InfoContext(ctx, "step claimed",
		slog.String("step_type", step.StepType),
		slog.String("claim_id", claimIDString(step)))
	e.annotateAttemptSpan(ctx, step, origin)

	executor, err := e.registry.Get(step.StepType)
	if err != nil {
		// Step types are validated against the catalog at submit time, so
		// a miss here means worker/definition version skew or corrupt
		// state — deterministic and permanent (ADR-006 taxonomy row 4), so
		// a recorded step failure, never a redelivery loop.
		logger.ErrorContext(ctx, "no executor registered for step type; recording step failure",
			slog.String("step_type", step.StepType),
			slog.Any("error", err))
		return e.completeFailure(ctx, step, exec.Output{}, err, dag.ClassPermanent, origin.RunTrace)
	}

	timeout, terr := stepTimeout(step.Timeout)
	if terr != nil {
		// Corrupt materialized timeout — deterministic stored-state failure
		// (ADR-006 taxonomy row 7 in spirit), like a corrupt retry policy.
		logger.ErrorContext(ctx, "corrupt materialized step timeout; recording step failure",
			slog.Any("error", terr))
		return e.completeFailure(ctx, step, exec.Output{}, terr, dag.ClassPermanent, origin.RunTrace)
	}
	// Template rendering (ticket 8.2): resolve the config's `${{ ... }}`
	// references against upstream outputs and run params. Deterministic
	// failures (missing reference, unparseable template) land a permanent
	// failure completion — the run's recorded state is immutable, so a
	// retry would fail identically; transport failures decide nothing and
	// redeliver.
	renderedConfig, rcErr := e.renderConfig(ctx, step)
	if rcErr != nil {
		var re *renderError
		if errors.As(rcErr, &re) {
			logger.ErrorContext(ctx, "step config rendering failed; recording step failure",
				slog.Any("error", rcErr))
			return e.completeFailure(ctx, step, exec.Output{}, rcErr, dag.ClassPermanent, origin.RunTrace)
		}
		return rcErr
	}
	// Output-validation chain resolution (ticket 11.1, ADR-013): resolve the
	// step's materialized validation chain against the validator registry
	// before the cache read and any spend. An unknown validator or a config
	// that does not match its schema (or a corrupt materialized policy) is a
	// deterministic permanent failure that must fire before money is spent —
	// the tool-args gate, lifted to the chain. A step with no chain resolves
	// to nil (zero validation work below).
	chain, chErr := e.resolveChain(step)
	if chErr != nil {
		logger.ErrorContext(ctx, "step validation chain unresolvable; recording step failure",
			slog.Any("error", chErr))
		return e.completeFailure(ctx, step, exec.Output{}, chErr, dag.ClassPermanent, origin.RunTrace)
	}
	// The claim always stamps a claim_id; the guard only keeps a corrupt
	// row from panicking the journal binding (misuse detection catches the
	// rest downstream).
	claimID := uuid.Nil
	if step.ClaimID != nil {
		claimID = *step.ClaimID
	}
	// Per-step log capture (ticket 7.4): the executor's logger — and only
	// the executor's — tees into the durable step_logs store, stamped with
	// the attempt span's trace id. The engine's own lines are pipeline
	// diagnostics, not step logs, and keep the plain logger.
	execLogger := logger
	if e.stepLogs != nil {
		traceID := ""
		if sc := oteltrace.SpanContextFromContext(ctx); sc.HasTraceID() {
			traceID = sc.TraceID().String()
		}
		execLogger = e.stepLogs.LoggerFor(logger, step.RunID, step.StepID, int(step.AttemptCount), traceID)
	}
	sc := exec.StepContext{
		StepType: dag.StepType(step.StepType),
		// The rendered config (ticket 8.2): templates resolved, ready for
		// the executor's strict decode. Input stays nil — rendering happens
		// in place inside the config, not as a separate payload.
		Config:  renderedConfig,
		Input:   nil,
		Attempt: int(step.AttemptCount),
		// Stable per (run, step) — the same key on every attempt, retry,
		// reclaim, and takeover of this step (ticket 5.5).
		IdempotencyKey: effects.Key(step.RunID, step.StepID),
		Effects:        e.effects.ForStep(step.RunID, step.StepID, int(step.AttemptCount), claimID, logger),
		Logger:         execLogger,
	}
	// Semantic-retry feedback injection (ticket 11.4, ADR-013): if this claim
	// carries a pending critique — a prior attempt's output failed its
	// validation chain and the step was semantic-retried — fold it into the
	// request now, before the cache read and any spend. The augmented request
	// re-keys the response cache (a semantic retry is cache-bypassed by
	// construction) and re-prices the estimate, exactly as a model downgrade
	// re-targets the pipeline. The feedback record's semantic-attempt number
	// threads into the validation chain (validate.Input.Attempt) below.
	augmentedConfig, feedback := e.injectFeedback(ctx, step, executor, sc.Config)
	sc.Config = augmentedConfig
	semanticAttempt := 1
	if feedback != nil && feedback.SemanticAttempt > 0 {
		semanticAttempt = feedback.SemanticAttempt
	}
	// Response cache read-through (ticket 9.5, ADR-011): ahead of the rate
	// limiter, consult the cache. A hit completes the step from the stored
	// result — no limiter acquire, no provider call — and returns handled.
	// A miss (or a declined/ineligible policy, or any fail-open) returns a
	// write binding threaded to the write-through after a successful
	// execution; steps whose type is not cacheable bypass this entirely.
	hit, cacheWB, cherr := e.cacheRead(ctx, step, executor, sc, chain, semanticAttempt)
	if hit {
		return cherr
	}
	// Claim-time budget enforcement (ticket 10.3, ADR-012): after the cache
	// read (a hit is $0, never budget-gated) and before the rate limiter
	// (parking after a limiter acquire would strand debited tokens), project
	// this step's pre-flight cost against the run budget and the step's caps.
	// A projection over budget parks the run (reason budget_exceeded,
	// resumable) or fails the step permanently, per policy; an oversized
	// request or an unpriceable model under fail-closed fails before the call.
	// Steps that estimate no cost bypass this entirely.
	//
	// Model downgrade (ticket 10.4): as the run approaches its budget, the
	// check may route the claim to a cheaper model in the step's
	// model_fallbacks chain instead of parking. It returns the re-targeted
	// config; because the model is the cache key's input, the primary's miss
	// binding is discarded and the cache is re-read on the fallback model
	// before proceeding — a fallback hit still short-circuits the provider call.
	budgeted, downgraded, berr := e.budgetCheck(ctx, step, executor, sc, origin)
	if budgeted {
		return berr
	}
	if downgraded != nil {
		sc.Config = downgraded
		var reReadErr error
		hit, cacheWB, reReadErr = e.cacheRead(ctx, step, executor, sc, chain, semanticAttempt)
		if hit {
			return reReadErr
		}
	}
	// Fleet-wide rate limiting (ticket 9.2, ADR-010): before a cost-bearing
	// executor's provider call, acquire the step's resource buckets. A
	// denial defers the step (throttle → delayed requeue, slot released now,
	// no counted attempt), an impossible request perm-fails, a Redis error
	// fails open. Executors that name no resource bypass this entirely.
	// A granted, token-metered acquire returns a binding so the middleware can
	// reconcile the estimate against real usage after the call (ticket 9.3).
	handled, rc, herr := e.rateLimit(ctx, step, executor, sc, origin)
	if handled {
		return herr
	}
	// The in-flight cancellation watch (ticket 5.6): polls the run's
	// status while the executor runs and cancels its context when the run
	// turns cancelling. Pure latency — the completion transactions below
	// re-check the run status under the run lock either way.
	watchCtx, stopWatch := e.watchRunCancel(ctx, step.RunID)
	execStart := e.now()
	execCtx, execSpan := e.tracer.Start(watchCtx, "step.executor")
	out, expired, execErr := runExecutor(execCtx, executor, sc, timeout)
	if execErr != nil {
		execSpan.RecordError(execErr)
		execSpan.SetStatus(codes.Error, "executor failed")
	}
	execSpan.End()
	execDur := e.now().Sub(execStart)
	stopWatch()
	if ctx.Err() != nil {
		// The handler context itself is canceled — the consumer's drain
		// deadline at shutdown (ticket 5.7), or the zero-drain immediate
		// cancellation tests use to simulate crashes. The run-cancel watch
		// never cancels this context (only watchCtx), so this is always
		// shutdown. No completion transaction can commit on a canceled
		// context, so decide nothing: the returned error leaves the entry
		// un-acked and the lease expires naturally into another worker's
		// reclaim/takeover — the crash path, which is exactly what an
		// abandon is.
		logger.WarnContext(ctx, "shutdown abandoned in-flight step; lease expires into reclaim",
			slog.Bool("executor_errored", execErr != nil))
		return fmt.Errorf("engine: step abandoned at shutdown: %w", context.Cause(ctx))
	}
	cancelled := errors.Is(context.Cause(watchCtx), errRunCancelled)
	if cancelled {
		// The routing below stays uniform: a success is honored (the
		// completion transaction skips fan-out on a cancelling run) and a
		// failure settles the step as cancelled — both via the run-status
		// check inside the completion transaction, which is the authority.
		logger.InfoContext(ctx, "executor interrupted by run cancellation",
			slog.Bool("executor_errored", execErr != nil))
	}
	// The step-duration histogram (ticket 7.2), labeled by the executor
	// invocation's outcome — mirroring the routing below, which the
	// completion transactions then make durable.
	e.metrics.StepDuration(step.StepType, executionOutcome(execErr, expired, cancelled), execDur)
	// Post-call token-cost reconciliation (ticket 9.3, ADR-010): the provider
	// call happened and reported real usage, so correct the token bucket by
	// actual − estimate before the completion transaction — even when the
	// success raced a timeout or a cancellation, the tokens were still spent.
	// An errored call carries no usage (out.Usage nil), so the estimate stays
	// debited, deliberately conservative. rc is non-nil only for a granted,
	// token-metered acquire.
	if execErr == nil && out.Usage != nil && rc != nil {
		e.reconcileTokens(ctx, rc, out.Usage.InputTokens+out.Usage.OutputTokens)
	}
	if expired {
		if execErr != nil {
			// The engine assigns the timeout class from context state
			// (ADR-006 row 3) — the executor's own error, whatever it
			// wrapped, is the recorded cause.
			return e.completeFailure(ctx, step, out,
				fmt.Errorf("step timed out after %s: %w", timeout, execErr), dag.ClassTimeout, origin.RunTrace)
		}
		// Success racing the deadline: the work is done, and discarding it
		// to record a timeout would waste budget and re-run side effects.
		logger.WarnContext(ctx, "executor succeeded after its timeout deadline; honoring the result",
			slog.Duration("timeout", timeout))
	}
	if execErr != nil {
		// Worker-side classification at completion time (ADR-006): a
		// declared class is honored, config-decode misses are permanent,
		// and everything unclassified defaults to transient.
		return e.completeFailure(ctx, step, out, execErr, classifyFailure(execErr), origin.RunTrace)
	}
	// Output validation (ticket 11.1, ADR-013): run the resolved chain over
	// the executor's output. It sits here — after a successful execute and
	// before cacheWrite — because an output that fails its chain must never
	// be cached (ADR-011). A validator's own error is a transport failure of
	// the validation stage (routes through the ADR-006 taxonomy); a fail
	// verdict routes to the validation_failed outcome (dead-letter, 11.1); a
	// pass (or no chain, verdict nil) proceeds to cacheWrite + success.
	verdict, vErr := e.runChain(ctx, chain, out, semanticAttempt)
	if vErr != nil {
		if ctx.Err() != nil {
			// Shutdown during validation — abandon like the executor path.
			return fmt.Errorf("engine: step abandoned during validation: %w", context.Cause(ctx))
		}
		// The executor SUCCEEDED (out carries its usage/resource) but the
		// validation chain errored — pass out so the productive spend is
		// metered even though the step fails (ticket 11.5 gap fix); the judge's
		// own billed usage rides on vErr.
		return e.completeFailure(ctx, step, out, vErr, validatorErrorClass(vErr), origin.RunTrace)
	}
	if verdict != nil && !verdict.Passed() {
		// Invalid output: never cached. Route through the semantic-retry engine
		// (ticket 11.4) — a feedback-augmented re-attempt if the semantic budget
		// remains, else dead-lettered as validation_failed with the verdict
		// history.
		return e.completeValidationFailure(ctx, step, out, *verdict, origin.RunTrace)
	}
	// Response cache write-through (ticket 9.5, ADR-011): a successful miss
	// whose policy writes stores its (validated) result for the next
	// identical request. cacheWB is non-nil only for a cache-eligible,
	// writing step that missed; every write failure is swallowed (fail-open
	// / oversize skip), so this never affects the completion.
	if cacheWB != nil {
		e.cacheWrite(ctx, cacheWB, out)
	}
	return e.completeSuccess(ctx, step, out, verdict, semanticAttempt)
}

// reconcileBinding carries what the middleware needs, after a granted
// token-metered acquire, to reconcile the pre-call estimate against the
// provider's real usage (ticket 9.3): the resource name to re-resolve, the
// resolved config-entry label for the estimate-error metric, and the estimate
// that was debited.
type reconcileBinding struct {
	name      string // the resource name to reconcile (re-resolved, as Acquire did)
	resource  string // the resolved config-entry name — the bounded metric label
	estTokens int64
}

// rateLimit is the M9 limiter middleware (ticket 9.2, ADR-010). For a
// cost-bearing executor — one implementing exec.ResourceClaimer — it
// acquires the step's fleet-wide resource buckets before the provider call.
// It returns handled=true when it decided the delivery, with the Handle/ACK
// error to return: a throttle (running → retrying + delayed requeue, ACK,
// slot released with no counted attempt) returns nil; an impossible request
// (estimate > token-bucket capacity, or a never-refilling bucket) perm-fails
// to the DLQ (ADR-010 wait-vs-never). It returns handled=false when the step
// may execute: no limiter wired, the executor names no resource, the claim
// binding could not be computed, the resource is unlimited, the acquire was
// granted, or a Redis error made it fail open (ADR-010 — a limit is never a
// correctness dependency). When a granted acquire debited the token bucket,
// it also returns a non-nil reconcileBinding so the caller can correct the
// estimate against real usage after the executor returns (ticket 9.3).
func (e *Engine) rateLimit(ctx context.Context, step gen.RunStep, executor exec.Executor, sc exec.StepContext, origin store.ClaimOrigin) (bool, *reconcileBinding, error) {
	if e.limiter == nil {
		return false, nil, nil
	}
	claimer, ok := executor.(exec.ResourceClaimer)
	if !ok {
		return false, nil, nil // names no resource — bypasses the limiter
	}
	logger := log.From(ctx)
	resourceName, estTokens, err := claimer.ResourceClaim(sc)
	if err != nil {
		// The binding could not be computed (unroutable model, corrupt
		// config). Don't classify here — let the executor run and land its
		// own permanent failure, so the routing judgment lives in one place.
		logger.WarnContext(ctx, "resource claim failed; skipping rate limit, executor will classify",
			slog.Any("error", err))
		return false, nil, nil
	}
	dec, err := e.limiter.Acquire(ctx, resourceName, estTokens)
	if err != nil {
		if errors.Is(err, resource.ErrCostExceedsCapacity) {
			// The estimate exceeds the token bucket's capacity — no refill
			// admits it (ADR-010 wait-vs-never). Permanent → DLQ.
			logger.ErrorContext(ctx, "token estimate exceeds resource capacity; failing permanently",
				slog.String("resource", resourceName),
				slog.Int64("est_tokens", estTokens),
				slog.Any("error", err))
			return true, nil, e.completeFailure(ctx, step, exec.Output{},
				fmt.Errorf("resource %q: %w", resourceName, err), dag.ClassPermanent, origin.RunTrace)
		}
		// Any other error is a Redis/transport failure: fail open.
		e.metrics.RateLimitFailOpen()
		logger.WarnContext(ctx, "resource limiter error; failing open (proceeding without limit)",
			slog.String("resource", resourceName),
			slog.Any("error", err))
		return false, nil, nil
	}
	if dec.Allowed {
		// A granted acquire that actually debited tokens gets reconciled after
		// the call; an unlimited or requests-only grant (nothing on the token
		// ledger) does not.
		if dec.TokensMetered {
			return false, &reconcileBinding{name: resourceName, resource: dec.Resource, estTokens: estTokens}, nil
		}
		return false, nil, nil
	}
	if dec.RetryAfter == resource.RetryAfterNever {
		// Denied by a never-refilling bucket — the tokens never come back
		// (ADR-010 wait-vs-never; v1 config forbids rate-zero buckets, so
		// this is defensive). Permanent → DLQ.
		logger.ErrorContext(ctx, "resource denied by a never-refilling bucket; failing permanently",
			slog.String("resource", resourceName),
			slog.String("bucket", dec.Bucket))
		return true, nil, e.completeFailure(ctx, step, exec.Output{},
			fmt.Errorf("resource %q bucket %q never refills — request can never be admitted", resourceName, dec.Bucket),
			dag.ClassPermanent, origin.RunTrace)
	}
	// A genuine throttle: defer the step, release the slot, no attempt spent.
	return true, nil, e.completeThrottle(ctx, step, dec, origin.RunTrace)
}

// reconcileTokens corrects the resource's token bucket by actual − estimate
// after a granted, token-metered call has returned (ticket 9.3, ADR-010). It
// records the signed estimate error on the histogram (by the bounded resolved
// resource label) and applies the correction; a limiter error is fail-open —
// logged and counted, never affecting the step's outcome, since a rate limit
// is never a correctness dependency.
func (e *Engine) reconcileTokens(ctx context.Context, rc *reconcileBinding, actualTokens int64) {
	e.metrics.EstimateError(rc.resource, actualTokens-rc.estTokens)
	if err := e.limiter.Reconcile(ctx, rc.name, rc.estTokens, actualTokens); err != nil {
		e.metrics.ReconcileFailure()
		log.From(ctx).WarnContext(ctx, "token-cost reconciliation failed; estimate stays debited (fail-open)",
			slog.String("resource", rc.resource),
			slog.Int64("est_tokens", rc.estTokens),
			slog.Int64("actual_tokens", actualTokens),
			slog.Any("error", err))
	}
}

// annotateAttemptSpan enriches the consumer's attempt span (ticket 7.3)
// once the claim has decided what this delivery is: the durable attempt
// number and step type as attributes; a link to the previous attempt's
// span when this claim re-executes work — a retry after a judged failure,
// or a takeover after the holder's lease expired (ADR-008: links, never
// parent-child); and, for fan-in joins, links to every firing parent's
// attempt span. Skipped entirely when no valid span context is active
// (tracing off), so the join-parent read never runs untraced.
func (e *Engine) annotateAttemptSpan(ctx context.Context, step gen.RunStep, origin store.ClaimOrigin) {
	span := oteltrace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return
	}
	span.SetAttributes(
		attribute.Int("attempt", int(step.AttemptCount)),
		attribute.String("step_type", step.StepType),
	)
	if link, ok := obstrace.LinkFromTraceparent(origin.PrevTraceSpan); ok {
		span.AddLink(link)
	}
	if step.StepType != string(dag.StepJoin) {
		return
	}
	parents, err := e.store.Steps().ListFiringParentTraceSpans(ctx, step.RunID, step.StepID)
	if err != nil {
		// Links are observability, never control flow: log and move on.
		log.From(ctx).WarnContext(ctx, "listing firing-parent trace spans failed; join links skipped",
			slog.Any("error", err))
		return
	}
	for _, s := range parents {
		if link, ok := obstrace.LinkFromTraceparent(s); ok {
			span.AddLink(link)
		}
	}
}

// claimAction is what the handler does with a delivery whose claim (or
// takeover) transaction failed.
type claimAction int

const (
	// claimRedeliver: nothing was decided — leave the entry in the PEL.
	claimRedeliver claimAction = iota
	// claimAckDrop: consuming this delivery is provably unnecessary — ACK
	// and drop.
	claimAckDrop
	// claimTakeover: reclaimed delivery of a running step — perform the
	// lease-expiry takeover (4.5) and execute.
	claimTakeover
)

// String renders the action for structured logs.
func (a claimAction) String() string {
	switch a {
	case claimAckDrop:
		return "ack_drop"
	case claimTakeover:
		return "takeover"
	default:
		return "redeliver"
	}
}

// claimDecision is a classified claim/takeover failure.
type claimDecision struct {
	action claimAction
	reason string
	level  slog.Level
	// holderClaim is the observed holder's fencing token, set only for
	// claimTakeover — the takeover CAS is fenced on it.
	holderClaim *uuid.UUID
	// fenced marks a rejection produced by claim_id fencing (a stale
	// takeover bounced off a newer live claim) — the 7.2 fencing counter.
	fenced bool
}

// classifyClaimFailure maps a failed claim onto ADR-005's ACK-discipline
// table. Pure — unit-tested without a database.
func classifyClaimFailure(err error, deliveryCount int64) claimDecision {
	var te *store.TransitionError
	switch {
	case errors.As(err, &te):
		if te.Reason == store.ConflictRunNotRunning {
			// The run-status guard (ticket 5.2, ADR-006): the run is
			// terminal (later: parked/cancelling), so executing this step
			// is provably unnecessary — drop the delivery.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "run not running (" + te.From + ") — dropping delivery",
			}
		}
		if te.Reason != store.ConflictWrongStatus {
			// ClaimStep guards only on status, so any other reason is a
			// protocol surprise; keep the entry pending — the rising
			// delivery count walks it to the visible poison path.
			return claimDecision{
				action: claimRedeliver, level: slog.LevelError,
				reason: "unexpected conflict reason " + string(te.Reason),
			}
		}
		switch te.From {
		case store.StepStatusSucceeded, store.StepStatusFailed, store.StepStatusSkipped,
			store.StepStatusDeadLettered, store.StepStatusCancelled:
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "step already terminal (" + te.From + ") — duplicate of finished work",
			}
		case store.StepStatusRetrying:
			// A delivery of a retrying step whose backoff has not elapsed
			// (a due one would have matched the claim CAS): an early
			// duplicate, or the original entry redelivered after a
			// post-commit crash. Dropping is safe — next_attempt_at is
			// durable, and the delayed entry or the reconciler's
			// overdue-retrying heal carries the future dispatch.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "step retrying with backoff pending — future dispatch is covered",
			}
		case store.StepStatusRunning:
			if deliveryCount <= 1 {
				// Fresh-delivery path: a concurrent duplicate. The live
				// holder's own entry covers the crash case.
				return claimDecision{
					action: claimAckDrop, level: slog.LevelInfo,
					reason: "step already running — concurrent duplicate delivery",
				}
			}
			// Reclaim path: the holder went silent past the lease TTL —
			// take over (ADR-005's lease-expiry takeover, ticket 4.5).
			if te.CurrentClaimID == nil {
				// A running step always carries a claim; nil is corrupt
				// state, and without an observed claim the fenced takeover
				// CAS cannot run. Keep the entry pending — visible.
				return claimDecision{
					action: claimRedeliver, level: slog.LevelError,
					reason: "running step has no claim_id — corrupt state, cannot take over",
				}
			}
			return claimDecision{
				action: claimTakeover, level: slog.LevelWarn,
				reason:      "reclaimed delivery of a running step — holder presumed dead, taking over",
				holderClaim: te.CurrentClaimID,
			}
		default:
			// pending (never dispatched → outbox bug) or an unknown
			// status: not provably unnecessary, keep it pending.
			return claimDecision{
				action: claimRedeliver, level: slog.LevelError,
				reason: "step in unexpected status " + te.From,
			}
		}
	case errors.Is(err, store.ErrNotFound):
		// Dangling reference: run or step row gone (e.g. deleted under a
		// retention policy).
		return claimDecision{
			action: claimAckDrop, level: slog.LevelWarn,
			reason: "run or step not found — dangling reference",
		}
	default:
		// Transport/transaction failure: nothing was decided; redeliver.
		return claimDecision{
			action: claimRedeliver, level: slog.LevelError,
			reason: "claim transaction failed",
		}
	}
}

// classifyTakeoverFailure maps a failed takeover-and-claim transaction onto
// the ACK discipline. Pure — unit-tested without a database. The From=ready
// takeover conflict never reaches here (swallowed inside the transaction);
// a claim conflict (To=running) cannot race under the held run lock, so it
// is a protocol surprise.
func classifyTakeoverFailure(err error) claimDecision {
	var te *store.TransitionError
	switch {
	case errors.As(err, &te):
		if te.Reason == store.ConflictRunNotRunning {
			// The post-takeover claim hit the run-status guard (ticket
			// 5.2): the run is terminal, so the takeover rolled back with
			// it and the delivery is unnecessary.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "run not running (" + te.From + ") — dropping reclaimed delivery",
			}
		}
		if te.To != store.StepStatusReady {
			return claimDecision{
				action: claimRedeliver, level: slog.LevelError,
				reason: "unexpected conflict on the post-takeover claim",
			}
		}
		switch {
		case te.Reason == store.ConflictClaimMismatch:
			// The step was taken over and re-claimed by a live holder since
			// this entry's observation — the observed claim is stale. The
			// new holder claimed off its own entry, which covers its crash
			// case, so dropping this one is safe.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelWarn,
				reason: "step re-claimed by a newer holder since observation — stale takeover dropped",
				fenced: true,
			}
		case te.Reason == store.ConflictWrongStatus &&
			(te.From == store.StepStatusSucceeded || te.From == store.StepStatusFailed ||
				te.From == store.StepStatusSkipped || te.From == store.StepStatusRetrying ||
				te.From == store.StepStatusDeadLettered || te.From == store.StepStatusCancelled):
			// ADR-005's takeover race rule: the holder's completion — a
			// terminal one, or since 5.2 a retry routing (5.4: or a
			// dead-lettering) — committed between the reclaim and the
			// takeover CAS. Either way that commit consumed the work this
			// entry carried.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "step completed between reclaim and takeover (" + te.From + ") — duplicate of finished work",
			}
		default:
			return claimDecision{
				action: claimRedeliver, level: slog.LevelError,
				reason: "unexpected takeover conflict (" + string(te.Reason) + " from " + te.From + ")",
			}
		}
	case errors.Is(err, store.ErrNotFound):
		return claimDecision{
			action: claimAckDrop, level: slog.LevelWarn,
			reason: "run or step not found — dangling reference",
		}
	default:
		return claimDecision{
			action: claimRedeliver, level: slog.LevelError,
			reason: "takeover transaction failed",
		}
	}
}

// executionOutcome derives the step-duration metric's outcome label from
// one executor invocation's result, mirroring the routing in execute: the
// classed failure vocabulary plus succeeded and cancelled (`lost` never
// appears — a crashed worker records nothing). Pure — unit-tested without
// a database.
func executionOutcome(execErr error, expired, cancelled bool) string {
	switch {
	case execErr == nil:
		return store.StepStatusSucceeded
	case cancelled:
		// The completion transaction settles a failure on a cancelling run
		// as cancelled, never judged (ADR-006 row 8).
		return store.AttemptOutcomeCancelled
	case expired:
		return store.AttemptOutcomeTimeout
	default:
		return string(classifyFailure(execErr))
	}
}

// claimIDString renders the step's claim ID for logs. ClaimStep always
// returns a non-nil claim, but a nil-check beats a panic in a log call.
func claimIDString(step gen.RunStep) string {
	if step.ClaimID == nil {
		return "<none>"
	}
	return step.ClaimID.String()
}
