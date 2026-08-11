package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mathcslearner/agentloom/internal/api"
)

// runCtl executes the command tree with the given args and env, returning
// stdout, stderr, and the error.
func runCtl(t *testing.T, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	root := newRootCmd(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

// writeFile drops content into a temp file and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

const validDef = `{
	"schema_version": 1,
	"name": "ctl-test",
	"steps": [{"id": "only", "type": "noop"}],
	"edges": []
}`

func TestValidateValidDefinition(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "valid.json", validDef)
	out, _, err := runCtl(t, nil, "validate", path)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.Contains(out, "valid") {
		t.Errorf("output %q does not report validity", out)
	}
}

func TestValidateCanonicalExamples(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("../../examples/definitions/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing examples: files=%v err=%v", files, err)
	}
	for _, f := range files {
		if _, _, err := runCtl(t, nil, "validate", f); err != nil {
			t.Errorf("validate %s: %v", f, err)
		}
	}
}

func TestValidateInvalidDefinition(t *testing.T) {
	t.Parallel()

	// Unknown edge endpoint: structural validation failure with a path.
	path := writeFile(t, "invalid.json", `{
		"schema_version": 1,
		"name": "bad",
		"steps": [{"id": "a", "type": "noop"}],
		"edges": [{"from": "a", "to": "ghost"}]
	}`)
	out, _, err := runCtl(t, nil, "validate", path)
	if err == nil {
		t.Fatal("validate of invalid definition: want error, got nil")
	}
	if !strings.Contains(out, "edges[0]") || !strings.Contains(out, "unknown_edge_endpoint") {
		t.Errorf("output %q lacks the path-qualified issue", out)
	}
}

func TestValidateMalformedJSON(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "garbage.json", `{"name": `)
	if _, _, err := runCtl(t, nil, "validate", path); err == nil {
		t.Fatal("validate of malformed JSON: want error, got nil")
	}
}

func TestSubmitPrintsRunID(t *testing.T) {
	t.Parallel()

	const runID = "6b8c0f2e-0000-4000-8000-000000000001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req api.SubmitRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding submit body: %v", err)
		}
		if len(req.Definition) == 0 {
			t.Error("submit body has no definition")
		}
		if string(req.Params) != `{"topic":"x"}` {
			t.Errorf("params = %s", req.Params)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(api.SubmitRunResponse{
			RunID: runID, Status: "running", EntrySteps: []string{"only"},
		}); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	}))
	defer srv.Close()

	path := writeFile(t, "def.json", validDef)
	out, errOut, err := runCtl(t, nil, "submit", path, "--api", srv.URL, "--params", `{"topic":"x"}`)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if strings.TrimSpace(out) != runID {
		t.Errorf("stdout = %q, want just the run id %q", out, runID)
	}
	if !strings.Contains(errOut, "entry steps: only") {
		t.Errorf("stderr %q lacks the entry-step summary", errOut)
	}
}

func TestSubmitRendersValidationErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(api.ErrorBody{Error: api.ErrorDetail{
			Code:    api.ErrCodeInvalidDefinition,
			Message: "definition failed validation",
			Issues: []api.Issue{{
				Code: "unknown_edge_endpoint", Severity: "error",
				Path: "edges[0]", Msg: "edge references unknown step",
			}},
		}}); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	}))
	defer srv.Close()

	path := writeFile(t, "def.json", validDef)
	_, errOut, err := runCtl(t, nil, "submit", path, "--api", srv.URL)
	if err == nil {
		t.Fatal("submit rejected by API: want error, got nil")
	}
	if !strings.Contains(errOut, "edges[0]") || !strings.Contains(errOut, "invalid_definition") {
		t.Errorf("stderr %q lacks the rendered issues", errOut)
	}
}

func TestSubmitRejectsBadParams(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "def.json", validDef)
	if _, _, err := runCtl(t, nil, "submit", path, "--params", "{not json"); err == nil {
		t.Fatal("submit with invalid --params: want error, got nil")
	}
}

// watchResponse fabricates a two-step run in the given status.
func watchResponse(status, stepStatus string) api.RunResponse {
	succeeded := 0
	if stepStatus == "succeeded" {
		succeeded = 2
	}
	return api.RunResponse{
		Run: api.RunView{ID: "run-1", Status: status, StepsTotal: 2, StepsSucceeded: succeeded},
		Steps: []api.StepView{
			{ID: "first", Type: "noop", Status: stepStatus},
			{ID: "second", Type: "echo", Status: stepStatus},
		},
		Edges: []api.EdgeView{{From: "first", To: "second", Type: "normal", Resolution: "fired"}},
	}
}

func TestWatchUntilSucceeded(t *testing.T) {
	t.Parallel()

	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run-1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		resp := watchResponse("running", "running")
		if polls.Add(1) >= 3 {
			resp = watchResponse("succeeded", "succeeded")
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	}))
	defer srv.Close()

	out, _, err := runCtl(t, nil, "watch", "run-1", "--api", srv.URL, "--interval", "5ms", "--timeout", "10s")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if got := polls.Load(); got < 3 {
		t.Errorf("polled %d times, want at least 3", got)
	}
	// The tree renders in topological order and reflects the final state.
	if !strings.Contains(out, "run run-1: succeeded — 2/2 succeeded") {
		t.Errorf("output lacks the final header:\n%s", out)
	}
	first := strings.Index(out, "first [noop]")
	second := strings.Index(out, "second [echo]")
	if first == -1 || second == -1 || second < first {
		t.Errorf("steps missing or out of topological order:\n%s", out)
	}
}

func TestWatchFailedRunExitsNonZero(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(watchResponse("failed", "failed")); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	}))
	defer srv.Close()

	_, _, err := runCtl(t, nil, "watch", "run-1", "--api", srv.URL, "--interval", "5ms")
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("watch of failed run: err = %v, want failure", err)
	}
}

func TestAPIBaseFromEnv(t *testing.T) {
	t.Parallel()

	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(watchResponse("succeeded", "succeeded")); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	}))
	defer srv.Close()

	_, _, err := runCtl(t, map[string]string{"AGENTLOOM_API_URL": srv.URL},
		"watch", "run-1", "--interval", "5ms")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if !hit.Load() {
		t.Error("server from AGENTLOOM_API_URL was never called")
	}
}

// Guard against cobra swallowing usage errors silently.
func TestUnknownCommandErrors(t *testing.T) {
	t.Parallel()

	root := newRootCmd(func(string) (string, bool) { return "", false })
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"frobnicate"})
	if err := root.Execute(); err == nil {
		t.Fatal("unknown subcommand: want error, got nil")
	}
}
