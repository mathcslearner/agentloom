package main

// Unit tests for the 6.5 commands against httptest fakes — the ctl_test.go
// pattern: assert the request shape on the fake and the stdout/stderr
// split on the command.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
)

// fakeAPI serves one expected POST and encodes resp (every 6.5 lifecycle
// op is a POST).
func fakeAPI(t *testing.T, path string, status int, resp any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			t.Errorf("unexpected request: %s %s, want POST %s", r.Method, r.URL.Path, path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if resp != nil {
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Errorf("encoding response: %v", err)
			}
		}
	}))
}

func TestSubmitSendsIdempotencyKeyHeader(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(api.IdempotencyKeyHeader); got != "tok-42" {
			t.Errorf("%s header = %q, want tok-42", api.IdempotencyKeyHeader, got)
		}
		var req map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		if _, ok := req["idempotency_token"]; ok {
			t.Error("body still carries the retired idempotency_token field")
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SubmitRunResponse{RunID: "r", Status: "running"})
	}))
	defer srv.Close()

	path := writeFile(t, "def.json", validDef)
	if _, _, err := runCtl(t, nil, "submit", path, "--api", srv.URL, "--token", "tok-42"); err != nil {
		t.Fatalf("submit: %v", err)
	}
}

func TestCancelCommand(t *testing.T) {
	t.Parallel()
	srv := fakeAPI(t, "/v1/runs/r-1/cancel", http.StatusOK, api.CancelRunResponse{
		Run:            api.RunView{ID: "r-1", Status: "cancelled"},
		CancelledSteps: []string{"a", "b"},
		Finalized:      true,
	})
	defer srv.Close()

	out, errOut, err := runCtl(t, nil, "cancel", "r-1", "--api", srv.URL)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "cancelled") || !strings.Contains(errOut, "2 step(s) swept") {
		t.Errorf("stderr = %q, want the finalized summary", errOut)
	}
}

func TestCancelConflictSurfacesAPIError(t *testing.T) {
	t.Parallel()
	srv := fakeAPI(t, "/v1/runs/r-1/cancel", http.StatusConflict, api.ErrorBody{
		Error: api.ErrorDetail{Code: api.ErrCodeConflict, Message: "run is succeeded"},
	})
	defer srv.Close()

	_, _, err := runCtl(t, nil, "cancel", "r-1", "--api", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("cancel of finished run: err = %v, want the conflict envelope", err)
	}
}

func TestParkUnparkCommands(t *testing.T) {
	t.Parallel()
	park := fakeAPI(t, "/v1/runs/r-1/park", http.StatusOK, api.ParkRunResponse{
		Run: api.RunView{ID: "r-1", Status: "parked"},
	})
	defer park.Close()
	if _, errOut, err := runCtl(t, nil, "park", "r-1", "--api", park.URL); err != nil || !strings.Contains(errOut, "parked") {
		t.Fatalf("park: err=%v stderr=%q", err, errOut)
	}

	unpark := fakeAPI(t, "/v1/runs/r-1/unpark", http.StatusOK, api.UnparkRunResponse{
		Run: api.RunView{ID: "r-1", Status: "running"}, Dispatched: []string{"b"},
	})
	defer unpark.Close()
	if _, errOut, err := runCtl(t, nil, "unpark", "r-1", "--api", unpark.URL); err != nil || !strings.Contains(errOut, "1 step(s) re-dispatched") {
		t.Fatalf("unpark: err=%v stderr=%q", err, errOut)
	}
}

func TestRequeueCommand(t *testing.T) {
	t.Parallel()
	srv := fakeAPI(t, "/v1/runs/r-1/steps/flaky/requeue", http.StatusOK, api.RequeueStepResponse{
		RunID: "r-1", StepID: "flaky", Status: "ready",
		RunResumed: true, Revived: []string{"down"},
	})
	defer srv.Close()

	_, errOut, err := runCtl(t, nil, "requeue", "r-1", "flaky", "--api", srv.URL)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	for _, want := range []string{"requeued", "run resumed", "revived: down"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr = %q, missing %q", errOut, want)
		}
	}
}

func TestRunsCommandRendersTableAndCursor(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("status") != "running" || q.Get("limit") != "2" {
			t.Errorf("query = %v, want status=running limit=2", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ListRunsResponse{
			Runs: []api.RunView{
				{ID: "r-2", Status: "running", StepsTotal: 3, StepsSucceeded: 1, CreatedAt: created},
				{ID: "r-1", Status: "running", StepsTotal: 3, CreatedAt: created.Add(-time.Minute)},
			},
			NextCursor: "opaque-cursor",
		})
	}))
	defer srv.Close()

	out, errOut, err := runCtl(t, nil, "runs", "--api", srv.URL, "--status", "running", "--limit", "2")
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if !strings.Contains(out, "ID") || !strings.Contains(out, "r-2") || !strings.Contains(out, "r-1") {
		t.Errorf("stdout table = %q", out)
	}
	if !strings.Contains(errOut, "--cursor opaque-cursor") {
		t.Errorf("stderr = %q, want the next-page hint", errOut)
	}
}
