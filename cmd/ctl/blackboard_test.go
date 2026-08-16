package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/api"
)

// fakeBlackboardServer stubs GET /v1/runs/{id}/blackboard, recording the
// raw query it received.
func fakeBlackboardServer(t *testing.T, lastQuery *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/blackboard") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		*lastQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.BlackboardResponse{
			RunID: "r", History: false,
			Entries: []api.BlackboardEntryView{
				{Key: "draft", Version: 2, Value: json.RawMessage(`"hi"`), TokenCount: 3, TokenCounter: "fallback/chars4@1", Tags: []string{"writer"}, AuthorStepID: "w"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestBlackboardRendersTableAndQuery(t *testing.T) {
	t.Parallel()
	var query string
	srv := fakeBlackboardServer(t, &query)

	out, _, err := runCtl(t, nil, "blackboard", "run-1",
		"--name", "draft", "--tag", "writer", "--history", "--limit", "50",
		"--api", srv.URL, "--key", "sk_"+strings.Repeat("a", 43))
	if err != nil {
		t.Fatalf("blackboard: %v", err)
	}
	for _, needle := range []string{"KEY", "VERSION", "TOKENS", "TAGS", "AUTHOR", "draft", "writer"} {
		if !strings.Contains(out, needle) {
			t.Errorf("output %q missing %q", out, needle)
		}
	}
	if !strings.Contains(query, "history=true") || !strings.Contains(query, "tag=writer") || !strings.Contains(query, "limit=50") {
		t.Errorf("query %q missing expected params", query)
	}
}
