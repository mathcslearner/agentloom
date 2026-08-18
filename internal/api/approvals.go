package api

// Human-approval decision endpoints (ticket 15.3, ADR-017): the pending-
// approval inbox and the single decide endpoint. GET /v1/approvals lists
// approvals (read scope); POST /v1/approvals/{id}:decide resolves one (approve
// scope) through the engine's Control.Decide arbiter — approve/route succeed
// the parked step and dispatch its successors on the fleet's drain cadence
// (ADR-002 — the API holds no queue), reject-fail dead-letters it.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// handleListApprovals is GET /v1/approvals (ticket 15.3): one keyset page of
// approvals, oldest-first (the inbox order), with optional status and run_id
// filters. The cursor is opaque; a page's next_cursor feeds the next request.
func (h *Handler) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := store.ListApprovalsArgs{}

	if status := q.Get("status"); status != "" {
		if !validApprovalStatus(status) {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "status is not an approval status: " + status,
			})
			return
		}
		args.Status = status
	}
	if raw := q.Get("run_id"); raw != "" {
		runID, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "run_id is not a valid UUID",
			})
			return
		}
		args.RunID = &runID
	}
	limit, ok := pageLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	if raw := q.Get("cursor"); raw != "" {
		cur, err := decodeApprovalsCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "cursor is not a valid approval-list cursor",
			})
			return
		}
		args.AfterCreatedAt = &cur.T
		args.AfterID = &cur.ID
	}
	// Fetch one extra to detect a next page.
	args.Limit = limit + 1

	rows, err := h.st.Approvals().List(r.Context(), args)
	if err != nil {
		internalError(w, r, "listing approvals", err)
		return
	}

	resp := ApprovalListResponse{Approvals: []ApprovalView{}}
	if len(rows) > int(limit) {
		last := rows[limit-1]
		resp.NextCursor = encodeApprovalsCursor(approvalsCursor{T: last.CreatedAt, ID: last.ID})
		rows = rows[:limit]
	}
	for _, a := range rows {
		v := buildApprovalView(a)
		v.RunID = a.RunID.String()
		resp.Approvals = append(resp.Approvals, v)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDecideApproval is POST /v1/approvals/{id}:decide (ticket 15.3): decode
// the decision, attribute it to the authenticated key, and run the engine's
// Control.Decide arbiter. The typed errors map to 404 / 409 / 422; a success
// records the decision counter and returns the decided approval + run rollup.
func (h *Handler) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "approvalID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "approval id is not a valid UUID",
		})
		return
	}

	var body DecideApprovalRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "request body is not valid: " + err.Error(),
		})
		return
	}
	if !validApprovalDecision(body.Decision) {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "decision must be one of approve, reject",
		})
		return
	}
	if len(body.Comment) > maxApprovalCommentBytes {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "comment exceeds the maximum length",
		})
		return
	}

	// The actor is the authenticated key (root rides under "root"); requireScope
	// stamped the identity into the context.
	actor := rootKeyID
	if idn, ok := identityFrom(r.Context()); ok {
		actor = idn.keyID
	}

	res, err := h.ctl.Decide(r.Context(), id, engine.DecideRequest{
		Decision:      dag.ApprovalDecision(body.Decision),
		EditedPayload: body.EditedPayload,
		Comment:       body.Comment,
		DecidedBy:     actor,
		Source:        store.ApprovalSourceHuman,
	})
	if err != nil {
		h.writeDecideError(w, r, id, err)
		return
	}

	h.metrics.ApprovalDecided(body.Decision, store.ApprovalSourceHuman)

	// Re-read the run for the rollup after settlement (Control.Decide only
	// returns the run row when it terminalized it).
	run, gerr := h.st.Runs().Get(r.Context(), res.Approval.RunID)
	if gerr != nil {
		internalError(w, r, "reading run after decision", gerr)
		return
	}
	writeJSON(w, http.StatusOK, DecideApprovalResponse{
		Approval:     buildApprovalView(res.Approval),
		Run:          buildRunView(run),
		ReadiedSteps: orEmpty(res.Readied),
	})
}

// writeDecideError maps Control.Decide's typed errors onto the wire: an unknown
// approval is 404, a not-pending race 409, an invalid decision 422 (with the
// edit-schema issues), a wrong-run-state refusal 409, anything else a 500.
func (h *Handler) writeDecideError(w http.ResponseWriter, r *http.Request, id uuid.UUID, err error) {
	var notPending *store.ApprovalNotPendingError
	var invalid *engine.DecisionInvalidError
	var te *store.TransitionError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrorDetail{
			Code: ErrCodeApprovalNotFound, Message: "no approval with id " + id.String(),
		})
	case errors.As(err, &notPending):
		writeError(w, http.StatusConflict, ErrorDetail{
			Code:    ErrCodeApprovalNotPending,
			Message: "approval is not pending (status " + notPending.Status + ")",
		})
	case errors.As(err, &invalid):
		writeError(w, http.StatusUnprocessableEntity, ErrorDetail{
			Code: ErrCodeApprovalDecisionInvalid, Message: invalid.Message,
			Issues: decisionIssues(invalid.Issues),
		})
	case errors.As(err, &te):
		writeError(w, http.StatusConflict, ErrorDetail{Code: ErrCodeConflict, Message: te.Error()})
	default:
		internalError(w, r, "deciding approval", err)
	}
}

// decisionIssues maps engine edit-schema issues onto the wire issue shape.
func decisionIssues(in []engine.DecisionIssue) []Issue {
	if len(in) == 0 {
		return nil
	}
	out := make([]Issue, len(in))
	for i, is := range in {
		out[i] = Issue{Severity: "error", Path: is.Path, Msg: is.Message}
	}
	return out
}

// buildApprovalView projects an approvals row to its client-facing view. RunID
// is left empty (the run-status view scopes to one run); the approvals list
// sets it after building.
func buildApprovalView(a gen.Approval) ApprovalView {
	v := ApprovalView{
		ID:               a.ID.String(),
		StepID:           a.StepID,
		Attempt:          int(a.Attempt),
		Status:           a.Status,
		Title:            a.Title,
		Description:      a.Description,
		Payload:          a.Payload,
		AllowedDecisions: a.AllowedDecisions,
		AllowEdit:        a.AllowEdit,
		EditSchema:       a.EditSchema,
		TimeoutAt:        a.TimeoutAt,
		EditedPayload:    a.EditedPayload,
		DecidedAt:        a.DecidedAt,
		ExpiredAt:        a.ExpiredAt,
		CreatedAt:        a.CreatedAt,
	}
	if a.Decision != nil {
		v.Decision = *a.Decision
	}
	if a.Comment != nil {
		v.Comment = *a.Comment
	}
	if a.DecidedBy != nil {
		v.DecidedBy = *a.DecidedBy
	}
	if a.DecisionSource != nil {
		v.DecisionSource = *a.DecisionSource
	}
	return v
}

// maxApprovalCommentBytes caps an approver comment; it is stored verbatim in
// the audit trail and surfaced in run status.
const maxApprovalCommentBytes = 4096

// validApprovalStatus reports whether s is an approval-status filter value.
func validApprovalStatus(s string) bool {
	switch s {
	case store.ApprovalStatusPending, store.ApprovalStatusApproved,
		store.ApprovalStatusRejected, store.ApprovalStatusExpired,
		store.ApprovalStatusCancelled:
		return true
	}
	return false
}

// validApprovalDecision reports whether s is a decidable decision.
func validApprovalDecision(s string) bool {
	return s == string(dag.ApprovalApprove) || s == string(dag.ApprovalReject)
}

// approvalsCursor is the decoded approval-list cursor: the previous page's
// last row in list order (created_at ASC, id ASC). Wire form is
// base64url(JSON) — opaque to clients.
type approvalsCursor struct {
	T  time.Time `json:"t"`
	ID uuid.UUID `json:"id"`
}

func encodeApprovalsCursor(c approvalsCursor) string {
	b, _ := json.Marshal(c) //nolint:errcheck // plain struct
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeApprovalsCursor(s string) (approvalsCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return approvalsCursor{}, err
	}
	var c approvalsCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return approvalsCursor{}, err
	}
	if c.T.IsZero() || c.ID == uuid.Nil {
		return approvalsCursor{}, errors.New("cursor missing fields")
	}
	return c, nil
}
