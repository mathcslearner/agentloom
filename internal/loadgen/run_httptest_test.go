package loadgen

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
)

// fakeAPI is a minimal in-memory API implementing exactly the endpoints the
// generator drives in poll mode: health, definition register, submit, get run
// (terminal on first read), run list (reconciliation), and system stats.
type fakeAPI struct {
	mu   sync.Mutex
	runs map[string]string // run id → definition id
}

func newFakeAPI() *fakeAPI { return &fakeAPI{runs: map[string]string{}} }

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /v1/system/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, api.SystemStatsResponse{ObservedAt: time.Now(), Queue: &api.QueueStatsView{}, Outbox: api.OutboxStatsView{}})
	})
	mux.HandleFunc("POST /v1/definitions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(201)
		writeJSON(w, api.DefinitionResponse{DefinitionView: api.DefinitionView{ID: uuid.NewString(), Version: 1}})
	})
	mux.HandleFunc("POST /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		var req api.SubmitRunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := uuid.NewString()
		f.mu.Lock()
		f.runs[id] = req.DefinitionID
		f.mu.Unlock()
		w.WriteHeader(201)
		writeJSON(w, api.SubmitRunResponse{RunID: id, Status: "running"})
	})
	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		fin := time.Now()
		writeJSON(w, api.RunResponse{Run: api.RunView{ID: id, Status: "succeeded", StepsTotal: 10, StepsSucceeded: 10, FinishedAt: &fin}})
	})
	mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		defID := r.URL.Query().Get("definition_id")
		f.mu.Lock()
		var out []api.RunView
		for id, d := range f.runs {
			if d == defID {
				fin := time.Now()
				out = append(out, api.RunView{ID: id, Status: "succeeded", FinishedAt: &fin})
			}
		}
		f.mu.Unlock()
		writeJSON(w, api.ListRunsResponse{Runs: out})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestRunDryRunPollMode(t *testing.T) {
	srv := httptest.NewServer(newFakeAPI().handler())
	defer srv.Close()

	dir := t.TempDir()
	cfg := Config{
		APIBase: srv.URL, APIKey: "sk_test", ScenarioDir: "../../test/load/scenarios",
		Scenario: "linear-10", Track: TrackPoll, SchedSample: 0,
		RateOverride: 50, MaxRuns: 100, WarmupOverride: 1, // ~0 warmup
		OutDir: dir, PollInterval: 50 * time.Millisecond, PollAfter: 10 * time.Millisecond,
		Progress: 200 * time.Millisecond, DrainTimeout: 5 * time.Second, RunTimeout: 10 * time.Second,
	}
	rep, err := Run(t.Context(), cfg, nil, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// DoD-1: a complete report artifact with percentiles.
	for _, name := range []string{"summary.json", "summary.md", "runs.csv", "timeseries.csv", "hist-end_to_end.csv"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
	if rep.Counts[classRunSucceeded] != 100 {
		t.Errorf("succeeded = %d, want 100", rep.Counts[classRunSucceeded])
	}
	if rep.Integrity.LostRuns != 0 {
		t.Errorf("lost runs = %d, want 0", rep.Integrity.LostRuns)
	}
	if rep.Latency["end_to_end"].Count == 0 || rep.Latency["submit_rtt"].Count == 0 {
		t.Error("latency histograms are empty")
	}

	// DoD-2: open-loop achieved rate accurate within ±5% of the offered 50/s.
	if rep.Rate.OfferedPerSec != 50 {
		t.Errorf("offered = %g, want 50", rep.Rate.OfferedPerSec)
	}
	if rep.Rate.RateErrorPct < -5 || rep.Rate.RateErrorPct > 5 {
		t.Errorf("rate error = %.2f%%, want within ±5%% (achieved %.1f/s)", rep.Rate.RateErrorPct, rep.Rate.AchievedPerSec)
	}

	// DoD-3: the failure taxonomy is present and sums to the run count.
	total := 0
	for _, tal := range rep.Taxonomy {
		total += tal.Count
	}
	if total != 100 {
		t.Errorf("taxonomy sums to %d, want 100", total)
	}

	// summary.md carries the arrival-accuracy and outcomes sections.
	md, _ := os.ReadFile(filepath.Join(dir, "summary.md")) //nolint:gosec // G304: test-controlled temp dir
	if !strings.Contains(string(md), "open-loop accuracy") || !strings.Contains(string(md), "Outcomes") {
		t.Error("summary.md missing expected sections")
	}
}
