package api

// Cross-run dead-letter list (ticket 18.6): GET /v1/dead-letters serves the
// operator DLQ triage page — every dead-lettered step across runs, newest
// first, with its live step/run status so the operator can see what is still
// open and requeue it. The requeue action itself is the existing
// POST /v1/runs/{id}/steps/{sid}/requeue (ticket 6.5). Read-scoped; keyset
// paginated like GET /v1/approvals.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// dlqSources is the closed set of dead-letter sources a filter may name (the
// dead_letters.source CHECK). Naming anything else is a 400, not a silent
// empty page that could mask a typo.
var dlqSources = map[string]bool{
	"retries_exhausted": true,
	"permanent":         true,
	"poison":            true,
}

// handleListDeadLetters is GET /v1/dead-letters (ticket 18.6): one keyset page
// of dead-letter records across runs, newest first, with optional status
// (open|all), run_id, and source filters. The cursor is opaque; a page's
// next_cursor feeds the next request.
func (h *Handler) handleListDeadLetters(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// status defaults to "open" — the operator's working set (steps still
	// awaiting a requeue). "all" includes every historical death, including
	// steps that were later requeued and succeeded.
	args := store.DeadLetterPageArgs{OpenOnly: true}
	switch status := q.Get("status"); status {
	case "", "open":
		args.OpenOnly = true
	case "all":
		args.OpenOnly = false
	default:
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "status must be open or all: " + status,
		})
		return
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
	if source := q.Get("source"); source != "" {
		if !dlqSources[source] {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "source is not a dead-letter source: " + source,
			})
			return
		}
		args.Source = source
	}
	limit, ok := pageLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	if raw := q.Get("cursor"); raw != "" {
		cur, err := decodeDeadLetterCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "cursor is not a valid dead-letter-list cursor",
			})
			return
		}
		args.Cursor = &cur
	}
	// Fetch one extra to detect a next page.
	args.Limit = limit + 1

	rows, err := h.st.DeadLetters().ListPage(r.Context(), args)
	if err != nil {
		internalError(w, r, "listing dead letters", err)
		return
	}

	resp := DeadLetterListResponse{DeadLetters: []DeadLetterListItem{}}
	if len(rows) > int(limit) {
		last := rows[limit-1]
		resp.NextCursor = encodeDeadLetterCursor(store.DeadLetterCursor{
			CreatedAt: last.CreatedAt, RunID: last.RunID, StepID: last.StepID, Seq: last.Seq,
		})
		rows = rows[:limit]
	}
	for _, row := range rows {
		resp.DeadLetters = append(resp.DeadLetters, buildDeadLetterListItem(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildDeadLetterListItem projects one join row onto the wire. Open = the step
// is still dead_lettered at its latest death; the query's open filter already
// enforces this when status=open, but the flag is computed here too so an
// all-mode page reports it per row.
func buildDeadLetterListItem(row gen.ListDeadLettersPageRow) DeadLetterListItem {
	item := DeadLetterListItem{
		RunID:           row.RunID.String(),
		StepID:          row.StepID,
		StepType:        row.StepType,
		StepStatus:      row.StepStatus,
		RunStatus:       row.RunStatus,
		Seq:             int(row.Seq),
		Source:          row.Source,
		Error:           row.Error,
		AttemptsAtDeath: int(row.AttemptsAtDeath),
		Open:            row.StepStatus == "dead_lettered",
		CreatedAt:       row.CreatedAt,
	}
	if row.Class != nil {
		item.Class = *row.Class
	}
	if row.DefinitionID != nil {
		item.DefinitionID = row.DefinitionID.String()
	}
	return item
}

// deadLetterCursor is the wire form of a DLQ-list keyset position: the previous
// page's last row in list order (created_at DESC, run_id, step_id, seq).
// base64url(JSON), opaque to clients.
type deadLetterCursor struct {
	T   time.Time `json:"t"`
	Run uuid.UUID `json:"r"`
	Sid string    `json:"s"`
	Seq int32     `json:"q"`
}

func encodeDeadLetterCursor(c store.DeadLetterCursor) string {
	b, _ := json.Marshal(deadLetterCursor{T: c.CreatedAt, Run: c.RunID, Sid: c.StepID, Seq: c.Seq}) //nolint:errcheck // plain struct
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeDeadLetterCursor(s string) (store.DeadLetterCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.DeadLetterCursor{}, err
	}
	var c deadLetterCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return store.DeadLetterCursor{}, err
	}
	if c.T.IsZero() || c.Run == uuid.Nil || c.Sid == "" || c.Seq < 1 {
		return store.DeadLetterCursor{}, errors.New("cursor missing fields")
	}
	return store.DeadLetterCursor{CreatedAt: c.T, RunID: c.Run, StepID: c.Sid, Seq: c.Seq}, nil
}
