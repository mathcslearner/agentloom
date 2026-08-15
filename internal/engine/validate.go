package engine

// This file is the engine's output-validation stage (ticket 11.1,
// ADR-013): resolving a claimed step's materialized validation chain,
// running it over the executor's output, and routing a failing verdict to
// the validation_failed outcome. It sits in execute() after a successful
// executor call and before the response-cache write — an output that fails
// its chain is never cached (ADR-011) — and the cache-hit path re-runs it,
// so a stored-but-now-invalid output is re-executed rather than served.
//
// A validator that returns an error (an llm_judge provider error, 11.5) is
// a transport failure of the validation stage itself: it routes through the
// ADR-006 taxonomy (completeFailure), NOT as a fail verdict. A fail verdict
// is a successful validation with a negative result, recorded as the
// validation_failed attempt outcome with the verdict persisted, then
// dead-lettered (source retries_exhausted — the semantic-retry budget,
// implicitly one attempt in 11.1; 11.4 makes it configurable and inserts
// the feedback-augmented re-attempt before this terminal step).

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// resolveChain resolves a claimed step's materialized validation chain
// (ADR-013's pre-flight gate). It reads the validation_policy off the
// claimed row (never reparsing the definition snapshot), decodes it, and
// resolves each entry against the validator registry — an unknown
// validator or a config that does not match its validator's schema is a
// deterministic permanent failure that must fire before any money is spent.
// A step with no chain (nil policy, or no registry wired and no chain
// authored) resolves to (nil, nil); the caller then does zero validation
// work. A returned error is always permanent (the caller records it so).
func (e *Engine) resolveChain(step gen.RunStep) (*validate.Chain, error) {
	var authored *dag.ValidationPolicy
	if len(step.ValidationPolicy) > 0 {
		var policy dag.ValidationPolicy
		if err := json.Unmarshal(step.ValidationPolicy, &policy); err != nil {
			// Corrupt materialized policy — deterministic stored-state failure,
			// like a corrupt retry policy or timeout: permanent.
			return nil, err
		}
		authored = &policy
	}
	// The implicit structured-output validator (ticket 11.3, ADR-013): an llm
	// step declaring an output_format prepends a json_schema entry that
	// enforces the declared shape over the (possibly repaired) completion, so
	// an unrepairable output is a validation_failed verdict (feeding 11.4's
	// semantic retry) rather than a silent success. Prepending keeps the
	// cheap structural check ahead of any author-supplied validators.
	implicit, ierr := implicitOutputFormatSpec(step)
	if ierr != nil {
		return nil, ierr
	}
	policy := mergeValidationPolicy(implicit, authored)
	if policy == nil {
		return nil, nil // no chain authored and no output_format — zero work
	}
	return validate.Resolve(e.validators, policy, dag.StepType(step.StepType))
}

// implicitOutputFormatSpec builds the synthetic json_schema validator an llm
// step's output_format implies (ticket 11.3), or nil when the step declares
// none. A json_schema format enforces the author's schema; a plain json
// format enforces parseability (an empty `{}` schema matches any JSON, so
// only a non-JSON output — an unrepairable completion — fails, as
// invalid_json). The schema is read off the materialized config (output_format
// is never templated), so this needs no rendered config.
func implicitOutputFormatSpec(step gen.RunStep) (*dag.ValidatorSpec, error) {
	if dag.StepType(step.StepType) != dag.StepLLM || len(step.Config) == 0 {
		return nil, nil
	}
	cfg, err := dag.DecodeStepConfig(dag.StepLLM, step.Config)
	if err != nil {
		// Corrupt materialized config — deterministic, permanent (the render
		// stage lands the same class independently).
		return nil, err
	}
	llmCfg, ok := cfg.(*dag.LLMConfig)
	if !ok || llmCfg.OutputFormat == nil {
		return nil, nil
	}
	schema := llmCfg.OutputFormat.Schema
	if len(schema) == 0 {
		// Plain json format: any JSON accepted, so an empty schema — the
		// parseability-only check.
		schema = json.RawMessage(`{}`)
	}
	config, err := json.Marshal(map[string]json.RawMessage{"schema": schema})
	if err != nil {
		return nil, err
	}
	return &dag.ValidatorSpec{Name: validate.NameJSONSchema, Config: config}, nil
}

// mergeValidationPolicy prepends the implicit validator (if any) to the
// authored chain (if any), returning nil when there is nothing to run.
func mergeValidationPolicy(implicit *dag.ValidatorSpec, authored *dag.ValidationPolicy) *dag.ValidationPolicy {
	if implicit == nil {
		return authored
	}
	merged := &dag.ValidationPolicy{Validators: []dag.ValidatorSpec{*implicit}}
	if authored != nil {
		merged.Validators = append(merged.Validators, authored.Validators...)
	}
	return merged
}

// runChain runs a resolved chain over a step's output (ADR-013's chain
// semantics), returning the aggregated verdict — or nil when the chain is
// empty (no validation). A non-nil error is a transport failure of a
// validator (an llm_judge provider error, a ctx cancellation): the caller
// routes it through the ADR-006 taxonomy, not as a fail verdict. The
// semantic attempt is always 1 in 11.1 (no loop yet); 11.4 threads the
// real semantic attempt number.
func (e *Engine) runChain(ctx context.Context, chain *validate.Chain, out exec.Output) (*validate.Verdict, error) {
	if chain.Empty() {
		return nil, nil
	}
	v, err := chain.Run(ctx, out.Data, validationAttemptNumber, log.From(ctx))
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// validationAttemptNumber is the semantic attempt number the engine passes
// to a validation chain. Always 1 in 11.1 (no semantic-retry loop); 11.4
// derives it from the semantic retry counter. Kept as a named constant so
// the 11.4 change has one obvious site.
const validationAttemptNumber = 1

// validatorErrorClass maps a validator's transport error onto its ADR-006
// class. A *validate.Error carries its declared class (transient or
// permanent); a context error and everything else default to transient (the
// classifyFailure convention). The original error is still recorded by
// completeFailure — only the class is derived here.
func validatorErrorClass(err error) dag.ErrorClass {
	var ve *validate.Error
	if errors.As(err, &ve) {
		switch ve.Class {
		case dag.ClassTransient, dag.ClassPermanent:
			return ve.Class
		}
	}
	return dag.ClassTransient
}

// completeValidationFailure is the validation-failure route (ADR-013's
// decision table): the executor succeeded but the output failed its chain.
// One transaction records the validation_failed attempt (with its usage and
// verdict persisted), meters the spent cost, and dead-letters the step
// (source retries_exhausted — the semantic budget is exhausted at one
// attempt in 11.1) so the run's on_failure disposition applies — the
// ordinary DLQ machinery, no new disposition logic. On a cancelling run the
// step settles cancelled instead (the failure is not judged, ADR-006 row 8),
// mirroring completeFailure.
//
// The verdict is load-bearing evidence: a marshal failure of it is a
// permanent step failure (unlike usage, which the success path drops with a
// warning), because losing the verdict would erase why the output was
// rejected.
func (e *Engine) completeValidationFailure(ctx context.Context, step gen.RunStep, out exec.Output, verdict validate.Verdict, runTrace store.TraceContext) error {
	logger := log.From(ctx)

	verdictJSON, verr := verdict.Marshal()
	if verr != nil {
		logger.ErrorContext(ctx, "marshaling validation verdict failed; failing step permanently",
			slog.Any("error", verr))
		return e.completeFailure(ctx, step, verr, dag.ClassPermanent, runTrace)
	}
	payload, perr := json.Marshal(map[string]any{
		"message": validationFailureMessage(verdict),
		"class":   string(dag.ClassValidationFailed),
		"issues":  verdict.Issues,
	})
	if perr != nil {
		payload = json.RawMessage(`{"message":"output validation failed","class":"validation_failed"}`)
	}

	// The attempt's token usage (the provider call happened and billed even
	// though the output was rejected — ADR-012 rule 5 amended): marshaled
	// here so it lands on the attempt row inside the transaction.
	var usage json.RawMessage
	if out.Usage != nil {
		if u, merr := json.Marshal(out.Usage); merr == nil {
			usage = u
		}
	}
	// The structured-output provenance (ticket 11.3): a validation_failed
	// output of a structured-output step carries how it was shaped (repaired /
	// unrepairable), the evidence 11.4's feedback builder reads alongside the
	// verdict. Best-effort like usage — the verdict is the load-bearing record.
	repairJSON := marshalRepair(ctx, logger, out.Repair)

	now := e.now()
	// The attempt's cost row (ADR-012): a validation_failed attempt spent
	// real money, so it ledgers exactly like a succeeded one. Priced before
	// the transaction — pure, no reads.
	costRow := e.priceAttempt(ctx, step, out, now)

	runFailed := false
	cancelSettled := false
	var terminalRun *gen.Run
	var run gen.Run
	var cancelledSteps []string
	var fenced *store.TransitionError
	txCtx, txSpan := e.tracer.Start(ctx, "step.completion")
	txErr := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		status, serr := store.LockRunStatus(ctx, q, step.RunID, now)
		if serr != nil {
			return serr
		}
		if status == store.RunStatusCancelling {
			// On a cancelling run the failure is not judged (ADR-006 row 8);
			// the step settles cancelled with the executor's output-failure
			// summary preserved, and the transaction attempts finalization.
			if _, cerr := store.CancelRunningStep(ctx, q, store.CancelRunningStepArgs{
				RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
				Reason: store.CancelReasonRunCancelled, Error: payload, Now: now,
			}); cerr != nil {
				errors.As(cerr, &fenced)
				return cerr
			}
			cancelSettled = true
			var rerr error
			terminalRun, rerr = attemptCancelRollup(ctx, q, step.RunID, now)
			return rerr
		}
		if _, err := store.DeadLetterStep(ctx, q, store.DeadLetterStepArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
			Source: store.DeadLetterSourceRetriesExhausted, Outcome: store.AttemptOutcomeValidationFailed,
			Error: payload, Usage: usage, Verdict: verdictJSON, Repair: repairJSON, Now: now,
		}); err != nil {
			errors.As(err, &fenced)
			return err
		}
		// The cost ledger row + run-aggregate bump, atomic with the
		// dead-letter and under the run lock DeadLetterStep took (the
		// completeSuccess pattern).
		if costRow != nil {
			if _, err := store.ApplyAttemptCost(ctx, q, *costRow); err != nil {
				return err
			}
		}
		if err := failpoint(stageAfterStepTransition); err != nil {
			return err
		}
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
		logger.ErrorContext(ctx, "validation-failure completion transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return txErr
	}

	if costRow != nil {
		e.recordCost(*costRow)
	}
	if cancelSettled {
		logger.InfoContext(ctx, "step output invalid but run is cancelling — settled cancelled, not judged",
			slog.Bool("run_cancelled", terminalRun != nil))
		if terminalRun != nil {
			e.recordRunCompleted(terminalRun.Status, terminalRun.StartedAt, now)
		}
		return nil
	}
	e.metrics.DeadLetter(store.DeadLetterSourceRetriesExhausted)
	if runFailed {
		e.recordRunCompleted(store.RunStatusFailed, run.StartedAt, now)
	}
	logger.WarnContext(ctx, "step output failed validation; dead-lettered",
		slog.Int("issue_count", len(verdict.Issues)),
		slog.Int("steps_cancelled", len(cancelledSteps)),
		slog.Bool("run_failed", runFailed))
	return nil
}

// marshalRepair marshals a step's structured-output provenance (ticket 11.3)
// for the attempt row, or returns nil when the step had none. A marshal
// failure of this fixed struct is logged and dropped (NULL), never fatal —
// the output payload is the truth, provenance is diagnostic.
func marshalRepair(ctx context.Context, logger *slog.Logger, repair *exec.Repair) json.RawMessage {
	if repair == nil {
		return nil
	}
	rb, merr := json.Marshal(repair)
	if merr != nil {
		logger.WarnContext(ctx, "marshaling repair provenance failed; recording none",
			slog.Any("error", merr))
		return nil
	}
	return rb
}

// validationFailureMessage renders a short, structure-only summary of a
// failing verdict for the dead-letter record — the issue codes and paths,
// never instance values.
func validationFailureMessage(v validate.Verdict) string {
	if len(v.Issues) == 0 {
		return "output validation failed"
	}
	first := v.Issues[0]
	msg := "output validation failed: " + first.Validator + ": " + first.Code
	if first.Path != "" {
		msg += " at " + first.Path
	}
	if len(v.Issues) > 1 {
		msg += " (and more)"
	}
	return msg
}
