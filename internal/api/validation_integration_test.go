//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 11.1 acceptance: verdict persistence is visible in the run status
// API. A validated llm step runs through the engine on the mock provider; a
// GET /v1/runs/{id} then carries the passing verdict on the attempt.

// apiPassValidator is a minimal in-test validator that always passes.
type apiPassValidator struct{}

func (apiPassValidator) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind: plugin.KindValidator, Name: "api_ok", Version: "1.0.0",
		Capabilities: plugin.Capabilities{Cacheable: true},
		ConfigSchema: validate.EmptyConfigSchema(),
	}
}

func (apiPassValidator) Validate(context.Context, validate.Input) (validate.Verdict, error) {
	return validate.PassVerdict(), nil
}

func TestRunVerdictVisibleInStatusAPI(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	rootKey := mintTestKey(t)
	handler, err := api.New(s, time.Now, nil, rootKey, api.RateLimitOptions{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{
		Interval: 10 * time.Millisecond, Batch: 16,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	dctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); d.Run(dctx) }()

	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	vreg, err := validate.NewRegistry(apiPassValidator{})
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(providers))
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge), engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1})

	def := `{
		"schema_version": 1,
		"name": "validated-run",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "produce output", "max_tokens": 64, "temperature": 0},
			 "validation": {"validators": [{"name": "api_ok"}]}}
		],
		"edges": []
	}`
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, rootKey, submitBody(t, []byte(def), `{}`), &sub); status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}

	deadline := time.Now().Add(15 * time.Second)
	var run api.RunResponse
	for {
		if getJSON(t, srv, rootKey, "/v1/runs/"+sub.RunID, &run) != http.StatusOK {
			t.Fatal("GET run failed mid-watch")
		}
		if run.Run.Status != store.RunStatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never finished:\n%+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Run.Status != store.RunStatusSucceeded {
		t.Fatalf("run status = %q, want succeeded", run.Run.Status)
	}

	// Find the gen step's single attempt and assert its verdict is present
	// and a pass — the 11.1 "verdict visible in run status API" criterion.
	var verdictRaw json.RawMessage
	for _, st := range run.Steps {
		if st.ID != "gen" {
			continue
		}
		if len(st.Attempts) != 1 {
			t.Fatalf("gen attempts = %d, want 1", len(st.Attempts))
		}
		verdictRaw = st.Attempts[0].Verdict
	}
	if len(verdictRaw) == 0 {
		t.Fatal("gen attempt carries no verdict on the wire")
	}
	var v validate.Verdict
	if err := json.Unmarshal(verdictRaw, &v); err != nil {
		t.Fatalf("unmarshaling wire verdict: %v", err)
	}
	if !v.Passed() || v.SchemaVersion != validate.VerdictSchemaVersion {
		t.Fatalf("wire verdict = %+v, want a pass at schema version %d", v, validate.VerdictSchemaVersion)
	}
}

// apiFailNValidator fails until the given semantic attempt is reached (using
// validate.Input.Attempt), then passes — the API-side driver for a
// semantic-retry run.
type apiFailNValidator struct{ passAt int }

func (apiFailNValidator) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind: plugin.KindValidator, Name: "api_gate", Version: "1.0.0",
		Capabilities: plugin.Capabilities{Cacheable: true},
		ConfigSchema: validate.EmptyConfigSchema(),
	}
}

func (g apiFailNValidator) Validate(_ context.Context, in validate.Input) (validate.Verdict, error) {
	if in.Attempt >= g.passAt {
		return validate.PassVerdict(), nil
	}
	return validate.FailVerdict(validate.Issue{Code: "too_early", Message: "needs another pass"}), nil
}

// TestSemanticRetryVisibleInStatusAPI (ticket 11.4): a semantic-retry run's
// attempt history on GET /v1/runs carries the per-attempt feedback critique,
// and the step reports the disjoint transport/validation failure counters.
func TestSemanticRetryVisibleInStatusAPI(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	rootKey := mintTestKey(t)
	handler, err := api.New(s, time.Now, nil, rootKey, api.RateLimitOptions{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{
		Interval: 10 * time.Millisecond, Batch: 16,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	dctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); d.Run(dctx) }()

	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	vreg, err := validate.NewRegistry(apiFailNValidator{passAt: 3})
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(providers))
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-sem",
		engine.WithDispatchNudge(d.Nudge), engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-sem", eng.Handle, queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1})

	def := `{
		"schema_version": 1,
		"name": "semantic-run",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "produce output", "max_tokens": 64, "temperature": 0},
			 "validation": {"validators": [{"name": "api_gate"}], "max_attempts": 3}}
		],
		"edges": []
	}`
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, rootKey, submitBody(t, []byte(def), `{}`), &sub); status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}

	deadline := time.Now().Add(15 * time.Second)
	var run api.RunResponse
	for {
		if getJSON(t, srv, rootKey, "/v1/runs/"+sub.RunID, &run) != http.StatusOK {
			t.Fatal("GET run failed mid-watch")
		}
		if run.Run.Status == store.RunStatusSucceeded || run.Run.Status == store.RunStatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never finished:\n%+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Run.Status != store.RunStatusSucceeded {
		t.Fatalf("run status = %q, want succeeded", run.Run.Status)
	}

	var gen api.StepView
	for _, st := range run.Steps {
		if st.ID == "gen" {
			gen = st
		}
	}
	if len(gen.Attempts) != 3 {
		t.Fatalf("gen attempts = %d, want 3", len(gen.Attempts))
	}
	// The disjoint failure counters: two semantic failures, no transport ones.
	if gen.ValidationFailures != 2 || gen.TransportFailures != 0 {
		t.Errorf("failure counts = transport %d / validation %d, want 0 / 2",
			gen.TransportFailures, gen.ValidationFailures)
	}
	// The first attempt carried no feedback; attempts 2 and 3 each carry a
	// distinct critique on the wire.
	if len(gen.Attempts[0].Feedback) != 0 {
		t.Errorf("attempt 1 carries feedback on the wire, want none: %s", gen.Attempts[0].Feedback)
	}
	var fb2, fb3 struct {
		SemanticAttempt int    `json:"semantic_attempt"`
		Text            string `json:"text"`
	}
	if err := json.Unmarshal(gen.Attempts[1].Feedback, &fb2); err != nil {
		t.Fatalf("attempt 2 feedback not on the wire: %v", err)
	}
	if err := json.Unmarshal(gen.Attempts[2].Feedback, &fb3); err != nil {
		t.Fatalf("attempt 3 feedback not on the wire: %v", err)
	}
	if fb2.SemanticAttempt != 2 || fb3.SemanticAttempt != 3 {
		t.Errorf("wire semantic attempts = %d,%d, want 2,3", fb2.SemanticAttempt, fb3.SemanticAttempt)
	}
	if fb2.Text == "" || fb2.Text == fb3.Text {
		t.Error("wire feedback text must be present and diffable across attempts")
	}

	// Ticket 11.6 acceptance: the per-step validation summary is on the wire,
	// rolled up from the three attempt verdicts (fail, fail, pass).
	if gen.Validation == nil {
		t.Fatal("step carries no validation summary on the wire")
	}
	vs := gen.Validation
	if vs.Attempts != 3 || vs.Passed != 1 || vs.Failed != 2 {
		t.Errorf("summary attempts/passed/failed = %d/%d/%d, want 3/1/2",
			vs.Attempts, vs.Passed, vs.Failed)
	}
	if vs.LastAttempt != 3 || vs.LastStatus != "pass" {
		t.Errorf("summary last = attempt %d status %q, want 3 pass", vs.LastAttempt, vs.LastStatus)
	}
	if len(vs.Validators) != 1 || vs.Validators[0].Name != "api_gate" {
		t.Fatalf("summary validators = %+v, want one api_gate", vs.Validators)
	}
	if v0 := vs.Validators[0]; v0.Passed != 1 || v0.Failed != 2 || v0.LastStatus != "pass" {
		t.Errorf("validator roll-up = passed %d failed %d last %q, want 1/2/pass",
			v0.Passed, v0.Failed, v0.LastStatus)
	}
}
