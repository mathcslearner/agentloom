package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// completeAwaitHuman parks a human_approval step without a lease (ticket
// 15.2, ADR-017). The executor produced the rendered, validated approval
// content (exec.ApprovalRequest); this settles it: in one transaction it
// writes the pending approvals row and CASes the step
// running → awaiting_human (fenced on the claim), then ACKs (returns nil) so
// the PEL holds nothing and no worker slot is pinned while a human
// deliberates. The attempt row is left open — it spans the wait, closed by
// 15.3's decision or a run cancel.
//
// Mirrors completeBudgetPark's structure: a cancelling run settles the step
// as cancelled (never parked); a fenced conflict abandons without ACK (the
// zombie's completion is rejected); a transport failure redelivers. The run
// itself stays running — only the step parks (a run-level park for
// awaiting_human is 15.4's on_timeout: park escalation).
func (e *Engine) completeAwaitHuman(ctx context.Context, step gen.RunStep, out exec.Output, runTrace store.TraceContext) error {
	logger := log.From(ctx)
	now := e.now()

	// Decode the executor's handoff. A corrupt payload is a deterministic
	// permanent failure — the executor is the only writer, so this cannot
	// heal on retry.
	var req exec.ApprovalRequest
	if err := json.Unmarshal(out.Data, &req); err != nil {
		logger.ErrorContext(ctx, "corrupt human_approval executor output; recording step failure",
			slog.Any("error", err))
		return e.completeFailure(ctx, step, exec.Output{},
			fmt.Errorf("decoding approval request: %v", err), dag.ClassPermanent, runTrace)
	}
	if req.Title == "" {
		return e.completeFailure(ctx, step, exec.Output{},
			errors.New("approval request missing title"), dag.ClassPermanent, runTrace)
	}

	approval := store.ApprovalRow{
		ID:               uuid.New(),
		Title:            req.Title,
		Description:      req.Description,
		Payload:          req.Payload,
		AllowedDecisions: req.AllowedDecisions,
		AllowEdit:        req.AllowEdit,
		EditSchema:       req.EditSchema,
	}
	// timeout_at is persisted now so 15.4's expiry scheduler has it; the
	// delayed-queue schedule itself is 15.4. 0 = wait indefinitely.
	if req.Timeout > 0 {
		at := now.Add(req.Timeout)
		approval.TimeoutAt = &at
	}

	cancelSettled := false
	var terminalRun *gen.Run
	var fenced *store.TransitionError
	txCtx, txSpan := e.tracer.Start(ctx, "step.completion")
	txErr := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		status, serr := store.LockRunStatus(ctx, q, step.RunID, now)
		if serr != nil {
			return serr
		}
		if status == store.RunStatusCancelling {
			// The run is cancelling — settle the step as cancelled instead of
			// parking (ADR-006 row 8), exactly as budget-park does. No
			// approvals row is written.
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
		if _, err := store.AwaitHumanStep(ctx, q, store.AwaitHumanStepArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: *step.ClaimID,
			Approval: approval, Now: now,
		}); err != nil {
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
		logger.ErrorContext(ctx, "human-approval park transaction failed; delivery will redeliver",
			slog.Any("error", txErr))
		return txErr
	}
	if cancelSettled {
		logger.InfoContext(ctx, "human_approval step on a cancelling run — settled as cancelled",
			slog.Bool("run_cancelled", terminalRun != nil))
		if terminalRun != nil {
			e.recordRunCompleted(terminalRun.Status, terminalRun.StartedAt, now)
		}
		return nil
	}
	// Crash seam (ticket 13.5): E5 — the park committed (the approvals row is
	// durable and the step is awaiting_human) but the ACK has not happened;
	// die here to prove the crash-before-ACK path converges via ACK-and-drop
	// (a redelivery sees awaiting_human → ack_drop). Inert unless
	// AGENTLOOM_WORKER_CRASH_POINT arms post_commit on this step.
	maybeCrash(CrashStagePostCommit, step.StepID)
	// Schedule the timeout expiry (ticket 15.4) through the delayed queue, due
	// at the approval's deadline. Best-effort and post-commit like scheduleRetry:
	// a nil scheduler or a failed Schedule is survivable — the reconciler's
	// overdue-approvals scan re-fires a lost expiry — so it never turns into a
	// handler error that would un-ACK the park.
	if approval.TimeoutAt != nil {
		e.scheduleApprovalExpiry(ctx, step, *approval.TimeoutAt, runTrace)
	}
	logger.InfoContext(ctx, "step parked awaiting human approval; acking (no lease held)",
		slog.String("approval_id", approval.ID.String()),
		slog.Any("timeout_at", approval.TimeoutAt))
	// ACK: return nil. The PEL now holds nothing for this step; the decision
	// path (15.3) resumes it through the ordinary dispatch path.
	return nil
}

// approvalTimeoutEnvelope builds the expiry envelope for a parked approval
// step (ticket 15.4): a pointer naming the step, reason approval_timeout,
// carrying the run's durable root trace context. It is built without an
// EnqueuedAt so re-scheduling the same (run, step) expiry encodes to a
// byte-identical delayed member and ZADD dedups to one pending expiry.
func approvalTimeoutEnvelope(runID uuid.UUID, stepID string, runTrace store.TraceContext) queue.Envelope {
	return queue.Envelope{
		RunID: runID, StepID: stepID, Reason: queue.ReasonApprovalTimeout,
		TraceParent: runTrace.Parent, TraceState: runTrace.State,
	}
}

// scheduleApprovalExpiry schedules a parked approval's timeout expiry through
// the delayed queue, due at timeoutAt. Failures (and a nil scheduler) only log
// — the committed approval carries timeout_at, so the reconciler's
// overdue-approvals scan re-dispatches a lost expiry; nothing here may become a
// handler error, which would un-ACK an already-committed park.
func (e *Engine) scheduleApprovalExpiry(ctx context.Context, step gen.RunStep, timeoutAt time.Time, runTrace store.TraceContext) {
	logger := log.From(ctx)
	if e.scheduler == nil {
		logger.WarnContext(ctx, "no scheduler configured; reconciler will fire the approval timeout")
		return
	}
	env := approvalTimeoutEnvelope(step.RunID, step.StepID, runTrace)
	if err := e.scheduler.Schedule(ctx, env, timeoutAt); err != nil {
		logger.ErrorContext(ctx, "scheduling approval timeout failed; reconciler will fire it",
			slog.Time("timeout_at", timeoutAt), slog.Any("error", err))
	}
}

// handleApprovalTimeout applies an approval's on_timeout policy (ticket 15.4,
// ADR-017). An approval_timeout delivery (from the delayed queue, or a
// reconciler heal) names the parked step; this resolves the step's current
// pending approval and, if its timeout has genuinely passed, applies the
// policy through the SAME arbiter CAS as a human decision — so a decide racing
// the expiry has exactly one winner. The return follows the Handle/ACK
// contract: nil = handled (policy applied, or nothing to do → ack-and-drop);
// non-nil = redeliver.
func (e *Engine) handleApprovalTimeout(ctx context.Context, d queue.Delivery) error {
	logger := log.From(ctx)
	now := e.now()

	approval, err := e.store.Approvals().GetPendingByStep(ctx, d.Envelope.RunID, d.Envelope.StepID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No pending approval for this step — already decided, expired, or
			// cancelled. The expiry is moot; ack-and-drop.
			logger.DebugContext(ctx, "approval timeout for a step with no pending approval; ack-drop")
			return nil
		}
		return err // transport error → redeliver
	}
	if approval.TimeoutAt == nil {
		// Defensive: an approval with no deadline should never have been
		// scheduled. Nothing to do.
		logger.WarnContext(ctx, "approval timeout for an approval with no deadline; ack-drop",
			slog.String("approval_id", approval.ID.String()))
		return nil
	}
	if approval.ExpiredAt != nil {
		// The on_timeout: park policy was already applied (the run was parked
		// once). A redelivery or reconciler heal lands here; ack-and-drop so the
		// run is not re-parked.
		logger.DebugContext(ctx, "approval timeout already applied (park); ack-drop",
			slog.String("approval_id", approval.ID.String()))
		return nil
	}
	if now.Before(*approval.TimeoutAt) {
		// Not yet due (an AOF replay or a manual redelivery ahead of the score).
		// Re-schedule at the real deadline rather than firing early; ack-and-drop
		// the premature delivery.
		e.scheduleApprovalExpiry(ctx, gen.RunStep{RunID: approval.RunID, StepID: approval.StepID},
			*approval.TimeoutAt, store.TraceContext{Parent: d.Envelope.TraceParent, State: d.Envelope.TraceState})
		logger.DebugContext(ctx, "approval timeout not yet due; rescheduled",
			slog.String("approval_id", approval.ID.String()), slog.Time("timeout_at", *approval.TimeoutAt))
		return nil
	}

	policy := e.approvalTimeoutPolicy(ctx, approval)
	switch policy {
	case dag.ApprovalTimeoutPark:
		return e.applyTimeoutPark(ctx, approval, now)
	case dag.ApprovalTimeoutApprove:
		return e.applyTimeoutDecision(ctx, approval, dag.ApprovalApprove, policy)
	default: // reject (the default when a timeout is set)
		return e.applyTimeoutDecision(ctx, approval, dag.ApprovalReject, policy)
	}
}

// approvalTimeoutPolicy reads the parked step's materialized on_timeout policy.
// The step config is never templated, so decoding the materialized config is
// authoritative; an unreadable/absent policy defaults to reject (the safe
// default when a timeout is set — ADR-017).
func (e *Engine) approvalTimeoutPolicy(ctx context.Context, approval gen.Approval) dag.ApprovalTimeoutPolicy {
	step, err := e.store.Steps().Get(ctx, approval.RunID, approval.StepID)
	if err != nil {
		log.From(ctx).WarnContext(ctx, "reading approval step config for timeout policy failed; defaulting to reject",
			slog.Any("error", err))
		return dag.ApprovalTimeoutReject
	}
	if cfg, derr := dag.DecodeStepConfig(dag.StepHumanApproval, step.Config); derr == nil {
		if hac, ok := cfg.(*dag.HumanApprovalConfig); ok && hac.OnTimeout != "" {
			return hac.OnTimeout
		}
	}
	return dag.ApprovalTimeoutReject
}

// applyTimeoutDecision applies a reject/approve timeout policy through
// Control.Decide — the same CAS a human decision uses, so a decide racing the
// expiry has exactly one winner. A lost race (the approval is no longer
// pending) or a non-live run is ack-and-drop; a transport error redelivers.
func (e *Engine) applyTimeoutDecision(ctx context.Context, approval gen.Approval, decision dag.ApprovalDecision, policy dag.ApprovalTimeoutPolicy) error {
	logger := log.From(ctx)
	_, err := e.Decide(ctx, approval.ID, DecideRequest{
		Decision:  decision,
		DecidedBy: store.ApprovalActorTimeout,
		Source:    store.ApprovalSourceTimeout,
	})
	if err != nil {
		var notPending *store.ApprovalNotPendingError
		if errors.As(err, &notPending) {
			logger.InfoContext(ctx, "approval timeout lost the race to a decision; ack-drop",
				slog.String("approval_id", approval.ID.String()),
				slog.String("winning_status", notPending.Status))
			return nil
		}
		var te *store.TransitionError
		if errors.As(err, &te) && te.Reason == store.ConflictRunNotRunning {
			// The run is not running/parked (failed/cancelled/terminal) — the
			// gate is moot. Ack-and-drop.
			logger.InfoContext(ctx, "approval timeout on a non-live run; ack-drop",
				slog.String("approval_id", approval.ID.String()))
			return nil
		}
		return err // transport error → redeliver
	}
	action := store.ApprovalActionRejected
	if decision == dag.ApprovalApprove {
		action = store.ApprovalActionApproved
	}
	e.metrics.ApprovalDecided(string(decision), store.ApprovalSourceTimeout)
	e.metrics.ApprovalTimeout(action)
	logger.InfoContext(ctx, "approval timeout policy applied",
		slog.String("approval_id", approval.ID.String()),
		slog.String("policy", string(policy)),
		slog.String("decision", string(decision)))
	return nil
}

// applyTimeoutPark applies the on_timeout: park policy (ticket 15.4): the
// approval stays pending (still decidable) and the run parks as an escalation.
// A lost race (the approval is no longer pending, or its park policy was
// already applied) is ack-and-drop; a transport error redelivers.
func (e *Engine) applyTimeoutPark(ctx context.Context, approval gen.Approval, now time.Time) error {
	logger := log.From(ctx)
	var res store.ExpireApprovalParkResult
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var perr error
		res, perr = store.ExpireApprovalPark(ctx, q, store.ExpireApprovalParkArgs{
			ID: approval.ID, RunID: approval.RunID, StepID: approval.StepID,
			AttemptNo: approval.Attempt, TimeoutAt: approval.TimeoutAt, Now: now,
		})
		return perr
	})
	if txErr != nil {
		var notPending *store.ApprovalNotPendingError
		var already *store.ApprovalAlreadyExpiredError
		if errors.As(txErr, &notPending) || errors.As(txErr, &already) {
			logger.InfoContext(ctx, "approval timeout park already resolved; ack-drop",
				slog.String("approval_id", approval.ID.String()))
			return nil
		}
		return txErr // transport error → redeliver
	}
	e.metrics.ApprovalTimeout(res.Action)
	logger.InfoContext(ctx, "approval timeout policy applied: park",
		slog.String("approval_id", approval.ID.String()),
		slog.String("action", res.Action))
	return nil
}
