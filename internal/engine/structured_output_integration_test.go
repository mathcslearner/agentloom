//go:build integration

package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 11.3 headline e2e (ADR-013): the structured-output pipeline. A
// scripted mock returns a fenced-and-trailing-comma completion, a prose
// completion, and (by default) a native structured echo; the llm executor's
// JSON-repair pass and the implicit json_schema validator turn those into a
// succeeded run with repaired provenance, a validation_failed dead-letter with
// unrepairable provenance, and a native-provenance success respectively — with
// the repaired JSON flowing to a downstream step.

// structuredFixture wires a store, harness, scripted mock provider, llm +
// echo executors, and the real json_schema validator (for the implicit
// output-format validator) into one engine.
type structuredFixture struct {
	s *store.Store
	h *queuetest.Harness
}

func newStructuredFixture(t *testing.T) *structuredFixture {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	// Scripted mock: a messy fenced/trailing-comma answer for "repair-me", a
	// prose non-answer for "prose-me"; everything else falls to the default
	// structured echo (native JSON under a ResponseFormat request).
	mock, err := llm.NewMock(llm.MockConfig{
		Rules: []llm.MockRule{
			{Substring: "repair-me", Respond: []llm.MockOutcome{{Text: "```json\n{\"title\": \"repaired\",}\n```"}}},
			{Substring: "prose-me", Respond: []llm.MockOutcome{{Text: "I cannot produce that."}}},
		},
	})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(providers), exec.EchoExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validate.NewJSONSchema())
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "structured-worker",
		engine.WithDispatchNudge(d.Nudge), engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("structured-worker", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return &structuredFixture{s: s, h: h}
}

func (f *structuredFixture) submit(t *testing.T, def *dag.Definition) uuid.UUID {
	t.Helper()
	res, err := f.s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return res.Run.ID
}

// requireSteps loads a run's steps into a map keyed by step id.
func requireSteps(t *testing.T, s *store.Store, runID uuid.UUID) map[string]gen.RunStep {
	t.Helper()
	rows, err := s.Steps().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	m := make(map[string]gen.RunStep, len(rows))
	for _, r := range rows {
		m[r.StepID] = r
	}
	return m
}

func (f *structuredFixture) genRepair(t *testing.T, runID uuid.UUID) *exec.Repair {
	t.Helper()
	atts, err := f.s.Attempts().ListByStep(t.Context(), runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(atts))
	}
	if len(atts[0].Repair) == 0 {
		t.Fatal("attempt carried no repair provenance")
	}
	var r exec.Repair
	if err := json.Unmarshal(atts[0].Repair, &r); err != nil {
		t.Fatalf("unmarshaling repair: %v", err)
	}
	return &r
}

// TestStructuredOutputRepairedSucceedsAndFlows: a fenced-and-trailing-comma
// completion is repaired into valid JSON, passes the implicit json_schema
// validator, and the repaired `.json.title` flows into a downstream echo step.
func TestStructuredOutputRepairedSucceedsAndFlows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newStructuredFixture(t)

	def := mustDecode(t, `{
		"schema_version": 1,
		"name": "structured-repaired",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "repair-me", "max_tokens": 64, "temperature": 0,
			   "output_format": {"type": "json_schema",
			     "schema": {"type": "object", "properties": {"title": {"type": "string"}}, "required": ["title"]}}}},
			{"id": "use", "type": "echo",
			 "config": {"input": {"got": "${{ steps.gen.output.json.title }}"}}}
		],
		"edges": [{"from": "gen", "to": "use"}]
	}`)
	runID := f.submit(t, def)

	waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	r := f.genRepair(t, runID)
	if r.Status != "repaired" || len(r.Steps) == 0 {
		t.Fatalf("repair provenance = %+v, want repaired with steps", r)
	}
	if r.RawText == "" {
		t.Error("repaired provenance should retain the raw text")
	}

	// The gen output carries the repaired structured JSON. The `text` field is
	// the canonical compact JSON (a string value, preserved verbatim); the
	// `json` field is a nested object, so it is compared semantically (JSONB
	// storage does not preserve whitespace).
	steps := requireSteps(t, f.s, runID)
	var gen struct {
		Text string `json:"text"`
		JSON struct {
			Title string `json:"title"`
		} `json:"json"`
	}
	if err := json.Unmarshal(steps["gen"].Output, &gen); err != nil {
		t.Fatalf("unmarshaling gen output: %v", err)
	}
	if gen.Text != `{"title":"repaired"}` {
		t.Errorf("gen.text = %q, want canonical compact JSON", gen.Text)
	}
	if gen.JSON.Title != "repaired" {
		t.Errorf("gen.json.title = %q, want repaired", gen.JSON.Title)
	}

	// The downstream step received the repaired title through templating.
	var use struct {
		Got string `json:"got"`
	}
	if err := json.Unmarshal(steps["use"].Output, &use); err != nil {
		t.Fatalf("unmarshaling use output: %v", err)
	}
	if use.Got != "repaired" {
		t.Errorf("downstream got = %q, want the repaired title to flow through", use.Got)
	}
}

// TestStructuredOutputUnrepairableDeadLetters: a prose completion cannot be
// repaired, so the implicit json_schema validator raises invalid_json, the
// step dead-letters as validation_failed, and the attempt carries unrepairable
// provenance.
func TestStructuredOutputUnrepairableDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newStructuredFixture(t)

	def := mustDecode(t, `{
		"schema_version": 1,
		"name": "structured-unrepairable",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "prose-me", "max_tokens": 64, "temperature": 0,
			   "output_format": {"type": "json"}}}
		],
		"edges": []
	}`)
	runID := f.submit(t, def)

	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	requireStepStatuses(t, f.s, runID, map[string]string{"gen": store.StepStatusDeadLettered})

	atts, err := f.s.Attempts().ListByStep(ctx, runID, "gen")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 1 || atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomeValidationFailed {
		t.Fatalf("attempts = %+v, want one validation_failed", atts)
	}
	var v validate.Verdict
	if err := json.Unmarshal(atts[0].Verdict, &v); err != nil {
		t.Fatalf("unmarshaling verdict: %v", err)
	}
	found := false
	for _, iss := range v.Issues {
		if iss.Validator == "json_schema" && iss.Code == "invalid_json" {
			found = true
		}
	}
	if !found {
		t.Fatalf("verdict issues = %+v, want an invalid_json issue", v.Issues)
	}
	r := f.genRepair(t, runID)
	if r.Status != "unrepairable" {
		t.Fatalf("repair provenance = %+v, want unrepairable", r)
	}
}

// TestStructuredOutputNativeEcho: with no scripted rule, the mock answers a
// ResponseFormat request with native structured JSON; a plain `json` format
// (parseability only) passes and records native provenance.
func TestStructuredOutputNativeEcho(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newStructuredFixture(t)

	def := mustDecode(t, `{
		"schema_version": 1,
		"name": "structured-native",
		"steps": [
			{"id": "gen", "type": "llm",
			 "config": {"model": "mock/sim-1", "prompt": "hello native", "max_tokens": 64, "temperature": 0,
			   "output_format": {"type": "json"}}}
		],
		"edges": []
	}`)
	runID := f.submit(t, def)

	waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	r := f.genRepair(t, runID)
	if r.Status != "native" {
		t.Fatalf("repair provenance = %+v, want native", r)
	}
	steps := requireSteps(t, f.s, runID)
	var gen struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(steps["gen"].Output, &gen); err != nil {
		t.Fatalf("unmarshaling gen output: %v", err)
	}
	if !json.Valid(gen.JSON) || len(gen.JSON) == 0 {
		t.Errorf("gen.json = %s, want valid native JSON", gen.JSON)
	}
}
