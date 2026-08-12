//go:build integration

package api_test

// Ticket 6.5's run-list contract tests: keyset pagination (including the
// acceptance criterion — stability under concurrent inserts), the
// status/definition/time-range filters, and parameter validation.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
)

// listRuns GETs /v1/runs with the given query string, asserting 200.
func listRuns(t *testing.T, srv *httptest.Server, key, query string) api.ListRunsResponse {
	t.Helper()
	var resp api.ListRunsResponse
	if status := getJSON(t, srv, key, "/v1/runs"+query, &resp); status != http.StatusOK {
		t.Fatalf("GET /v1/runs%s = %d, want 200", query, status)
	}
	return resp
}

func TestListRunsFilters(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	// Three inline runs; one is cancelled, one parked. Plus two runs of a
	// stored definition for the definition filter.
	var inline []string
	for range 3 {
		inline = append(inline, submitProbeRun(t, srv, key))
	}
	if status := postOp(t, srv, key, "/v1/runs/"+inline[0]+"/cancel", nil); status != http.StatusOK {
		t.Fatalf("cancel fixture = %d, want 200", status)
	}
	if status := postOp(t, srv, key, "/v1/runs/"+inline[1]+"/park", nil); status != http.StatusOK {
		t.Fatalf("park fixture = %d, want 200", status)
	}
	def := createDef(t, srv, key, defBody(t, "filter-def", "v1"))
	refBody, err := json.Marshal(api.SubmitRunRequest{DefinitionID: def.ID})
	if err != nil {
		t.Fatal(err)
	}
	byRef := map[string]bool{}
	for range 2 {
		var sub api.SubmitRunResponse
		if status := postJSON(t, srv, key, refBody, &sub); status != http.StatusCreated {
			t.Fatalf("submit by ref = %d, want 201", status)
		}
		byRef[sub.RunID] = true
	}

	// Status filter.
	cancelled := listRuns(t, srv, key, "?status=cancelled")
	if len(cancelled.Runs) != 1 || cancelled.Runs[0].ID != inline[0] {
		t.Errorf("status=cancelled = %+v, want exactly %s", cancelled.Runs, inline[0])
	}
	parked := listRuns(t, srv, key, "?status=parked")
	if len(parked.Runs) != 1 || parked.Runs[0].ID != inline[1] {
		t.Errorf("status=parked = %+v, want exactly %s", parked.Runs, inline[1])
	}
	running := listRuns(t, srv, key, "?status=running")
	if len(running.Runs) != 3 {
		t.Errorf("status=running returned %d runs, want 3", len(running.Runs))
	}

	// Definition filter.
	refs := listRuns(t, srv, key, "?definition_id="+def.ID)
	if len(refs.Runs) != 2 {
		t.Fatalf("definition filter returned %d runs, want 2", len(refs.Runs))
	}
	for _, r := range refs.Runs {
		if !byRef[r.ID] || r.DefinitionID != def.ID {
			t.Errorf("definition filter returned foreign run %+v", r)
		}
	}

	// Time-range filter around a mid-list boundary: created_at is DB time
	// (DEFAULT now()), so the boundary comes from the rows themselves. The
	// unfiltered list is newest-first; runs strictly newer than the middle
	// row's created_at are exactly those listed before it.
	all := listRuns(t, srv, key, "")
	if len(all.Runs) != 5 {
		t.Fatalf("unfiltered list = %d runs, want 5", len(all.Runs))
	}
	mid := all.Runs[2].CreatedAt
	after := listRuns(t, srv, key, "?created_after="+mid.Format(time.RFC3339Nano))
	if len(after.Runs) != 3 { // >= mid: the middle row itself plus the 2 newer
		t.Errorf("created_after=%s returned %d runs, want 3", mid.Format(time.RFC3339Nano), len(after.Runs))
	}
	before := listRuns(t, srv, key, "?created_before="+mid.Format(time.RFC3339Nano))
	if len(before.Runs) != 2 { // strictly older than mid
		t.Errorf("created_before returned %d runs, want 2", len(before.Runs))
	}

	// Newest-first ordering.
	for i := 1; i < len(all.Runs); i++ {
		if all.Runs[i].CreatedAt.After(all.Runs[i-1].CreatedAt) {
			t.Errorf("list not newest-first at index %d", i)
		}
	}
}

func TestListRunsParameterValidation(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	cases := []struct{ name, query string }{
		{"bad status", "?status=meditating"},
		{"bad definition_id", "?definition_id=not-a-uuid"},
		{"bad created_after", "?created_after=yesterday"},
		{"bad limit", "?limit=0"},
		{"limit over cap", "?limit=100000"},
		{"garbage cursor", "?cursor=@@@"},
		{"well-formed cursor, wrong shape", "?cursor=bm90LWpzb24"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var envelope api.ErrorBody
			res := doAuth(t, srv, http.MethodGet, "/v1/runs"+tc.query, key, nil, &envelope)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			if envelope.Error.Code != api.ErrCodeInvalidRequest {
				t.Errorf("code = %q, want invalid_request", envelope.Error.Code)
			}
		})
	}
}

// TestListRunsKeysetStableUnderConcurrentInserts is the ticket's
// acceptance criterion: a pagination walk started before a burst of new
// submissions sees every pre-existing run exactly once — no skips, no
// duplicates — because new rows sort strictly before any already-issued
// cursor position in (created_at DESC, id DESC) order.
func TestListRunsKeysetStableUnderConcurrentInserts(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	seeded := map[string]bool{}
	for range 12 {
		seeded[submitProbeRun(t, srv, key)] = true
	}

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		query := "?limit=4"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		page := listRuns(t, srv, key, query)
		for _, r := range page.Runs {
			seen[r.ID]++
		}
		pages++
		if pages > 20 {
			t.Fatal("pagination never terminated")
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		// The concurrent insert, between every page fetch: three new runs
		// that must not disturb the walk.
		for range 3 {
			submitProbeRun(t, srv, key)
		}
	}

	for id := range seeded {
		if seen[id] != 1 {
			t.Errorf("seeded run %s appeared %d times, want exactly once", id, seen[id])
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("run %s appeared %d times — keyset pagination duplicated a row", id, n)
		}
	}
}
