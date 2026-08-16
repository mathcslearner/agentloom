//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 12.2's API suite: GET /v1/runs/{id}/blackboard over directly seeded
// blackboard_entries (the write paths are covered by the engine/store
// suites) — heads vs. history, the key and tag filters, pagination, and the
// 400/404 taxonomy.

// seedBlackboardRun submits a one-noop run and writes blackboard entries.
func seedBlackboardRun(t *testing.T, s *store.Store, srv *httptest.Server, key string) uuid.UUID {
	t.Helper()
	def := []byte(`{
		"schema_version": 1,
		"name": "blackboard-api",
		"steps": [{"id": "only", "type": "noop"}],
		"edges": []
	}`)
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, key, submitBody(t, def, ""), &sub); status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}
	runID := uuid.MustParse(sub.RunID)

	put := func(k, value string, tags []string) {
		if err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
			_, err := store.PutBlackboardEntry(ctx, q, store.BlackboardPutArgs{
				RunID: runID, Key: k, Value: json.RawMessage(value), Tags: tags,
				TokenCount: 3, TokenCounter: "fallback/chars4@1",
				AuthorStepID: "only", AuthorAttempt: 1, Now: testNow,
			})
			return err
		}); err != nil {
			t.Fatalf("seeding blackboard %s: %v", k, err)
		}
	}
	put("draft", `"v1"`, []string{"pinned", "writer"})
	put("draft", `"v2"`, []string{"writer"}) // draft head is v2
	put("notes", `{"n":1}`, []string{"writer"})
	put("scratch", `1`, nil)
	return runID
}

func bbPath(runID uuid.UUID, query string) string {
	p := "/v1/runs/" + runID.String() + "/blackboard"
	if query != "" {
		p += "?" + query
	}
	return p
}

func TestBlackboardEndpointHeadsFiltersHistory(t *testing.T) {
	t.Parallel()
	s, srv, key := newServer(t)
	runID := seedBlackboardRun(t, s, srv, key)

	// Default: heads only, ordered by key.
	var heads api.BlackboardResponse
	if status := getJSON(t, srv, key, bbPath(runID, ""), &heads); status != http.StatusOK {
		t.Fatalf("GET blackboard = %d, want 200", status)
	}
	if heads.History {
		t.Fatal("default response marked history")
	}
	if len(heads.Entries) != 3 {
		t.Fatalf("heads = %d, want 3 (draft, notes, scratch)", len(heads.Entries))
	}
	if heads.Entries[0].Key != "draft" || heads.Entries[0].Version != 2 {
		t.Fatalf("first head = %s v%d, want draft v2", heads.Entries[0].Key, heads.Entries[0].Version)
	}
	if heads.Entries[0].TokenCounter == "" || heads.Entries[0].AuthorStepID != "only" {
		t.Fatalf("head metadata missing: %+v", heads.Entries[0])
	}

	// Tag filter on heads applies to the head: pinned was on draft v1, so the
	// head (v2) is not pinned — the filter surfaces nothing.
	var pinned api.BlackboardResponse
	getJSON(t, srv, key, bbPath(runID, "tag=pinned"), &pinned)
	if len(pinned.Entries) != 0 {
		t.Fatalf("tag=pinned heads = %d, want 0 (superseded tag)", len(pinned.Entries))
	}

	// Key filter.
	var byKey api.BlackboardResponse
	getJSON(t, srv, key, bbPath(runID, "key=notes&key=scratch"), &byKey)
	if len(byKey.Entries) != 2 {
		t.Fatalf("key filter = %d, want 2", len(byKey.Entries))
	}

	// History: every version, ordered by (key, version). draft has 2.
	var hist api.BlackboardResponse
	getJSON(t, srv, key, bbPath(runID, "history=true"), &hist)
	if !hist.History || len(hist.Entries) != 4 {
		t.Fatalf("history = %d entries (history=%v), want 4", len(hist.Entries), hist.History)
	}
	// The pinned v1 is visible in history (matched by its own tag).
	var histPinned api.BlackboardResponse
	getJSON(t, srv, key, bbPath(runID, "history=true&tag=pinned"), &histPinned)
	if len(histPinned.Entries) != 1 || histPinned.Entries[0].Version != 1 {
		t.Fatalf("history tag=pinned = %d, want 1 (draft v1)", len(histPinned.Entries))
	}
}

func TestBlackboardEndpointPaginates(t *testing.T) {
	t.Parallel()
	s, srv, key := newServer(t)
	runID := seedBlackboardRun(t, s, srv, key)

	// Walk heads at limit 1: draft, notes, scratch across 3 pages.
	var keys []string
	query := "limit=1"
	for {
		var page api.BlackboardResponse
		if status := getJSON(t, srv, key, bbPath(runID, query), &page); status != http.StatusOK {
			t.Fatalf("GET page = %d, want 200", status)
		}
		for _, e := range page.Entries {
			keys = append(keys, e.Key)
		}
		if page.NextCursor == "" {
			break
		}
		query = "limit=1&cursor=" + page.NextCursor
	}
	if len(keys) != 3 || keys[0] != "draft" || keys[2] != "scratch" {
		t.Fatalf("paginated keys = %v, want [draft notes scratch]", keys)
	}
}

func TestBlackboardEndpointErrors(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	// Missing run → 404.
	var body map[string]any
	if status := getJSON(t, srv, key, bbPath(uuid.New(), ""), &body); status != http.StatusNotFound {
		t.Fatalf("GET missing run = %d, want 404", status)
	}

	// A valid run with a bad limit → 400.
	s2, srv2, key2 := newServer(t)
	runID := seedBlackboardRun(t, s2, srv2, key2)
	_ = s2
	if status := getJSON(t, srv2, key2, bbPath(runID, "limit=0"), &body); status != http.StatusBadRequest {
		t.Fatalf("GET limit=0 = %d, want 400", status)
	}
	if status := getJSON(t, srv2, key2, bbPath(runID, "cursor=not-base64!"), &body); status != http.StatusBadRequest {
		t.Fatalf("GET bad cursor = %d, want 400", status)
	}
}
