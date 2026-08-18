package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ApprovalDecidedEvent is the approval_decided event payload (ticket 15.3,
// ADR-017): a pending approval was decided through the single arbiter CAS.
// The M16 event feed and the M18 inbox read it to reflect a decision without
// re-reading the approvals table.
type ApprovalDecidedEvent struct {
	ApprovalID string `json:"approval_id"`
	StepID     string `json:"step_id"`
	AttemptNo  int32  `json:"attempt"`
	// Decision is the recorded decision (approve / reject).
	Decision string `json:"decision"`
	// Edited reports whether an edited payload replaced the original.
	Edited bool `json:"edited,omitempty"`
	// Comment is the optional approver note.
	Comment string `json:"comment,omitempty"`
	// DecidedBy is the actor: a key_id for a human decision, or the reserved
	// ApprovalActorTimeout for a timeout decision (15.4).
	DecidedBy string `json:"decided_by"`
	// Source is `human` or `timeout` (ApprovalSource*).
	Source string `json:"source"`
}

// ApprovalNotPendingError is returned by DecideApproval when the arbiter CAS
// matched no pending row: the approval was already decided, expired, or
// cancelled (a concurrent decide or a timeout expiry won the race). Status is
// the actual current status, surfaced to the caller as a 409. errors.As-able.
type ApprovalNotPendingError struct {
	ID     uuid.UUID
	Status string
}

func (e *ApprovalNotPendingError) Error() string {
	return "approval " + e.ID.String() + " is not pending (status " + e.Status + ")"
}

// DecideApprovalArgs are the inputs to DecideApproval.
type DecideApprovalArgs struct {
	// ID is the approval to decide.
	ID uuid.UUID
	// RunID is the approval's run (for the run lock and the event append).
	RunID uuid.UUID
	// StepID / AttemptNo attribute the approval to its parked step's open
	// attempt (for the event payload).
	StepID    string
	AttemptNo int32
	// Status is the target approval status: ApprovalStatusApproved or
	// ApprovalStatusRejected.
	Status string
	// Decision is the recorded decision string (approve / reject).
	Decision string
	// EditedPayload is the approver-supplied edited payload (nil = none / not
	// edited). Written verbatim onto the approvals row for audit; the step's
	// output payload is chosen by the caller (engine).
	EditedPayload json.RawMessage
	// Comment is the optional approver note; empty stores NULL.
	Comment string
	// DecidedBy is the actor's key_id (or ApprovalActorTimeout for a timeout).
	DecidedBy string
	// Source is ApprovalSourceHuman or ApprovalSourceTimeout.
	Source string
	// Now is the injected current time. Required.
	Now time.Time
}

// DecideApproval is the single-arbiter CAS of the decision API (ticket 15.3,
// ADR-017): under the run lock it CASes the approvals row
// pending → approved|rejected, keyed by id with a pending guard, and appends
// the approval_decided event. It writes ONLY the approvals row and the event
// — the caller settles the parked step (succeed / dead-letter) in the same
// transaction, so the whole decision is atomic. A CAS that matches nothing
// (already decided / expired / cancelled) returns a *ApprovalNotPendingError,
// which the caller turns into a 409; the transaction rolls back and nothing
// is written (the loser of the race changes nothing).
func DecideApproval(ctx context.Context, q Querier, args DecideApprovalArgs) (gen.Approval, error) {
	const op = "decide approval"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.Approval{}, err
	}
	if args.Status != ApprovalStatusApproved && args.Status != ApprovalStatusRejected {
		return gen.Approval{}, fmt.Errorf("store: %s: status %q is not a decision status", op, args.Status)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.Approval{}, err
	}
	var comment *string
	if args.Comment != "" {
		comment = &args.Comment
	}
	decision := args.Decision
	decidedBy := args.DecidedBy
	source := args.Source
	approval, err := gq.DecideApproval(ctx, gen.DecideApprovalParams{
		ID: args.ID, Status: args.Status, Decision: &decision,
		EditedPayload: args.EditedPayload, Comment: comment,
		DecidedBy: &decidedBy, DecisionSource: &source, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The CAS matched nothing — re-read to name the actual status.
		cur, gerr := gq.GetApproval(ctx, args.ID)
		if errors.Is(gerr, pgx.ErrNoRows) {
			return gen.Approval{}, wrapErr(op, gerr) // vanished (ErrNotFound)
		}
		if gerr != nil {
			return gen.Approval{}, wrapErr(op, gerr)
		}
		return gen.Approval{}, &ApprovalNotPendingError{ID: args.ID, Status: cur.Status}
	}
	if err != nil {
		return gen.Approval{}, wrapErr(op, err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventApprovalDecided, ApprovalDecidedEvent{
		ApprovalID: args.ID.String(), StepID: args.StepID, AttemptNo: args.AttemptNo,
		Decision: args.Decision, Edited: len(args.EditedPayload) > 0,
		Comment: args.Comment, DecidedBy: args.DecidedBy, Source: args.Source,
	}); err != nil {
		return gen.Approval{}, err
	}
	return approval, nil
}

// SucceedAwaitingHumanStepArgs are the inputs to SucceedAwaitingHumanStep.
type SucceedAwaitingHumanStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Output is the ADR-017 decision output that becomes the step's result —
	// the {approval_id, decision, payload, ...} object downstream steps read.
	Output json.RawMessage
	// Now is the injected current time. Required.
	Now time.Time
}

// SucceedAwaitingHumanStep settles a parked approval step on an approve (or a
// reject routed via on_reject: route) — awaiting_human → succeeded (ticket
// 15.3, ADR-017). It closes the open attempt (which spanned the human wait)
// succeeded, bumps steps_succeeded, and appends step_succeeded. No claim
// fence — the step holds no lease while parked; the DecideApproval CAS the
// caller ran first is the single arbiter, so a second decision cannot reach
// here. The out-edge fan-out is the caller's next move in the same tx.
func SucceedAwaitingHumanStep(ctx context.Context, q Querier, args SucceedAwaitingHumanStepArgs) (gen.RunStep, error) {
	const op = "succeed awaiting human step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.SucceedAwaitingHumanRunStep(ctx, gen.SucceedAwaitingHumanRunStepParams{
		RunID: args.RunID, StepID: args.StepID, Output: args.Output, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusAwaitingHuman, to: StepStatusSucceeded,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	if err := finishAttempt(ctx, gq, op, step, StepStatusSucceeded, nil, nil, nil, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DSucceeded: 1}); err != nil {
		return gen.RunStep{}, err
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepSucceeded, stepFinishedPayload{
		StepID: args.StepID, AttemptNo: step.AttemptCount,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).DebugContext(ctx, "approval step resolved: succeeded",
		log.RunID(args.RunID.String()), log.StepID(args.StepID), log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// DeadLetterAwaitingHumanStepArgs are the inputs to DeadLetterAwaitingHumanStep.
type DeadLetterAwaitingHumanStepArgs struct {
	RunID  uuid.UUID
	StepID string
	// Error is the approval_rejected failure summary stored on the step, the
	// attempt, and the dead_letters record.
	Error json.RawMessage
	// Now is the injected current time. Required.
	Now time.Time
}

// DeadLetterAwaitingHumanStep settles a parked approval step on a reject under
// on_reject: fail — awaiting_human → dead_lettered (ticket 15.3, ADR-006 row
// 24). It closes the open attempt with the administrative outcome `permanent`
// (a reject is not a content failure of the executor — no retry, no budget
// consumption), bumps steps_failed, inserts the dead_letters record (source
// permanent), and appends step_dead_lettered. No claim fence (no lease held);
// the DecideApproval CAS is the arbiter. The run disposition (FailRun or the
// write-off walk) is the caller's next move in the same transaction.
func DeadLetterAwaitingHumanStep(ctx context.Context, q Querier, args DeadLetterAwaitingHumanStepArgs) (gen.RunStep, error) {
	const op = "dead-letter awaiting human step"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.RunStep{}, err
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.RunStep{}, err
	}
	step, err := gq.DeadLetterAwaitingHumanRunStep(ctx, gen.DeadLetterAwaitingHumanRunStepParams{
		RunID: args.RunID, StepID: args.StepID, Error: args.Error, Now: args.Now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.RunStep{}, stepConflict(ctx, gq, op, args.RunID, args.StepID, stepConflictArgs{
			want: StepStatusAwaitingHuman, to: StepStatusDeadLettered,
		})
	}
	if err != nil {
		return gen.RunStep{}, wrapErr(op, err)
	}
	// A reject is a permanent disposition but not a retryable-class failure —
	// record the attempt outcome as permanent (ADR-006 row 24).
	class := AttemptOutcomePermanent
	if err := finishAttempt(ctx, gq, op, step, class, args.Error, nil, nil, nil, args.Now); err != nil {
		return gen.RunStep{}, err
	}
	if err := bumpCounters(ctx, gq, op, args.RunID, gen.BumpRunStepCountersParams{DFailed: 1}); err != nil {
		return gen.RunStep{}, err
	}
	dl, err := gq.CreateDeadLetter(ctx, gen.CreateDeadLetterParams{
		RunID: args.RunID, StepID: args.StepID, Source: DeadLetterSourcePermanent, Class: &class,
		Error: args.Error, AttemptsAtDeath: step.AttemptCount, CreatedAt: args.Now,
	})
	if err != nil {
		return gen.RunStep{}, wrapErr(op+": insert dead letter", err)
	}
	if err := appendEvent(ctx, gq, op, args.RunID, EventStepDeadLettered, stepDeadLetteredPayload{
		StepID: args.StepID, Source: DeadLetterSourcePermanent, Class: class,
		Attempts: step.AttemptCount, Seq: dl.Seq,
	}); err != nil {
		return gen.RunStep{}, err
	}
	log.From(ctx).InfoContext(ctx, "approval step resolved: rejected → dead-lettered",
		log.RunID(args.RunID.String()), log.StepID(args.StepID), log.Attempt(int(step.AttemptCount)))
	return step, nil
}

// ListApprovalsArgs parameterize ApprovalRepo.List (ticket 15.3).
type ListApprovalsArgs struct {
	// Status, when non-empty, filters to one approval status.
	Status string
	// RunID, when non-nil, filters to one run.
	RunID *uuid.UUID
	// AfterCreatedAt / AfterID are the keyset cursor (nil = first page).
	AfterCreatedAt *time.Time
	AfterID        *uuid.UUID
	// Limit is the max rows to return (the handler passes limit+1 to detect a
	// next page).
	Limit int32
}

// ApprovalRepo reads the approvals table (ticket 15.2/15.3, ADR-017). Writes
// go through the transition-style helpers above (AwaitHumanStep,
// CancelAwaitingHumanStep, DecideApproval, SucceedAwaitingHumanStep,
// DeadLetterAwaitingHumanStep) so the approval row, the step CAS, and the
// events are one atomic unit — like the cost ledger. These reads serve the
// run-status view (ListByRun), the decision API list (List), and the
// pending-approvals gauge (CountPending).
type ApprovalRepo interface {
	// Get reads one approval by id (15.3's decide path).
	Get(ctx context.Context, id uuid.UUID) (gen.Approval, error)
	// ListByRun returns a run's approvals, newest first.
	ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.Approval, error)
	// List returns approvals filtered by status/run, keyset paginated
	// oldest-first (the GET /v1/approvals inbox).
	List(ctx context.Context, args ListApprovalsArgs) ([]gen.Approval, error)
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

func (r approvalRepo) List(ctx context.Context, args ListApprovalsArgs) ([]gen.Approval, error) {
	var status *string
	if args.Status != "" {
		status = &args.Status
	}
	rows, err := r.q.ListApprovals(ctx, gen.ListApprovalsParams{
		Status: status, RunID: args.RunID,
		AfterCreatedAt: args.AfterCreatedAt, AfterID: args.AfterID,
		RowLimit: args.Limit,
	})
	return rows, wrapErr("list approvals", err)
}

func (r approvalRepo) CountPending(ctx context.Context) (int64, error) {
	n, err := r.q.CountPendingApprovals(ctx)
	return n, wrapErr("count pending approvals", err)
}
