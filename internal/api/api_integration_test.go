//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 4.6's integration suite: the ingest API over a real store, and —
// the headline — the canonical fanout fixture submitted through the API
// and executed to success by the production dispatcher + two workers.

// testNow is the injected clock for the pure handler tests (the
// end-to-end test uses time.Now, exactly as cmd/api wires it).
var testNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// newServer boots the API over a fresh per-test database and returns a
// bearer key scoped submit+read — since ticket 6.2 every /v1 route
// requires one. The root credential is discarded after minting it, as
// ADR-007's bootstrap flow prescribes.
func newServer(t *testing.T) (*store.Store, *httptest.Server, string) {
	t.Helper()
	_, s, srv, key := newServerWithPool(t)
	return s, srv, key
}

// newServerWithPool is newServer exposing the underlying pool too, for
// tests that need raw SQL access (e.g. manufacturing pre-0009 rows).
func newServerWithPool(t *testing.T) (*pgxpool.Pool, *store.Store, *httptest.Server, string) {
	t.Helper()
	pool := storetest.NewDB(t)
	s := store.NewFromPool(pool)
	rootKey := mintTestKey(t)
	h, err := api.New(s, func() time.Time { return testNow }, nil, rootKey, api.RateLimitOptions{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := createKey(t, srv, rootKey, api.CreateKeyRequest{Name: "test-client", Scopes: []string{"submit", "read"}})
	return pool, s, srv, client.Key
}

// postJSON POSTs body to the run-submission route with the bearer
// credential and decodes the response into out (may be nil), returning
// the status code.
func postJSON(t *testing.T, srv *httptest.Server, bearer string, body []byte, out any) int {
	t.Helper()
	res := doAuth(t, srv, http.MethodPost, "/v1/runs", bearer, body, out)
	return res.StatusCode
}

// getJSON GETs path with the bearer credential (empty = anonymous) and
// decodes the response into out (may be nil), returning the status code.
func getJSON(t *testing.T, srv *httptest.Server, bearer, path string, out any) int {
	t.Helper()
	res := doAuth(t, srv, http.MethodGet, path, bearer, nil, out)
	return res.StatusCode
}

// submitBody wraps a definition document into a submission request.
// Idempotent submission rides the Idempotency-Key header since 6.5 — see
// postRunIdem.
func submitBody(t *testing.T, def []byte, params string) []byte {
	t.Helper()
	req := api.SubmitRunRequest{Definition: def}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling submit request: %v", err)
	}
	return body
}

// postRunIdem POSTs a run submission carrying the Idempotency-Key header.
func postRunIdem(t *testing.T, srv *httptest.Server, bearer, idemKey string, body []byte, out any) int {
	t.Helper()
	res := doAuthHdr(t, srv, http.MethodPost, "/v1/runs", bearer,
		map[string]string{api.IdempotencyKeyHeader: idemKey}, body, out)
	return res.StatusCode
}

// fanoutJSON loads the canonical fanout fixture — the definition the
// ticket's acceptance names.
func fanoutJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../examples/definitions/fanout.json")
	if err != nil {
		t.Fatalf("reading fanout fixture: %v", err)
	}
	return raw
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	_, srv, _ := newServer(t)

	var body map[string]string
	if status := getJSON(t, srv, "", "/healthz", &body); status != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", status)
	}
	if body["status"] != "ok" {
		t.Errorf("healthz body = %v", body)
	}
}

func TestSubmitAndGetRun(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	var sub api.SubmitRunResponse
	status := postJSON(t, srv, key, submitBody(t, fanoutJSON(t), `{"topic": "durable execution"}`), &sub)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}
	if sub.Status != store.RunStatusRunning || sub.Reused {
		t.Errorf("submit response = %+v", sub)
	}
	if len(sub.EntrySteps) != 1 || sub.EntrySteps[0] != "start" {
		t.Errorf("entry steps = %v, want [start]", sub.EntrySteps)
	}

	var run api.RunResponse
	if status := getJSON(t, srv, key, "/v1/runs/"+sub.RunID, &run); status != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s = %d, want 200", sub.RunID, status)
	}
	if run.Run.ID != sub.RunID || run.Run.Status != store.RunStatusRunning {
		t.Errorf("run view = %+v", run.Run)
	}
	if run.Run.StepsTotal != 6 || len(run.Steps) != 6 {
		t.Errorf("steps_total = %d, len(steps) = %d, want 6", run.Run.StepsTotal, len(run.Steps))
	}
	byID := map[string]api.StepView{}
	for _, s := range run.Steps {
		byID[s.ID] = s
	}
	if got := byID["start"].Status; got != store.StepStatusReady {
		t.Errorf("start status = %q, want ready", got)
	}
	if got := byID["gather"].Status; got != store.StepStatusPending {
		t.Errorf("gather status = %q, want pending", got)
	}
	if got := byID["gather"].RemainingDeps; got != 3 {
		t.Errorf("gather remaining_deps = %d, want 3", got)
	}
	if len(run.Edges) != 7 {
		t.Errorf("len(edges) = %d, want 7", len(run.Edges))
	}
	for _, e := range run.Edges {
		if e.Resolution != store.EdgeResolutionUnresolved {
			t.Errorf("edge %s->%s resolution = %q, want unresolved", e.From, e.To, e.Resolution)
		}
	}
}

func TestSubmitInvalidDefinitionRejected(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	// Fanout with a broken edge: structural validation must reject it with
	// the path-qualified issue, and nothing may be written.
	bad := []byte(`{
		"schema_version": 1,
		"name": "bad",
		"steps": [{"id": "a", "type": "noop"}],
		"edges": [{"from": "a", "to": "ghost"}]
	}`)
	var envelope api.ErrorBody
	status := postJSON(t, srv, key, submitBody(t, bad, ""), &envelope)
	if status != http.StatusBadRequest {
		t.Fatalf("POST invalid definition = %d, want 400", status)
	}
	if envelope.Error.Code != api.ErrCodeInvalidDefinition {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, api.ErrCodeInvalidDefinition)
	}
	var hit bool
	for _, i := range envelope.Error.Issues {
		if i.Path == "edges[0].to" && i.Code == "unknown_edge_endpoint" {
			hit = true
		}
	}
	if !hit {
		t.Errorf("issues %+v lack the path-qualified unknown_edge_endpoint", envelope.Error.Issues)
	}
}

func TestSubmitMalformedRequests(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"not json", `{"definition": `, api.ErrCodeInvalidRequest},
		{"unknown body field", `{"definitions": {}}`, api.ErrCodeInvalidRequest},
		{"neither definition nor ref", `{"params": {}}`, api.ErrCodeInvalidRequest},
		{"both definition and ref", `{"definition": {"schema_version":1,"name":"x","steps":[{"id":"a","type":"noop"}],"edges":[]}, "definition_id": "6b8c0f2e-0000-4000-8000-000000000001"}`, api.ErrCodeInvalidRequest},
		{"bad definition_id", `{"definition_id": "not-a-uuid"}`, api.ErrCodeInvalidRequest},
		{"unknown definition_id", `{"definition_id": "6b8c0f2e-0000-4000-8000-000000000001"}`, api.ErrCodeDefinitionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var envelope api.ErrorBody
			status := postJSON(t, srv, key, []byte(tc.body), &envelope)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if envelope.Error.Code != tc.code {
				t.Errorf("error code = %q, want %q", envelope.Error.Code, tc.code)
			}
		})
	}
}

func TestSubmitIdempotencyToken(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	body := submitBody(t, fanoutJSON(t), `{"topic": "x"}`)
	var first api.SubmitRunResponse
	if status := postRunIdem(t, srv, key, "ctl-test-token", body, &first); status != http.StatusCreated {
		t.Fatalf("first POST = %d, want 201", status)
	}
	var second api.SubmitRunResponse
	if status := postRunIdem(t, srv, key, "ctl-test-token", body, &second); status != http.StatusOK {
		t.Fatalf("second POST = %d, want 200", status)
	}
	if !second.Reused || second.RunID != first.RunID {
		t.Errorf("second submit = %+v, want reuse of %s", second, first.RunID)
	}
}

func TestSubmitByStoredDefinitionRef(t *testing.T) {
	t.Parallel()
	s, srv, key := newServer(t)

	row, err := s.Definitions().Create(t.Context(), gen.CreateDefinitionParams{
		Name: "fanout-fanin", Version: 1, Spec: fanoutJSON(t),
	})
	if err != nil {
		t.Fatalf("storing definition: %v", err)
	}

	body, err := json.Marshal(api.SubmitRunRequest{
		DefinitionID: row.ID.String(),
		Params:       json.RawMessage(`{"topic": "stored"}`),
	})
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, key, body, &sub); status != http.StatusCreated {
		t.Fatalf("POST by ref = %d, want 201", status)
	}

	// The run must record its definition provenance.
	run, err := s.Runs().Get(t.Context(), uuid.MustParse(sub.RunID))
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.DefinitionID == nil || *run.DefinitionID != row.ID {
		t.Errorf("run.definition_id = %v, want %s", run.DefinitionID, row.ID)
	}
}

func TestGetRunMisses(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	var envelope api.ErrorBody
	if status := getJSON(t, srv, key, "/v1/runs/"+uuid.NewString(), &envelope); status != http.StatusNotFound {
		t.Fatalf("GET unknown run = %d, want 404", status)
	}
	if envelope.Error.Code != api.ErrCodeRunNotFound {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, api.ErrCodeRunNotFound)
	}
	if status := getJSON(t, srv, key, "/v1/runs/not-a-uuid", &envelope); status != http.StatusBadRequest {
		t.Fatalf("GET bad uuid = %d, want 400", status)
	}
}

// TestSubmitFanoutRunsToCompletion is the ticket's acceptance loop minus
// Docker: the canonical fanout fixture goes in through POST /v1/runs, the
// production dispatcher drains its outbox rows, two builtin-registry
// workers (llm/tool/retrieve running as dev stubs) execute it, and GET
// /v1/runs/{id} — the same endpoint ctl watch polls — reports succeeded
// with the full attempt history.
func TestSubmitFanoutRunsToCompletion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	// The root credential doubles as the bearer here (implicit admin
	// covers submit + read) — the end-to-end test needs no stored key.
	rootKey := mintTestKey(t)
	handler, err := api.New(s, time.Now, nil, rootKey, api.RateLimitOptions{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// The production dispatch path, wired as cmd/worker wires it.
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
	// fanout.json's llm steps route to the offline mock provider
	// (model mock/sim-1), so the fleet needs a mock-backed registry to
	// run them — the same wiring cmd/worker does from AGENTLOOM_LLM_MOCK_ENABLED.
	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	for _, name := range []string{"worker-a", "worker-b"} {
		eng, err := engine.New(s, exec.Builtins(providers), name, engine.WithDispatchNudge(d.Nudge))
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		h.Spawn(name, eng.Handle, queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1})
	}

	var sub api.SubmitRunResponse
	status := postJSON(t, srv, rootKey, submitBody(t, fanoutJSON(t), `{"topic": "durable execution"}`), &sub)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}

	// Poll the read endpoint exactly like ctl watch does.
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
		t.Fatalf("run status = %q, want succeeded (%+v)", run.Run.Status, run)
	}
	if run.Run.StepsSucceeded != 6 {
		t.Errorf("steps_succeeded = %d, want 6", run.Run.StepsSucceeded)
	}
	for _, step := range run.Steps {
		if step.Status != store.StepStatusSucceeded {
			t.Errorf("step %s status = %q, want succeeded", step.ID, step.Status)
		}
		if len(step.Attempts) != 1 {
			t.Errorf("step %s has %d attempts, want 1", step.ID, len(step.Attempts))
			continue
		}
		if got := step.Attempts[0].Outcome; got != store.StepStatusSucceeded {
			t.Errorf("step %s attempt outcome = %q, want succeeded", step.ID, got)
		}
	}
	for _, e := range run.Edges {
		if e.Resolution != store.EdgeResolutionFired {
			t.Errorf("edge %s->%s resolution = %q, want fired", e.From, e.To, e.Resolution)
		}
	}
	// The tool and retrieve steps still run as dev stubs (8.7/8.8) —
	// visible in their outputs.
	for _, id := range []string{"search_docs", "fetch_news"} {
		var out struct {
			Stub bool `json:"stub"`
		}
		step, ok := stepFrom(run, id)
		if !ok {
			t.Fatalf("step %s missing from response", id)
		}
		if err := json.Unmarshal(step.Output, &out); err != nil || !out.Stub {
			t.Errorf("step %s output = %s, want a stub-marked payload (err=%v)", id, step.Output, err)
		}
	}
	// The llm steps ran the production executor against the mock provider:
	// their outputs carry completion text, and the attempt row reports
	// usage through the API (ticket 8.6, criterion 2 — usage visible in the
	// run-status API).
	for _, id := range []string{"brainstorm", "synthesize"} {
		step, ok := stepFrom(run, id)
		if !ok {
			t.Fatalf("step %s missing from response", id)
		}
		var out struct {
			Text  string `json:"text"`
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(step.Output, &out); err != nil {
			t.Fatalf("step %s output %s: %v", id, step.Output, err)
		}
		if out.Text == "" {
			t.Errorf("step %s output has no completion text: %s", id, step.Output)
		}
		var au struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		}
		if len(step.Attempts[0].Usage) == 0 {
			t.Errorf("step %s attempt has no usage in the API response", id)
		} else if err := json.Unmarshal(step.Attempts[0].Usage, &au); err != nil {
			t.Fatalf("step %s attempt usage %s: %v", id, step.Attempts[0].Usage, err)
		} else if au.InputTokens <= 0 || au.OutputTokens <= 0 {
			t.Errorf("step %s attempt usage = %+v, want positive token counts", id, au)
		}
	}

	// Nothing left anywhere: outbox drained, queue quiescent, no duplicate
	// handling per claim.
	tasks, err := s.Outbox().List(ctx, 100)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("outbox not drained: %+v", tasks)
	}
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
}

func stepFrom(run api.RunResponse, id string) (api.StepView, bool) {
	for _, s := range run.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return api.StepView{}, false
}
