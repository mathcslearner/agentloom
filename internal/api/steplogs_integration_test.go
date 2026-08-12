//go:build integration

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Ticket 7.4's API suite: GET /v1/runs/{id}/steps/{sid}/logs over
// directly seeded step_logs rows (the tee itself is covered by the
// engine suite) — the pagination walk, attempt defaulting, the level
// filter, the derived truncation marker, and the 400/404 taxonomy.

// seedLogsRun submits a one-noop run through the API, claims its step
// (attempt_count 1 — the "latest attempt" the endpoint defaults to), and
// writes lines: seq 1..n cycling info/warn levels.
func seedLogsRun(t *testing.T, s *store.Store, srv *httptest.Server, key string, lines int) uuid.UUID {
	t.Helper()
	def := []byte(`{
		"schema_version": 1,
		"name": "steplog-api",
		"steps": [{"id": "only", "type": "noop"}],
		"edges": []
	}`)
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, key, submitBody(t, def, ""), &sub); status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}
	runID := uuid.MustParse(sub.RunID)
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: "only", Now: testNow})
		return err
	})
	if err != nil {
		t.Fatalf("claiming step: %v", err)
	}
	rows := make([]gen.CreateStepLogsParams, 0, lines)
	for seq := 1; seq <= lines; seq++ {
		level := store.LogLevelInfo
		if seq%5 == 0 {
			level = store.LogLevelWarn
		}
		trace := "0123456789abcdef0123456789abcdef"
		rows = append(rows, gen.CreateStepLogsParams{
			RunID: runID, StepID: "only", Attempt: 1, Seq: int64(seq),
			Level: level, Message: fmt.Sprintf("line %d", seq),
			TraceID: &trace, LoggedAt: testNow.Add(time.Duration(seq) * time.Millisecond),
		})
	}
	if _, err := s.StepLogs().CreateBatch(t.Context(), rows); err != nil {
		t.Fatalf("seeding step logs: %v", err)
	}
	return runID
}

func logsPath(runID uuid.UUID, query string) string {
	p := "/v1/runs/" + runID.String() + "/steps/only/logs"
	if query != "" {
		p += "?" + query
	}
	return p
}

func TestStepLogsEndpointPaginatesAndFilters(t *testing.T) {
	t.Parallel()
	s, srv, key := newServer(t)
	runID := seedLogsRun(t, s, srv, key, 25)

	// Keyset walk at limit 10: 10 + 10 + 5, cursors chaining, seq order.
	var all []api.StepLogLineView
	query := "limit=10"
	pages := 0
	for {
		var page api.StepLogsResponse
		if status := getJSON(t, srv, key, logsPath(runID, query), &page); status != http.StatusOK {
			t.Fatalf("GET logs page = %d, want 200", status)
		}
		if page.Attempt != 1 || page.Truncated || page.DroppedLines != 0 {
			t.Fatalf("page = %+v, want attempt 1 (defaulted to latest), nothing truncated", page)
		}
		all = append(all, page.Lines...)
		pages++
		if page.NextCursor == "" {
			break
		}
		query = "limit=10&cursor=" + page.NextCursor
	}
	if pages != 3 || len(all) != 25 {
		t.Fatalf("walk = %d pages, %d lines, want 3 pages of 25 lines", pages, len(all))
	}
	for i, line := range all {
		if line.Seq != int64(i+1) {
			t.Fatalf("walk out of order at %d: seq %d", i, line.Seq)
		}
	}
	if all[0].Message != "line 1" || all[0].Level != store.LogLevelInfo ||
		all[0].TraceID == "" || all[0].LoggedAt.IsZero() {
		t.Errorf("first line = %+v, want message/level/trace_id/logged_at populated", all[0])
	}

	// Minimum-level filter: warn+ returns exactly the five warn lines.
	var warns api.StepLogsResponse
	if status := getJSON(t, srv, key, logsPath(runID, "level=warn"), &warns); status != http.StatusOK {
		t.Fatalf("GET logs level=warn = %d, want 200", status)
	}
	if len(warns.Lines) != 5 {
		t.Fatalf("warn+ lines = %d, want 5", len(warns.Lines))
	}
	for _, line := range warns.Lines {
		if line.Level != store.LogLevelWarn {
			t.Errorf("warn+ filter returned level %q", line.Level)
		}
	}

	// Explicit ?attempt= of an unattempted number: empty page, not a 404.
	var empty api.StepLogsResponse
	if status := getJSON(t, srv, key, logsPath(runID, "attempt=7"), &empty); status != http.StatusOK {
		t.Fatalf("GET logs attempt=7 = %d, want 200", status)
	}
	if len(empty.Lines) != 0 || empty.Attempt != 7 || empty.NextCursor != "" {
		t.Errorf("unattempted page = %+v, want empty", empty)
	}
}

func TestStepLogsEndpointTruncationMarker(t *testing.T) {
	t.Parallel()
	s, srv, key := newServer(t)
	runID := seedLogsRun(t, s, srv, key, 20)

	// Simulate the ring cap having evicted the oldest 12 lines.
	if _, err := s.StepLogs().Trim(t.Context(), runID, "only", 1, 12); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	var page api.StepLogsResponse
	if status := getJSON(t, srv, key, logsPath(runID, ""), &page); status != http.StatusOK {
		t.Fatalf("GET logs = %d, want 200", status)
	}
	if !page.Truncated || page.DroppedLines != 12 {
		t.Errorf("truncation = (%v, %d), want (true, 12) — the derived marker", page.Truncated, page.DroppedLines)
	}
	if len(page.Lines) != 8 || page.Lines[0].Seq != 13 {
		t.Errorf("retained window = %d lines from seq %d, want the newest 8 from seq 13", len(page.Lines), page.Lines[0].Seq)
	}
}

func TestStepLogsEndpointErrors(t *testing.T) {
	t.Parallel()
	s, srv, key := newServer(t)
	runID := seedLogsRun(t, s, srv, key, 3)

	requireErr := func(path string, wantStatus int, wantCode string) {
		t.Helper()
		var body api.ErrorBody
		if status := getJSON(t, srv, key, path, &body); status != wantStatus {
			t.Fatalf("GET %s = %d, want %d", path, status, wantStatus)
		}
		if body.Error.Code != wantCode {
			t.Errorf("GET %s code = %q, want %q", path, body.Error.Code, wantCode)
		}
	}

	requireErr("/v1/runs/"+uuid.NewString()+"/steps/only/logs", http.StatusNotFound, api.ErrCodeRunNotFound)
	requireErr("/v1/runs/"+runID.String()+"/steps/ghost/logs", http.StatusNotFound, api.ErrCodeStepNotFound)
	requireErr("/v1/runs/not-a-uuid/steps/only/logs", http.StatusBadRequest, api.ErrCodeInvalidRequest)
	requireErr(logsPath(runID, "attempt=0"), http.StatusBadRequest, api.ErrCodeInvalidRequest)
	requireErr(logsPath(runID, "attempt=x"), http.StatusBadRequest, api.ErrCodeInvalidRequest)
	requireErr(logsPath(runID, "level=verbose"), http.StatusBadRequest, api.ErrCodeInvalidRequest)
	requireErr(logsPath(runID, "limit=0"), http.StatusBadRequest, api.ErrCodeInvalidRequest)
	requireErr(logsPath(runID, "limit=1001"), http.StatusBadRequest, api.ErrCodeInvalidRequest)
	requireErr(logsPath(runID, "cursor=garbage!"), http.StatusBadRequest, api.ErrCodeInvalidRequest)

	// A never-attempted step answers an empty page at attempt 0.
	def := []byte(`{
		"schema_version": 1,
		"name": "steplog-unattempted",
		"steps": [{"id": "a", "type": "noop"}, {"id": "b", "type": "noop"}],
		"edges": [{"from": "a", "to": "b"}]
	}`)
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, key, submitBody(t, def, ""), &sub); status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}
	var page api.StepLogsResponse
	path := "/v1/runs/" + sub.RunID + "/steps/b/logs"
	if status := getJSON(t, srv, key, path, &page); status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, status)
	}
	if page.Attempt != 0 || len(page.Lines) != 0 || page.Truncated {
		t.Errorf("unattempted step page = %+v, want attempt 0 and no lines", page)
	}
}
