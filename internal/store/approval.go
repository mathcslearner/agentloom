package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// ApprovalRequestedEvent is the approval_requested event payload (ticket
// 15.2, ADR-017): a human_approval step parked and a pending approval was
// written. The M16 event feed and the M18 approval inbox read it to surface
// a new pending decision without reading the approvals table directly.
type ApprovalRequestedEvent struct {
	ApprovalID       string   `json:"approval_id"`
	StepID           string   `json:"step_id"`
	AttemptNo        int32    `json:"attempt"`
	Title            string   `json:"title"`
	AllowedDecisions []string `json:"allowed_decisions"`
	AllowEdit        bool     `json:"allow_edit,omitempty"`
	// TimeoutAt is when 15.4's expiry fires; nil = wait indefinitely.
	TimeoutAt *time.Time `json:"timeout_at,omitempty"`
}

// ApprovalCancelledEvent is the approval_cancelled event payload (ticket
// 15.2): a pending approval was cancelled because its run was cancelled.
type ApprovalCancelledEvent struct {
	ApprovalID string `json:"approval_id"`
	StepID     string `json:"step_id"`
	Reason     string `json:"reason"`
}

// AwaitHumanStepArgs are the inputs to AwaitHumanStep.
type AwaitHumanStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// ClaimID is the fencing token ClaimStep issued to this caller; the park
	// CAS is fenced on it so a zombie's park is rejected.
	ClaimID uuid.UUID
	// Approval carries the rendered content and edit constraints of the
	// pending approval row.
	Approval ApprovalRow
	// Now is the injected current time. Required.
	Now time.Time
}

// ApprovalRow is the rendered, snapshotted content of a pending approval —
// what AwaitHumanStep inserts and the run-status view surfaces.
type ApprovalRow struct {
	// ID is the approval's UUID; the engine mints it before the transaction.
	ID uuid.UUID
	// Title / Description / Payload are the rendered (8.2) config values, a
	// snapshot immune to later graph changes.
	Title       string
	Description string
	Payload     json.RawMessage
	// AllowedDecisions is the resolved decision set (never empty — the engine
	// applies the [approve, reject] default before park).
	AllowedDecisions []string
	AllowEdit        bool
	// EditSchema constrains an edited payload at decide time (15.3); nil when
	// the step permits any JSON edit.
	EditSchema json.RawMessage
	// TimeoutAt is when the timeout policy fires (15.4); nil = wait
	// indefinitely.
	TimeoutAt *time.Time
}

// AwaitHumanStep parks a human_approval step without a lease (ticket 15.2,
// ADR-017): in one transaction under the run lock it CASes the step
// running → awaiting_human (fenced on the caller's claim, clearing the
// claim so no lease is held), inserts the pending approvals row, and appends
// the approval_requested event. The attempt row is left open — the attempt
// spans the human wait, closed by 15.3's decision or a run cancel.
//
// A fenced conflict (the claim is stale) returns a *TransitionError so the
// caller abandons without ACKing (the engine's abandonFenced path). The
// unique partial index on (run_id, step_id) WHERE status = 'pending' makes a
// duplicate pending row impossible — but a duplicate delivery cannot reach
// here anyway, because the fenced CAS already rejects a second park of the
// same claim.
func AwaitHumanStep(ctx context.Context, q Querier, args AwaitHumanStepArgs) (gen.RunStep, error) {
	const op = "await human step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.AwaitHumanRunStep(ctx, gen.AwaitHumanRunStepParams{
		RunID: args.RunID, StepID: args.StepID, ClaimID: &args.ClaimID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusRunning, to: StepStatusAwaitingHuman, claim: &args.ClaimID,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if _, err := gq.InsertApproval(ctx, gen.InsertApprovalParams{
		ID:               args.Approval.ID,
		RunID:            args.RunID,
		StepID:           args.StepID,
		Attempt:          step.AttemptCount,
		Title:            args.Approval.Title,
		Description:      args.Approval.Description,
		Payload:          args.Approval.Payload,
		AllowedDecisions: args.Approval.AllowedDecisions,
		AllowEdit:        args.Approval.AllowEdit,
		EditSchema:       args.Approval.EditSchema,
		TimeoutAt:        args.Approval.TimeoutAt,
		Now:              args.Now,
	}); err != nil {
		return gen.RunStep{}, wrapErr(op+": insert approval", err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventApprovalRequested, ApprovalRequestedEvent{
		ApprovalID:       args.Approval.ID.String(),
		StepID:           args.StepID,
		AttemptNo:        step.AttemptCount,
		Title:            args.Approval.Title,
		AllowedDecisions: args.Approval.AllowedDecisions,
		AllowEdit:        args.Approval.AllowEdit,
		TimeoutAt:        args.Approval.TimeoutAt,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).InfoContext(ctx, "step parked awaiting human approval",
		log.RunID(args.RunID.String()), log.StepID(args.StepID),
		log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// CancelAwaitingHumanStepArgs are the inputs to CancelAwaitingHumanStep.
type CancelAwaitingHumanStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Reason records why the step was cancelled — the step_cancelled event
	// payload (CancelReasonRunCancelled). Required.
	Reason string
	// Now is the injected current time. Required.
	Now time.Time
}

// CancelAwaitingHumanStep settles a parked approval step during a run cancel
// (ticket 15.2, ADR-017): the 5.6 run-cancel sweep, widened to
// awaiting_human. In one transaction under the run lock it CASes the step
// awaiting_human → cancelled, closes the open attempt with the
// administrative outcome `cancelled` (the attempt spanned the wait), bumps
// steps_cancelled, appends step_cancelled, and marks the pending approval
// cancelled with an approval_cancelled event. A stale expiry that later
// fires (15.4) finds the row non-pending and no-ops.
func CancelAwaitingHumanStep(ctx context.Context, q Querier, args CancelAwaitingHumanStepArgs) (gen.RunStep, error) {
	const op = "cancel awaiting human step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if args.Reason == "" {
		return gen.RunStep{}, errors.New("store: " + op + ": empty Reason — pass the cancel reason")
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.CancelAwaitingHumanRunStep(ctx, gen.CancelAwaitingHumanRunStepParams{
		RunID: args.RunID, StepID: args.StepID, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusAwaitingHuman, to: StepStatusCancelled,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	// Close the open attempt (the attempt spanned the wait). Cancelled is an
	// administrative outcome — never counted against the retry budget.
	if err := finishAttempt(ctx, gq, op, step, AttemptOutcomeCancelled, nil, nil, nil, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DCancelled: 1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepCancelled, stepCancelledPayload{
		StepID: args.StepID, Reason: args.Reason,
	}); err != nil {
		return gen.RunStep{}, err
	}
	// Mark the pending approval cancelled. The pending guard makes this a
	// single-arbiter CAS: if a decide/timeout already moved it off pending
	// (impossible while the step is awaiting_human under the run lock, but
	// defensive for 15.3/15.4), it matches nothing and we skip the event.
	approval, aerr := gq.CancelPendingApprovalByStep(ctx, gen.CancelPendingApprovalByStepParams{
		RunID: args.RunID, StepID: args.StepID, Now: args.Now,
	})
	if errors.Is(aerr, pgx.ErrNoRows) {
		// No pending approval to cancel (already off pending) — the step
		// transition is authoritative; skip the approval event.
		return step, nil
	}
	if aerr != nil {
		return gen.RunStep{}, wrapErr(op+": cancel approval", aerr)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventApprovalCancelled, ApprovalCancelledEvent{
		ApprovalID: approval.ID.String(), StepID: args.StepID, Reason: args.Reason,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).InfoContext(ctx, "parked approval cancelled with run",
		log.RunID(args.RunID.String()), log.StepID(args.StepID))
	return step, nil
}

// ApprovalRepo reads the approvals table (ticket 15.2, ADR-017). Writes go
// through the transition-style helpers above (AwaitHumanStep,
// CancelAwaitingHumanStep) so the approval row, the step CAS, and the events
// are one atomic unit — like the cost ledger. These reads serve the
// run-status view (ListByRun) and the pending-approvals gauge (CountPending).
type ApprovalRepo interface {
	// Get reads one approval by id (15.3's decide path).
	Get(ctx context.Context, id uuid.UUID) (gen.Approval, error)
	// ListByRun returns a run's approvals, newest first.
	ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.Approval, error)
	// CountPending is the fleet-wide pending-approval count (the gauge source).
	CountPending(ctx context.Context) (int64, error)
}

type approvalRepo struct{ q *gen.Queries }

func (r approvalRepo) Get(ctx context.Context, id uuid.UUID) (gen.Approval, error) {
	a, err := r.q.GetApproval(ctx, id)
	return a, wrapErr("get approval", err)
}

func (r approvalRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.Approval, error) {
	rows, err := r.q.ListApprovalsByRun(ctx, runID)
	return rows, wrapErr("list approvals by run", err)
}

func (r approvalRepo) CountPending(ctx context.Context) (int64, error) {
	n, err := r.q.CountPendingApprovals(ctx)
	return n, wrapErr("count pending approvals", err)
}
