package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/notify"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// hostReporter is the optional interface a notifier implements to name its
// target host — the only part of a webhook URL safe to record on an event
// (the path or query could carry a token). *notify.Webhook satisfies it.
type hostReporter interface{ Host() string }

// notifierHost returns the notifier's target host, or "" when the notifier
// does not report one.
func notifierHost(n notify.Notifier) string {
	if h, ok := n.(hostReporter); ok {
		return h.Host()
	}
	return ""
}

// notifyApproval delivers a pending-approval notification through the
// configured notifier (ticket 15.5, ADR-017). It runs post-commit in the park
// path (completeAwaitHuman), after the approval row is durable, and is
// deliberately best-effort: nothing here returns an error to Handle, so a
// broken webhook can never un-ACK an already-committed park. Delivery is
// effectively-once — the call rides the 5.5 side-effect journal, so a repeat
// invocation for the same approval short-circuits without re-POSTing, and the
// notification carries the approval id as its delivery id for a receiver to
// dedupe the residual at-least-once window.
func (e *Engine) notifyApproval(ctx context.Context, step gen.RunStep, approval store.ApprovalRow, defName string) {
	logger := log.From(ctx)
	n := buildApprovalNotification(step, approval, defName, e.now())

	// The claim the executor held is diagnostics-only on the journal's intent
	// row (never fencing — a parked step holds no lease). It is still set on the
	// row passed into completeAwaitHuman.
	claimID := uuid.Nil
	if step.ClaimID != nil {
		claimID = *step.ClaimID
	}
	j := e.effects.ForStep(step.RunID, step.StepID, int(step.AttemptCount), claimID, logger)
	effectID := "approval_notify:" + approval.ID.String()

	in, err := j.Begin(ctx, effectID)
	if err != nil {
		logger.WarnContext(ctx, "approval notification journal begin failed; skipping delivery",
			slog.String("approval_id", approval.ID.String()), slog.Any("error", err))
		return
	}
	if in.Done() {
		// A prior invocation already delivered this notification — do not POST
		// again and do not re-emit the event.
		logger.DebugContext(ctx, "approval notification already delivered; skipping",
			slog.String("approval_id", approval.ID.String()))
		return
	}

	res, nerr := e.notifier.Notify(ctx, n)
	if nerr != nil {
		// The intent is left dangling (no result journaled). A parked step is
		// never re-claimed, so this simply means the notification was not
		// delivered — correctness is unaffected. Record a warning event + metric.
		e.recordApprovalNotification(ctx, step.RunID, store.EventApprovalNotificationFailed,
			store.ApprovalNotificationFailedEvent{
				ApprovalID: approval.ID.String(), StepID: step.StepID,
				TargetHost: notifierHost(e.notifier), Attempts: notifyAttempts(nerr),
				Reason: notifyFailureReason(nerr),
			})
		e.metrics.ApprovalNotified("failed")
		logger.WarnContext(ctx, "approval notification delivery failed; run stays parked and decidable",
			slog.String("approval_id", approval.ID.String()),
			slog.String("reason", notifyFailureReason(nerr)), slog.Any("error", nerr))
		return
	}
	if _, cerr := j.Complete(ctx, in, mustMarshalResult(res)); cerr != nil {
		// The delivery succeeded but its journal result could not be written
		// (racing completer already won, or a transport error). The
		// notification still went out, so record the delivered event; the only
		// consequence of an unwritten result is that a (nonexistent for a parked
		// step) re-invocation would not short-circuit.
		logger.WarnContext(ctx, "approval notification delivered but journal result unwritten",
			slog.String("approval_id", approval.ID.String()), slog.Any("error", cerr))
	}
	e.recordApprovalNotification(ctx, step.RunID, store.EventApprovalNotified,
		store.ApprovalNotifiedEvent{
			ApprovalID: approval.ID.String(), StepID: step.StepID,
			TargetHost: notifierHost(e.notifier), Attempts: res.Attempts, StatusCode: res.StatusCode,
		})
	e.metrics.ApprovalNotified("delivered")
	logger.InfoContext(ctx, "approval notification delivered",
		slog.String("approval_id", approval.ID.String()),
		slog.Int("attempts", res.Attempts), slog.Int("status", res.StatusCode))
}

// recordApprovalNotification appends a best-effort notification event. A
// failure only logs — the notification's audit line is not a correctness
// record, so it never blocks the park path.
func (e *Engine) recordApprovalNotification(ctx context.Context, runID uuid.UUID, typ string, payload any) {
	err := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		return store.RecordApprovalNotification(ctx, q, runID, typ, payload, e.now())
	})
	if err != nil {
		log.From(ctx).WarnContext(ctx, "recording approval notification event failed",
			slog.String("event", typ), slog.Any("error", err))
	}
}

// approvalRunName reads the run's definition name from its stored snapshot,
// for the notification's run.definition_name. Best-effort: an unreadable run
// or snapshot yields "" (the field is omitempty). Only called when a notifier
// is wired, so the extra PK read never touches the default park path.
func (e *Engine) approvalRunName(ctx context.Context, runID uuid.UUID) string {
	run, err := e.store.Runs().Get(ctx, runID)
	if err != nil {
		return ""
	}
	var def struct {
		Name string `json:"name"`
	}
	if uerr := json.Unmarshal(run.Definition, &def); uerr != nil {
		return ""
	}
	return def.Name
}

// buildApprovalNotification projects the store rows onto the wire notification
// (ticket 15.5). Pure — the golden test drives it directly. createdAt is the
// approval's park time (the row's created_at, which the engine stamped with
// this same clock).
func buildApprovalNotification(step gen.RunStep, a store.ApprovalRow, defName string, createdAt time.Time) notify.ApprovalNotification {
	runID := step.RunID.String()
	approvalID := a.ID.String()
	return notify.ApprovalNotification{
		SchemaVersion: notify.SchemaVersion,
		Event:         notify.EventApprovalRequested,
		Approval: notify.ApprovalInfo{
			ID:               approvalID,
			RunID:            runID,
			StepID:           step.StepID,
			Attempt:          step.AttemptCount,
			Title:            a.Title,
			Description:      a.Description,
			Payload:          a.Payload,
			AllowedDecisions: a.AllowedDecisions,
			AllowEdit:        a.AllowEdit,
			EditSchema:       a.EditSchema,
			TimeoutAt:        a.TimeoutAt,
			CreatedAt:        createdAt.UTC(),
		},
		Run: notify.RunInfo{ID: runID, DefinitionName: defName},
		Links: notify.Links{
			Approval: "/v1/approvals/" + approvalID,
			Decide:   "/v1/approvals/" + approvalID + ":decide",
			Run:      "/v1/runs/" + runID,
		},
	}
}

// mustMarshalResult marshals a delivery Result for the journal; a Result is
// two ints, so marshalling cannot fail.
func mustMarshalResult(r notify.Result) json.RawMessage {
	b, _ := json.Marshal(r)
	return b
}

// notifyFailureReason maps a delivery error to a short, secret-free reason.
func notifyFailureReason(err error) string {
	var ne *notify.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.As(err, &ne):
		if ne.Permanent {
			return "permanent"
		}
		return "retries_exhausted"
	default:
		return "error"
	}
}

// notifyAttempts extracts the attempt count from a delivery error, or 0.
func notifyAttempts(err error) int {
	var ne *notify.Error
	if errors.As(err, &ne) {
		return ne.Attempts
	}
	return 0
}
