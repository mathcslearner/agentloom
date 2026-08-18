// Package notify is the outbound approval-notification seam (ticket 15.5,
// ADR-017): when a human_approval step parks, the engine hands a rendered
// ApprovalNotification to a Notifier, which delivers it to an external
// endpoint (the built-in Webhook POSTs a signed payload). Delivery is
// best-effort — a notification is never a correctness dependency, so a
// notifier failure must never block a run — and effectively-once, achieved
// by the engine wrapping the Notify call in the 5.5 side-effect journal plus
// a per-notification delivery id the receiver dedupes on.
//
// The package is a leaf: it imports only the standard library, so the engine
// (and tests) can depend on it without a cycle. The engine builds the
// ApprovalNotification from its store rows; this package only carries the
// wire shape, signs it, and delivers it.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is the ApprovalNotification wire-contract version. It rides
// on every payload so a receiver (and the M18 inbox) can branch on shape.
const SchemaVersion = 1

// EventApprovalRequested is the notification event name: a new pending
// approval was created. It is the only event v1 emits.
const EventApprovalRequested = "approval.requested"

// ApprovalNotification is the payload delivered when a human_approval step
// parks. It mirrors the API's approval view so a receiver can render the
// pending decision and link straight to the decide endpoint, without reading
// the approvals table itself.
type ApprovalNotification struct {
	SchemaVersion int          `json:"schema_version"`
	Event         string       `json:"event"`
	Approval      ApprovalInfo `json:"approval"`
	Run           RunInfo      `json:"run"`
	Links         Links        `json:"links"`
}

// ApprovalInfo is the pending approval's rendered, snapshotted content.
type ApprovalInfo struct {
	ID               string          `json:"id"`
	RunID            string          `json:"run_id"`
	StepID           string          `json:"step_id"`
	Attempt          int32           `json:"attempt"`
	Title            string          `json:"title"`
	Description      string          `json:"description,omitempty"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	AllowedDecisions []string        `json:"allowed_decisions"`
	AllowEdit        bool            `json:"allow_edit,omitempty"`
	EditSchema       json.RawMessage `json:"edit_schema,omitempty"`
	// TimeoutAt is when the timeout policy fires; nil = wait indefinitely.
	TimeoutAt *time.Time `json:"timeout_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// RunInfo identifies the approval's run.
type RunInfo struct {
	ID             string `json:"id"`
	DefinitionName string `json:"definition_name,omitempty"`
}

// Links are relative API paths a receiver can resolve against its configured
// agentloom base URL to act on the approval.
type Links struct {
	Approval string `json:"approval"`
	Decide   string `json:"decide"`
	Run      string `json:"run"`
}

// DeliveryID is the receiver's dedupe key for this notification — the
// approval id, which is stable across the delivery's retries. It rides the
// X-Agentloom-Delivery-Id header so a receiver that sees the same id twice
// (the residual at-least-once window between the webhook POST and the
// journal's result commit) can drop the duplicate.
func (n ApprovalNotification) DeliveryID() string { return n.Approval.ID }

// Result reports a successful delivery: how many attempts it took and the
// final HTTP status.
type Result struct {
	Attempts   int
	StatusCode int
}

// Notifier delivers approval notifications. The built-in implementation is
// Webhook; tests substitute a recorder. Notify returns (Result, nil) on a
// delivered notification, a *notify.Error on a delivery failure (permanent or
// retries-exhausted), or the context's error unwrapped when the caller's
// context is cancelled (so the engine keeps the cancellation judgment).
type Notifier interface {
	Notify(ctx context.Context, n ApprovalNotification) (Result, error)
}

// Error is a delivery failure. Permanent reports whether retrying is futile
// (a 4xx other than 408/429); Attempts is how many were made; StatusCode is
// the last HTTP status seen (0 if the call never completed). It deliberately
// carries no headers or response body — a webhook URL may embed a token, so
// nothing that could leak a secret is retained.
type Error struct {
	Permanent  bool
	Attempts   int
	StatusCode int
	// Op describes the failing operation for the message; it never includes
	// the URL, the payload, or a header.
	Op string
}

func (e *Error) Error() string {
	kind := "transient"
	if e.Permanent {
		kind = "permanent"
	}
	return fmt.Sprintf("notify: %s webhook delivery failed (%s) after %d attempt(s), last status %d",
		e.Op, kind, e.Attempts, e.StatusCode)
}
