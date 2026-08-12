package api

// Per-step logs endpoint (ticket 7.4): GET /v1/runs/{id}/steps/{sid}/logs
// serves one attempt's captured executor log lines — the durable tee of
// exec.StepContext.Logger — as ascending-seq keyset pages, feeding M18's
// per-step log view. Poll-based by design (live streaming is a recorded
// non-goal until the M16 WS work revisits it).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Step-log pagination bounds: log lines are small and floods are the
// interesting case, so pages run larger than the run list's.
const (
	defaultLogPageLimit = 200
	maxLogPageLimit     = 1000
)

// handleStepLogs is GET /v1/runs/{id}/steps/{sid}/logs. Query parameters:
// attempt (1-based; default the step's latest), level (minimum severity;
// default debug = everything stored), cursor (opaque, from next_cursor),
// limit. The truncation marker is derived, not stored: any line that
// consumed a seq but is absent — ring-cap eviction or writer backpressure
// — surfaces in dropped_lines, computed as max(seq) − stored rows.
func (h *Handler) handleStepLogs(w http.ResponseWriter, r *http.Request) {
	runID, ok := h.runIDParam(w, r)
	if !ok {
		return
	}
	stepID := chi.URLParam(r, "stepID")
	ctx := r.Context()
	step, err := h.st.Steps().Get(ctx, runID, stepID)
	if errors.Is(err, store.ErrNotFound) {
		// Steps().Get reports both a missing run and a missing step as not
		// found; one read tells the client which (same shape as requeue).
		if _, runErr := h.st.Runs().Get(ctx, runID); errors.Is(runErr, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrorDetail{
				Code: ErrCodeRunNotFound, Message: "no run with id " + runID.String(),
			})
		} else {
			writeError(w, http.StatusNotFound, ErrorDetail{
				Code: ErrCodeStepNotFound, Message: "run has no step " + stepID,
			})
		}
		return
	}
	if err != nil {
		internalError(w, r, "reading run step", err)
		return
	}

	q := r.URL.Query()
	attempt := int(step.AttemptCount)
	if raw := q.Get("attempt"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "attempt must be a positive integer",
			})
			return
		}
		// An attempt beyond the step's history is not distinguishable from
		// one that logged nothing; both answer an empty page.
		attempt = n
	}
	minLevel := store.LogLevelDebug
	if raw := q.Get("level"); raw != "" {
		if !store.ValidLogLevel(raw) {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "level must be one of debug, info, warn, error",
			})
			return
		}
		minLevel = raw
	}
	limit, ok := logPageLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	afterSeq := int64(0)
	if raw := q.Get("cursor"); raw != "" {
		cur, err := decodeLogsCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "cursor is not a valid step-log cursor",
			})
			return
		}
		afterSeq = cur.Seq
	}

	resp := StepLogsResponse{
		RunID: runID.String(), StepID: stepID, Attempt: attempt,
		Lines: []StepLogLineView{},
	}
	if attempt >= 1 {
		// One extra row decides whether a next page exists.
		rows, err := h.st.StepLogs().ListPage(ctx, gen.ListStepLogsPageParams{
			RunID: runID, StepID: stepID,
			Attempt: int32(attempt), //nolint:gosec // bounded: parsed small or from an INT column
			Seq:     afterSeq, Levels: store.LogLevelsAtOrAbove(minLevel), Limit: limit + 1,
		})
		if err != nil {
			internalError(w, r, "listing step logs", err)
			return
		}
		if len(rows) > int(limit) {
			rows = rows[:limit]
			resp.NextCursor = encodeLogsCursor(logsCursor{Seq: rows[len(rows)-1].Seq})
		}
		for _, row := range rows {
			resp.Lines = append(resp.Lines, buildStepLogLineView(row))
		}
		stats, err := h.st.StepLogs().Stats(ctx, runID, stepID, int32(attempt)) //nolint:gosec // as above
		if err != nil {
			internalError(w, r, "reading step log stats", err)
			return
		}
		resp.DroppedLines = stats.MaxSeq - stats.Stored
		resp.Truncated = resp.DroppedLines > 0
	}
	writeJSON(w, http.StatusOK, resp)
}

// logPageLimit parses ?limit= against the step-log bounds, answering the
// 400 itself on a bad value.
func logPageLimit(w http.ResponseWriter, raw string) (int32, bool) {
	if raw == "" {
		return defaultLogPageLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLogPageLimit {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidRequest,
			Message: "limit must be an integer in [1, " + strconv.Itoa(maxLogPageLimit) + "]",
		})
		return 0, false
	}
	return int32(n), true //nolint:gosec // bounded by maxLogPageLimit
}

// logsCursor is the decoded step-log cursor: the previous page's last seq.
// Wire form is base64url(JSON) — opaque to clients, like the run list's.
type logsCursor struct {
	Seq int64 `json:"s"`
}

func encodeLogsCursor(c logsCursor) string {
	b, _ := json.Marshal(c) //nolint:errcheck // plain struct
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeLogsCursor(s string) (logsCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return logsCursor{}, err
	}
	var c logsCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return logsCursor{}, err
	}
	if c.Seq < 1 {
		return logsCursor{}, errors.New("cursor missing seq")
	}
	return c, nil
}

// buildStepLogLineView projects one step_logs row onto the wire.
func buildStepLogLineView(row gen.StepLog) StepLogLineView {
	v := StepLogLineView{
		Seq: row.Seq, Level: row.Level, Message: row.Message,
		Fields: row.Fields, LoggedAt: row.LoggedAt,
	}
	if row.TraceID != nil {
		v.TraceID = *row.TraceID
	}
	return v
}
