package engine

// The decision API's engine op (ticket 15.3, ADR-017): Control.Decide is the
// single arbiter that resolves a parked human_approval step. It lives on
// Control (registry-free, no queue) so both the API server (a human decision)
// and 15.4's timeout policy (a system decision) drive the SAME compare-and-swap
// on the approvals row — "exactly one winner" holds by construction, not by
// careful ordering.
//
// The whole decision is one transaction: the DecideApproval CAS
// (pending → approved|rejected) is the fence, and in the same tx the parked
// step is settled — approve (or a reject routed via on_reject: route) succeeds
// with the ADR-017 decision output and fans out its successors through the
// transactional outbox; a reject under on_reject: fail dead-letters the step
// (ADR-006 row 24) and applies the run's on_failure disposition. Successors are
// dispatched on the fleet's drain cadence (Control holds no queue, ADR-002),
// exactly like Unpark/Requeue.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// DecideRequest is one decision on a pending approval.
type DecideRequest struct {
	// Decision is the operator's (or timeout policy's) choice.
	Decision dag.ApprovalDecision
	// EditedPayload, when non-nil, replaces the original payload as the step's
	// output payload (approve only, and only when the gate permits edits). Any
	// edit_schema constrains it.
	EditedPayload json.RawMessage
	// Comment is the optional approver note, recorded immutably.
	Comment string
	// DecidedBy is the actor: an API key_id for a human decision, or
	// store.ApprovalActorTimeout for a timeout decision.
	DecidedBy string
	// Source is store.ApprovalSourceHuman or store.ApprovalSourceTimeout.
	Source string
}

// DecideResult is what Decide did.
type DecideResult struct {
	// Approval is the decided approvals row (immutable decision recorded).
	Approval gen.Approval
	// Step is the parked step's row after settlement (succeeded or
	// dead_lettered).
	Step gen.RunStep
	// Readied lists the successors this decision made ready (approve / route).
	Readied []string
	// Skipped lists the successors written off (skip propagation).
	Skipped []string
	// RunFailed reports the run reached failed (a reject under on_reject: fail
	// with on_failure: fail_fast).
	RunFailed bool
	// RunTerminal is the run row when this decision terminalized it (succeeded
	// or failed), for run-completion metrics; nil otherwise.
	RunTerminal *gen.Run
}

// DecisionIssue is one edit-validation problem (RFC 6901 pointer + a
// structure-only message), surfaced to the caller as a 422 issue.
type DecisionIssue struct {
	Path    string
	Message string
}

// DecisionInvalidError is a semantically invalid decision — a decision outside
// the gate's allowed set, an edit where none is permitted, an edit on a
// non-approve decision, or an edited payload that violates the edit schema.
// The API maps it to 422 with the issues. Distinct from the wrong-run-state
// conflict (409) and the not-pending race (409). errors.As-able.
type DecisionInvalidError struct {
	Message string
	Issues  []DecisionIssue
}

func (e *DecisionInvalidError) Error() string { return "invalid decision: " + e.Message }

// Decide resolves a pending approval (ticket 15.3, ADR-017). Pre-transaction
// it loads the approval, its step and run, and validates the decision against
// the gate (a *DecisionInvalidError → 422). Then one transaction: gate on the
// run being live (running / parked — ADR-017 fail-fast note), CAS the approvals
// row (the arbiter — a loser gets *store.ApprovalNotPendingError → 409), settle
// the step, and route successors. Post-commit: an optional dispatch nudge.
func (c *Control) Decide(ctx context.Context, approvalID uuid.UUID, req DecideRequest) (DecideResult, error) {
	now := c.now()

	approval, err := c.store.Approvals().Get(ctx, approvalID)
	if err != nil {
		return DecideResult{}, err // store.ErrNotFound → 404
	}
	ctx = log.With(ctx, log.RunID(approval.RunID.String()), log.StepID(approval.StepID))

	// Fast-path the already-decided case so an obvious double-decide need not
	// take the run lock; the tx CAS is still the authority under a race.
	if approval.Status != store.ApprovalStatusPending {
		return DecideResult{}, &store.ApprovalNotPendingError{ID: approvalID, Status: approval.Status}
	}

	step, err := c.store.Steps().Get(ctx, approval.RunID, approval.StepID)
	if err != nil {
		return DecideResult{}, err
	}
	run, err := c.store.Runs().Get(ctx, approval.RunID)
	if err != nil {
		return DecideResult{}, err
	}

	// The reject-routing policy is static config (never templated), so decoding
	// the materialized step config is authoritative.
	onReject := dag.ApprovalRejectFail
	if cfg, derr := dag.DecodeStepConfig(dag.StepHumanApproval, step.Config); derr == nil {
		if hac, ok := cfg.(*dag.HumanApprovalConfig); ok && hac.OnReject != "" {
			onReject = hac.OnReject
		}
	}

	// Validate the decision against the gate (allowed set, edit permission,
	// edit schema). A failure is a client error (422), pre-CAS — nothing is
	// written. The allowed set and edit constraints come from the approvals
	// row (the resolved snapshot the approver saw).
	edited, invalid := c.validateDecision(ctx, approval, req)
	if invalid != nil {
		return DecideResult{}, invalid
	}

	// The ADR-017 decision output that becomes the step's result on
	// approve/route. payload is the edited value when supplied, else the
	// original snapshot.
	payload := approval.Payload
	if edited {
		payload = req.EditedPayload
	}
	output, merr := json.Marshal(decisionOutput{
		ApprovalID: approvalID.String(),
		Decision:   string(req.Decision),
		Payload:    payload,
		Edited:     edited,
		Comment:    req.Comment,
		DecidedBy:  req.DecidedBy,
		DecidedAt:  now.UTC().Format(time.RFC3339),
		Source:     req.Source,
	})
	if merr != nil {
		return DecideResult{}, fmt.Errorf("engine: Decide: marshaling decision output: %w", merr)
	}

	// route reports whether a rejection routes to reject edges (vs dead-letter).
	route := req.Decision == dag.ApprovalReject && onReject == dag.ApprovalRejectRoute
	// deadLetter reports a reject that dead-letters the step.
	deadLetter := req.Decision == dag.ApprovalReject && onReject == dag.ApprovalRejectFail

	var params map[string]any
	if len(run.Params) > 0 {
		if perr := json.Unmarshal(run.Params, &params); perr != nil {
			// Params were validated JSON at submission; a decode miss now is
			// corrupt stored state — surface it rather than silently dropping
			// edge predicates that read run.params.
			return DecideResult{}, fmt.Errorf("engine: Decide: decoding run params: %w", perr)
		}
	}

	var res DecideResult
	var notPending *store.ApprovalNotPendingError
	txErr := c.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		// Fail-fast gate (ADR-017): a run may be failed with an approval still
		// pending (a fail_fast dead-letter elsewhere stranded this gate). Only a
		// running or parked run accepts a decision; anything else is a 409.
		status, serr := store.LockRunStatus(ctx, q, approval.RunID, now)
		if serr != nil {
			return serr
		}
		if status != store.RunStatusRunning && status != store.RunStatusParked {
			return &store.TransitionError{
				Entity: "run", RunID: approval.RunID,
				From: status, To: "decided", Reason: store.ConflictRunNotRunning,
			}
		}

		// The arbiter CAS: pending → approved|rejected (a human or timeout
		// decision), or pending → expired (a timeout reject/approve policy —
		// ticket 15.4, so the audit trail separates an automatic expiry from an
		// operator's decision). A loser matches nothing and gets
		// *ApprovalNotPendingError (rolls the tx back — nothing else written).
		target := store.ApprovalStatusApproved
		if req.Decision == dag.ApprovalReject {
			target = store.ApprovalStatusRejected
		}
		var expiredAt *time.Time
		if req.Source == store.ApprovalSourceTimeout {
			target = store.ApprovalStatusExpired
			expiredAt = &now
		}
		decided, derr := store.DecideApproval(ctx, q, store.DecideApprovalArgs{
			ID: approvalID, RunID: approval.RunID, StepID: approval.StepID,
			AttemptNo: approval.Attempt, Status: target, Decision: string(req.Decision),
			EditedPayload: req.EditedPayload, Comment: req.Comment,
			DecidedBy: req.DecidedBy, Source: req.Source,
			ExpiredAt: expiredAt, TimeoutAt: approval.TimeoutAt, Now: now,
		})
		if derr != nil {
			errors.As(derr, &notPending)
			return derr
		}
		res.Approval = decided

		if deadLetter {
			return c.decideDeadLetter(ctx, q, step, run, req.Comment, now, &res)
		}
		return c.decideSucceed(ctx, q, step, output, req.Decision, params, now, &res)
	})
	if txErr != nil {
		if notPending != nil {
			return DecideResult{}, notPending
		}
		return DecideResult{}, txErr
	}

	// Early-decision cleanup (ticket 15.4): a human decision on an approval
	// that had a scheduled timeout best-effort ZREMs the pending expiry so it
	// never fires. Best-effort by design — the approvals CAS above is the
	// authority, so a failed or missed Cancel only means one stale expiry
	// fires later and no-ops. A timeout-sourced decision skips this (the expiry
	// is the thing firing).
	if req.Source == store.ApprovalSourceHuman && c.canceller != nil && approval.TimeoutAt != nil {
		env := approvalTimeoutEnvelope(approval.RunID, approval.StepID,
			store.TraceContext{Parent: deref(run.TraceParent), State: deref(run.TraceState)})
		if cerr := c.canceller.Cancel(ctx, env); cerr != nil {
			log.From(ctx).WarnContext(ctx, "failed to cancel approval expiry; a stale expiry will fire and no-op",
				slog.String("approval_id", approvalID.String()), slog.Any("error", cerr))
		}
	}

	log.From(ctx).InfoContext(ctx, "approval decided",
		slog.String("approval_id", approvalID.String()),
		slog.String("decision", string(req.Decision)),
		slog.String("source", req.Source),
		slog.Bool("edited", edited),
		slog.Bool("reject_routed", route),
		slog.Int("steps_readied", len(res.Readied)),
		slog.Bool("run_failed", res.RunFailed))
	if len(res.Readied) > 0 && c.nudge != nil {
		c.nudge()
	}
	return res, nil
}

// deref returns the string value of a *string, or "" for nil — used to project
// a run row's nullable trace columns onto a store.TraceContext.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// decisionOutput is the ADR-017 step output an approve/route decision writes;
// downstream steps read ${{ steps.<gate>.output.payload }} and may branch on
// output.decision.
type decisionOutput struct {
	ApprovalID string          `json:"approval_id"`
	Decision   string          `json:"decision"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Edited     bool            `json:"edited"`
	Comment    string          `json:"comment,omitempty"`
	DecidedBy  string          `json:"decided_by"`
	DecidedAt  string          `json:"decided_at"`
	Source     string          `json:"source"`
}

// decideSucceed settles the step succeeded (approve, or reject under route) and
// fans out the decision-appropriate successors, writing outbox rows for the
// newly-ready ones — all in the caller's transaction.
func (c *Control) decideSucceed(ctx context.Context, q store.Querier, step gen.RunStep, output json.RawMessage, decision dag.ApprovalDecision, params map[string]any, now time.Time, res *DecideResult) error {
	settled, err := store.SucceedAwaitingHumanStep(ctx, q, store.SucceedAwaitingHumanStepArgs{
		RunID: step.RunID, StepID: step.StepID, Output: output, Now: now,
	})
	if err != nil {
		return err
	}
	res.Step = settled

	edges, err := q.Steps().ListEdgesFromStep(ctx, step.RunID, step.StepID)
	if err != nil {
		return err
	}
	verdicts, err := planEdges(step.StepType, edges, output, params)
	if err != nil {
		// A CEL `when` failure on an approval out-edge: deterministic step-level
		// failure (ADR-003). The tx rolls back; the approval CAS undoes with it.
		return err
	}
	// Restrict the fired edges to those matching the recorded decision (ADR-017
	// decision edge marker): on approve, unmarked and approve-marked edges fire;
	// on reject-route, only reject-marked edges fire (unmarked = the approve
	// path, skipped).
	verdicts = filterDecisionVerdicts(edges, decision, verdicts)

	fanned, err := fanOut(ctx, q, step.RunID, now, terminalSource{stepID: step.StepID, verdicts: verdicts})
	if err != nil {
		return err
	}
	res.Readied = fanned.readied
	res.Skipped = fanned.skipped
	for _, id := range fanned.readied {
		if _, err := q.Outbox().Create(ctx, step.RunID, id, store.OutboxReasonStepReady); err != nil {
			return err
		}
	}
	terminalRun, err := attemptRunRollup(ctx, q, step.RunID, now)
	if err != nil {
		return err
	}
	res.RunTerminal = terminalRun
	return nil
}

// decideDeadLetter settles the step dead_lettered (reject under on_reject:
// fail, ADR-006 row 24) and applies the run's on_failure disposition — all in
// the caller's transaction.
func (c *Control) decideDeadLetter(ctx context.Context, q store.Querier, step gen.RunStep, run gen.Run, comment string, now time.Time, res *DecideResult) error {
	msg := "approval_rejected"
	if comment != "" {
		msg = "approval_rejected: " + comment
	}
	payload, err := json.Marshal(map[string]string{"message": msg, "class": string(dag.ClassPermanent)})
	if err != nil {
		payload = json.RawMessage(`{"message":"approval_rejected","class":"permanent"}`)
	}
	settled, err := store.DeadLetterAwaitingHumanStep(ctx, q, store.DeadLetterAwaitingHumanStepArgs{
		RunID: step.RunID, StepID: step.StepID, Error: payload, Now: now,
	})
	if err != nil {
		return err
	}
	res.Step = settled

	// The on_failure disposition: fail_fast → FailRun; continue → write off the
	// blocked descendants and roll up. run.OnFailure is immutable run state.
	cancelled, runFailed, derr := deadLetterDisposition(ctx, q, run.OnFailure, step.RunID, now)
	if derr != nil {
		return derr
	}
	res.Skipped = cancelled
	res.RunFailed = runFailed
	if runFailed {
		// FailRun terminalized the run; read it back for completion metrics.
		terminalRun, gerr := q.Runs().Get(ctx, step.RunID)
		if gerr != nil {
			return gerr
		}
		res.RunTerminal = &terminalRun
	}
	return nil
}

// validateDecision checks a decision against the gate (ticket 15.3). It returns
// whether an edited payload applies, and a *DecisionInvalidError (mapped to 422)
// when the decision is not permitted. The allowed set and edit constraints come
// from the approvals row — the resolved snapshot the approver saw.
func (c *Control) validateDecision(ctx context.Context, approval gen.Approval, req DecideRequest) (edited bool, invalid *DecisionInvalidError) {
	if !containsDecision(approval.AllowedDecisions, string(req.Decision)) {
		return false, &DecisionInvalidError{
			Message: fmt.Sprintf("decision %q is not in the gate's allowed set %v", req.Decision, approval.AllowedDecisions),
		}
	}
	if len(req.EditedPayload) == 0 {
		return false, nil
	}
	// An edit is supplied.
	if req.Decision != dag.ApprovalApprove {
		return false, &DecisionInvalidError{Message: "an edited payload may only accompany an approve decision"}
	}
	if !approval.AllowEdit {
		return false, &DecisionInvalidError{Message: "this approval does not permit edits"}
	}
	if len(approval.EditSchema) > 0 {
		if issues := validateEditSchema(ctx, approval.EditSchema, req.EditedPayload); len(issues) > 0 {
			return false, &DecisionInvalidError{
				Message: "edited payload does not satisfy the edit schema",
				Issues:  issues,
			}
		}
	}
	return true, nil
}

// validateEditSchema checks an edited payload against the gate's edit schema,
// reusing the built-in json_schema validator so the issue flattening is the
// same tested mapping the validate stage uses. A compile failure (impossible —
// 15.2 pre-compiled at park) surfaces as a single issue rather than a panic.
func validateEditSchema(ctx context.Context, schema, payload json.RawMessage) []DecisionIssue {
	cfg, err := json.Marshal(struct {
		Schema json.RawMessage `json:"schema"`
	}{Schema: schema})
	if err != nil {
		return []DecisionIssue{{Message: "edit schema is not encodable"}}
	}
	verdict, verr := validate.NewJSONSchema().Validate(ctx, validate.Input{
		StepType: dag.StepHumanApproval, Output: payload, Value: payload, Config: cfg,
	})
	if verr != nil {
		return []DecisionIssue{{Message: "edit schema could not be applied: " + verr.Error()}}
	}
	if verdict.Passed() {
		return nil
	}
	issues := make([]DecisionIssue, 0, len(verdict.Issues))
	for _, i := range verdict.Issues {
		issues = append(issues, DecisionIssue{Path: i.Path, Message: i.Message})
	}
	if len(issues) == 0 {
		issues = append(issues, DecisionIssue{Message: "edited payload does not satisfy the edit schema"})
	}
	return issues
}

// filterDecisionVerdicts narrows a set of edge verdicts to those whose decision
// marker matches the recorded decision (ADR-017): an approve fires unmarked and
// approve-marked edges; a reject fires only reject-marked edges (an unmarked
// edge is the approve/success path, skipped on reject). Edges that no longer
// match are forced to skipped, propagating through the ordinary fan-out.
func filterDecisionVerdicts(edges []gen.RunEdge, decision dag.ApprovalDecision, verdicts []edgeVerdict) []edgeVerdict {
	marker := make(map[int32]*string, len(edges))
	for _, e := range edges {
		if e.EdgeType == store.EdgeTypeNormal {
			marker[e.Ordinal] = e.Decision
		}
	}
	out := make([]edgeVerdict, len(verdicts))
	for i, v := range verdicts {
		out[i] = edgeVerdict{ordinal: v.ordinal, fired: v.fired && edgeMatchesDecision(marker[v.ordinal], decision)}
	}
	return out
}

// edgeMatchesDecision reports whether an edge with the given decision marker
// (nil = unmarked) fires under the recorded decision.
func edgeMatchesDecision(mark *string, decision dag.ApprovalDecision) bool {
	switch decision {
	case dag.ApprovalApprove:
		return mark == nil || *mark == string(dag.ApprovalApprove)
	case dag.ApprovalReject:
		return mark != nil && *mark == string(dag.ApprovalReject)
	}
	return false
}

func containsDecision(set []string, d string) bool {
	for _, s := range set {
		if s == d {
			return true
		}
	}
	return false
}
