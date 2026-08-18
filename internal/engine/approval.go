package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
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
	logger.InfoContext(ctx, "step parked awaiting human approval; acking (no lease held)",
		slog.String("approval_id", approval.ID.String()),
		slog.Any("timeout_at", approval.TimeoutAt))
	// ACK: return nil. The PEL now holds nothing for this step; the decision
	// path (15.3) resumes it through the ordinary dispatch path.
	return nil
}
