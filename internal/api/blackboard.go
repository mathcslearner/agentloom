package api

// Blackboard endpoint (ticket 12.2, ADR-014): GET /v1/runs/{id}/blackboard
// serves the run-scoped blackboard — each key's head by default, or the full
// version history with ?history=true — as keyset pages, filterable by key
// and tag. Read-only, like the cost and log endpoints; the API never writes
// the blackboard (steps do, in the worker) — ADR-002 untouched.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Blackboard pagination bounds (entries are memory items, so pages run like
// the run list's, not the larger log pages).
const (
	defaultBlackboardPageLimit = 200
	maxBlackboardPageLimit     = 1000
)

// handleRunBlackboard is GET /v1/runs/{id}/blackboard. Query parameters:
// key (repeatable; restrict to these keys), tag (repeatable; heads/versions
// carrying every listed tag — AND), history (true = every version, not just
// heads), cursor (opaque, from next_cursor), limit.
func (h *Handler) handleRunBlackboard(w http.ResponseWriter, r *http.Request) {
	runID, ok := h.runIDParam(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := h.st.Runs().Get(ctx, runID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrorDetail{
				Code: ErrCodeRunNotFound, Message: "no run with id " + runID.String(),
			})
			return
		}
		internalError(w, r, "reading run", err)
		return
	}

	q := r.URL.Query()
	keys := q["key"]
	tags := q["tag"]
	history := q.Get("history") == "true"
	limit, ok := blackboardPageLimit(w, q.Get("limit"))
	if !ok {
		return
	}

	resp := BlackboardResponse{RunID: runID.String(), History: history, Entries: []BlackboardEntryView{}}

	if history {
		cur := blackboardCursor{}
		if raw := q.Get("cursor"); raw != "" {
			c, err := decodeBlackboardCursor(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, ErrorDetail{
					Code: ErrCodeInvalidRequest, Message: "cursor is not a valid blackboard cursor",
				})
				return
			}
			cur = c
		}
		rows, err := h.st.Blackboard().ListVersionsPage(ctx, gen.ListBlackboardVersionsPageParams{
			RunID: runID, Keys: keys, Tags: tags,
			AfterKey: cur.Key, AfterVersion: cur.Version, RowLimit: limit + 1,
		})
		if err != nil {
			internalError(w, r, "listing blackboard versions", err)
			return
		}
		if len(rows) > int(limit) {
			rows = rows[:limit]
			last := rows[len(rows)-1]
			resp.NextCursor = encodeBlackboardCursor(blackboardCursor{Key: last.Key, Version: last.Version})
		}
		for _, row := range rows {
			resp.Entries = append(resp.Entries, blackboardEntryView(row))
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	afterKey := ""
	if raw := q.Get("cursor"); raw != "" {
		c, err := decodeBlackboardCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "cursor is not a valid blackboard cursor",
			})
			return
		}
		afterKey = c.Key
	}
	rows, err := h.st.Blackboard().ListHeadsPage(ctx, gen.ListBlackboardHeadsPageParams{
		RunID: runID, AfterKey: afterKey, Keys: keys, Tags: tags, RowLimit: limit + 1,
	})
	if err != nil {
		internalError(w, r, "listing blackboard heads", err)
		return
	}
	if len(rows) > int(limit) {
		rows = rows[:limit]
		resp.NextCursor = encodeBlackboardCursor(blackboardCursor{Key: rows[len(rows)-1].Key})
	}
	for _, row := range rows {
		resp.Entries = append(resp.Entries, blackboardHeadView(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

// blackboardPageLimit parses ?limit= against the blackboard bounds.
func blackboardPageLimit(w http.ResponseWriter, raw string) (int32, bool) {
	if raw == "" {
		return defaultBlackboardPageLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxBlackboardPageLimit {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidRequest,
			Message: "limit must be an integer in [1, " + strconv.Itoa(maxBlackboardPageLimit) + "]",
		})
		return 0, false
	}
	return int32(n), true //nolint:gosec // bounded by maxBlackboardPageLimit
}

// blackboardCursor is the decoded keyset cursor: the previous page's last
// (key, version). Heads pages only use Key; history pages use both. Wire
// form is base64url(JSON), opaque to clients.
type blackboardCursor struct {
	Key     string `json:"k"`
	Version int32  `json:"v,omitempty"`
}

func encodeBlackboardCursor(c blackboardCursor) string {
	b, _ := json.Marshal(c) //nolint:errcheck // plain struct
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeBlackboardCursor(s string) (blackboardCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return blackboardCursor{}, err
	}
	var c blackboardCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return blackboardCursor{}, err
	}
	if c.Key == "" {
		return blackboardCursor{}, errors.New("cursor missing key")
	}
	return c, nil
}

func blackboardEntryView(row gen.BlackboardEntry) BlackboardEntryView {
	v := BlackboardEntryView{
		Key: row.Key, Version: int(row.Version), Value: row.Value,
		TokenCount: int(row.TokenCount), TokenCounter: row.TokenCounter,
		Tags: row.Tags, CreatedAt: row.CreatedAt,
	}
	if v.Tags == nil {
		v.Tags = []string{}
	}
	if row.AuthorStepID != nil {
		v.AuthorStepID = *row.AuthorStepID
	}
	if row.AuthorAttempt != nil {
		v.AuthorAttempt = int(*row.AuthorAttempt)
	}
	return v
}

// blackboardHeadView projects a DISTINCT ON heads-page row (a structurally
// identical but distinct generated type) onto the wire.
func blackboardHeadView(row gen.ListBlackboardHeadsPageRow) BlackboardEntryView {
	v := BlackboardEntryView{
		Key: row.Key, Version: int(row.Version), Value: row.Value,
		TokenCount: int(row.TokenCount), TokenCounter: row.TokenCounter,
		Tags: row.Tags, CreatedAt: row.CreatedAt,
	}
	if v.Tags == nil {
		v.Tags = []string{}
	}
	if row.AuthorStepID != nil {
		v.AuthorStepID = *row.AuthorStepID
	}
	if row.AuthorAttempt != nil {
		v.AuthorAttempt = int(*row.AuthorAttempt)
	}
	return v
}
