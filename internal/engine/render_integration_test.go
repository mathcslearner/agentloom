//go:build integration

package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 8.2's integration suite: template rendering in the live
// claim→render→execute→complete pipeline, across two workers and the
// production dispatcher.

// setupWithParams is setup with submitted run parameters.
func setupWithParams(t *testing.T, defJSON string, params json.RawMessage) (*store.Store, *queuetest.Harness, uuid.UUID) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("decoding definition: %v", err)
	}
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Params: params, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	h.EnsureGroup(t.Context())
	return s, h, res.Run.ID
}

// requireStepOutput asserts a step's recorded output equals want (JSON
// compared semantically).
func requireStepOutput(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, want string) {
	t.Helper()
	step, err := s.Steps().Get(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("reading step %q: %v", stepID, err)
	}
	if !jsonEqual(t, step.Output, json.RawMessage(want)) {
		t.Errorf("step %q output = %s, want %s", stepID, step.Output, want)
	}
}

// TestTemplatePipelineDataFlow is the ticket 8.2 headline: the canonical
// echo_pipeline.json fixture executes end-to-end, and every step's
// recorded output proves what rendering resolved — params in, nested
// paths and interpolation across one hop, whole objects and the full
// function set across two hops.
func TestTemplatePipelineDataFlow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	defJSON, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "echo_pipeline.json"))
	if err != nil {
		t.Fatalf("reading echo_pipeline.json: %v", err)
	}
	s, h, runID := setupWithParams(t, string(defJSON),
		json.RawMessage(`{"greeting": "hello", "audience": {"name": "world"}}`))
	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"compose":  store.StepStatusSucceeded,
		"headline": store.StepStatusSucceeded,
		"package":  store.StepStatusSucceeded,
	})
	requireSingleAttempts(t, s, runID, []string{"compose", "headline", "package"})

	// Hop 1: params rendered in, the object param preserved as JSON.
	requireStepOutput(t, s, runID, "compose",
		`{"text": "hello", "audience": {"name": "world"}}`)
	// Hop 2: nested paths + string interpolation.
	requireStepOutput(t, s, runID, "headline", `"hello, world!"`)
	// Hop 3: multi-hop references (two steps upstream), whole-object
	// pass-through, and get/default/toJson/truncate.
	requireStepOutput(t, s, runID, "package", `{
		"headline": "hello, world!",
		"teaser": "hello, wor",
		"bundle": {"text": "hello", "audience": {"name": "world"}},
		"audience_json": "{\"name\":\"world\"}",
		"signoff": "kind regards"
	}`)

	// The stored configs still carry the authored templates: rendering is
	// per-execution, never written back.
	stored, err := s.Steps().Get(ctx, runID, "headline")
	if err != nil {
		t.Fatalf("reading stored config: %v", err)
	}
	if !strings.Contains(string(stored.Config), "${{ steps.compose.output.text }}") {
		t.Errorf("stored config lost its template: %s", stored.Config)
	}
}

const missingRefDef = `{
	"schema_version": 1,
	"name": "missing-ref",
	"steps": [
		{"id": "a", "type": "echo", "config": {"input": {"present": 1}}},
		{"id": "b", "type": "echo", "config": {"input": "${{ steps.a.output.absent }}"}}
	],
	"edges": [{"from": "a", "to": "b"}]
}`

// TestTemplateMissingRefDeadLettersPermanent: a reference that passes the
// static lint (the step exists and is upstream) but names a path the
// recorded output does not have is a deterministic runtime failure —
// judged permanent (ADR-006), dead-lettered without a retry, failing the
// run under the default fail_fast policy.
func TestTemplateMissingRefDeadLettersPermanent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, missingRefDef)
	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{
		"a": store.StepStatusSucceeded,
		"b": store.StepStatusDeadLettered,
	})

	// Exactly one attempt with the permanent outcome: no retry budget was
	// spent on an error that cannot change.
	attempts, err := s.Attempts().ListByStep(ctx, runID, "b")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.AttemptOutcomePermanent {
		t.Fatalf("attempts of b = %+v, want exactly one with outcome permanent", attempts)
	}

	// One DLQ row, source permanent, with the typed missing-reference
	// message (including the referenced step's status diagnostic).
	letters, err := s.DeadLetters().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(letters) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(letters))
	}
	dl := letters[0]
	if dl.StepID != "b" || dl.Source != store.DeadLetterSourcePermanent {
		t.Errorf("dead letter = step %q source %q, want step b source permanent", dl.StepID, dl.Source)
	}
	if msg := string(dl.Error); !strings.Contains(msg, "did not resolve") || !strings.Contains(msg, "steps.a.output.absent") {
		t.Errorf("dead letter error %s does not carry the missing-reference diagnostic", msg)
	}
}

const templatedSleepDef = `{
	"schema_version": 1,
	"name": "templated-sleep",
	"params": {"nap": {"type": "string", "required": true}},
	"steps": [{"id": "zzz", "type": "sleep", "config": {"duration": "${{ run.params.nap }}"}}],
	"edges": []
}`

// TestTemplatedSleepDurationExecutes: the validation carve-out for
// templated durations holds end-to-end — the literal check deferred at
// submit time is re-run by the executor against the rendered value.
func TestTemplatedSleepDurationExecutes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setupWithParams(t, templatedSleepDef, json.RawMessage(`{"nap": "10ms"}`))
	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	requireStepOutput(t, s, runID, "zzz", `{"slept": "10ms"}`)
}
