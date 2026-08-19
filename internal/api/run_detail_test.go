package api

// Contract goldens for the run-detail projections the step inspector (ticket
// 18.3) consumes. Like the run-graph fixture (13.6) these drive the pure
// builders over fixture rows — no database, deterministic timestamps — and
// pin the wire shape against committed JSON the frontend inspector tests read
// as ground truth. Regenerate with UPDATE_GOLDEN=1.
//
//   - TestRunDetailFixtureGolden      -> testdata/run_detail_fixture.json
//   - TestRunCostFixtureGolden        -> testdata/run_cost_fixture.json
//   - TestStepLogsFixtureGolden       -> testdata/step_logs_fixture.json
//
// The detail fixture exercises every inspector case in one run: a
// semantic-retry llm step (attempt 1 validation_failed with a verdict +
// feedback + config, attempt 2 succeeded); a reclaimed step whose `lost`
// attempt and successor ran on different workers (18.3 DoD-3); a downgraded
// step with usage; and a dead-lettered step.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

func iptr(i int64) *int64 { return &i }

// assertGolden marshals v and compares it to testdata/<name>, regenerating
// under UPDATE_GOLDEN=1.
func assertGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading golden (run with UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s does not match golden\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// detailFixtureRunID is fixed so effects.Key(runID, stepID) is deterministic
// in the golden.
var detailFixtureRunID = uuid.MustParse("22222222-2222-2222-2222-222222222222")

// runDetailFixture builds the rows buildRunResponse projects. Timestamps are
// fixed; worker ids, verdicts, feedback, config, and outcomes are the inspector
// contract.
func runDetailFixture() (gen.Run, []gen.RunStep, []gen.RunEdge, []gen.StepAttempt, []gen.DeadLetter) {
	runID := detailFixtureRunID
	t0 := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	at := func(sec int) *time.Time { u := t0.Add(time.Duration(sec) * time.Second); return &u }

	run := gen.Run{
		ID:               runID,
		Status:           "failed",
		OnFailure:        "continue_independent_branches",
		GraphVersion:     1,
		NextSeq:          20,
		StepsTotal:       4,
		StepsSucceeded:   2,
		StepsFailed:      1,
		CreatedAt:        t0,
		StartedAt:        at(0),
		FinishedAt:       at(30),
		SpentNanoUsd:     4200000,
		OnBudgetExceeded: "park",
	}

	claimA := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	claimB := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
	claimC := uuid.MustParse("cccccccc-0000-0000-0000-000000000003")
	claimD := uuid.MustParse("dddddddd-0000-0000-0000-000000000004")
	claimE := uuid.MustParse("eeeeeeee-0000-0000-0000-000000000005")

	steps := []gen.RunStep{
		// draft: a semantic-retry llm step with a validation chain.
		{
			RunID: runID, StepID: "draft", StepType: "llm", Status: "succeeded",
			GraphVersion: 1, AttemptCount: 2, StartedAt: at(0), FinishedAt: at(10),
			Config: json.RawMessage(`{"model":"mock/sim-1","prompt":"Write a blurb and end with APPROVED.","max_tokens":256}`),
			Output: json.RawMessage(`{"model":"mock/sim-1","text":"Launch blurb. APPROVED"}`),
		},
		// crunch: a reclaimed step — attempt 1 lost on worker-a, attempt 2
		// succeeded on worker-b.
		{
			RunID: runID, StepID: "crunch", StepType: "tool", Status: "succeeded",
			GraphVersion: 1, AttemptCount: 2, StartedAt: at(2), FinishedAt: at(18),
			Config: json.RawMessage(`{"tool":"json_transform","args":{"expr":".x"}}`),
			Output: json.RawMessage(`{"result":42}`),
		},
		// summarize: a downgraded llm step (a model_downgraded event names the
		// swap; the attempt carries usage).
		{
			RunID: runID, StepID: "summarize", StepType: "llm", Status: "succeeded",
			GraphVersion: 1, AttemptCount: 1, StartedAt: at(19), FinishedAt: at(24),
			Config: json.RawMessage(`{"model":"mock/expensive","prompt":"Summarize.","max_tokens":128,"model_fallbacks":[{"model":"mock/cheap","at_budget_fraction":0.5}]}`),
			Output: json.RawMessage(`{"model":"mock/cheap","text":"Summary."}`),
		},
		// boom: a dead-lettered step (permanent failure).
		{
			RunID: runID, StepID: "boom", StepType: "tool", Status: "dead_lettered",
			GraphVersion: 1, AttemptCount: 1, StartedAt: at(25), FinishedAt: at(26),
			Config: json.RawMessage(`{"tool":"http_request","args":{"url":"https://blocked.example"}}`),
			Error:  json.RawMessage(`{"class":"permanent","message":"host not allowed"}`),
		},
	}

	edges := []gen.RunEdge{
		{RunID: runID, Ordinal: 0, FromStep: "draft", ToStep: "crunch", EdgeType: "normal", Resolution: "fired", GraphVersion: 1},
	}

	sptrLocal := func(s string) *string { return &s }
	attempts := []gen.StepAttempt{
		// draft attempt 1: validation_failed, carries a verdict; attempt 2 the
		// feedback-augmented re-attempt that succeeded.
		{
			RunID: runID, StepID: "draft", AttemptNo: 1, ClaimID: claimA,
			Outcome: sptrLocal("validation_failed"), StartedAt: at(0), FinishedAt: at(4),
			WorkerID: sptrLocal("worker-alpha"),
			Usage:    json.RawMessage(`{"input_tokens":12,"output_tokens":8}`),
			Verdict:  json.RawMessage(`{"schema_version":1,"status":"fail","issues":[{"validator":"contains","code":"missing_substring","path":"/text","message":"output does not contain \"APPROVED\""}],"results":[{"validator":"contains","status":"fail"}]}`),
		},
		{
			RunID: runID, StepID: "draft", AttemptNo: 2, ClaimID: claimB,
			Outcome: sptrLocal("succeeded"), StartedAt: at(5), FinishedAt: at(10),
			WorkerID: sptrLocal("worker-alpha"),
			Usage:    json.RawMessage(`{"input_tokens":40,"output_tokens":11}`),
			Verdict:  json.RawMessage(`{"schema_version":1,"status":"pass","results":[{"validator":"contains","status":"pass"}]}`),
			Feedback: json.RawMessage(`{"schema_version":1,"semantic_attempt":2,"max_attempts":3,"prior_attempt":1,"text":"This is attempt 2 of 3. Your previous draft was rejected:\noutput does not contain \"APPROVED\"\n\nRevise it and end with APPROVED once it is ready."}`),
		},
		// crunch: reclaimed — attempt 1 lost on worker-a, attempt 2 on worker-b.
		{
			RunID: runID, StepID: "crunch", AttemptNo: 1, ClaimID: claimC,
			Outcome: sptrLocal("lost"), StartedAt: at(2), FinishedAt: at(12),
			WorkerID: sptrLocal("worker-alpha"),
		},
		{
			RunID: runID, StepID: "crunch", AttemptNo: 2, ClaimID: claimD,
			Outcome: sptrLocal("succeeded"), StartedAt: at(13), FinishedAt: at(18),
			WorkerID: sptrLocal("worker-bravo"),
		},
		// summarize: one attempt after the downgrade, carries usage.
		{
			RunID: runID, StepID: "summarize", AttemptNo: 1, ClaimID: claimE,
			Outcome: sptrLocal("succeeded"), StartedAt: at(19), FinishedAt: at(24),
			WorkerID: sptrLocal("worker-bravo"),
			Usage:    json.RawMessage(`{"input_tokens":30,"output_tokens":6}`),
		},
		// boom: one permanent-failure attempt.
		{
			RunID: runID, StepID: "boom", AttemptNo: 1, ClaimID: uuid.MustParse("ffffffff-0000-0000-0000-000000000006"),
			Outcome: sptrLocal("permanent"), StartedAt: at(25), FinishedAt: at(26),
			WorkerID: sptrLocal("worker-alpha"),
			Error:    json.RawMessage(`{"class":"permanent","message":"host not allowed"}`),
		},
	}

	deadLetters := []gen.DeadLetter{
		{
			RunID: runID, StepID: "boom", Seq: 1, Source: "permanent",
			Class: sptrLocal("permanent"), Error: json.RawMessage(`{"class":"permanent","message":"host not allowed"}`),
			AttemptsAtDeath: 1, CreatedAt: *at(26),
		},
	}

	return run, steps, edges, attempts, deadLetters
}

func TestRunDetailFixtureGolden(t *testing.T) {
	t.Parallel()
	run, steps, edges, attempts, dls := runDetailFixture()
	resp := buildRunResponse(run, steps, edges, attempts, dls, nil)
	assertGolden(t, "run_detail_fixture.json", resp)

	// The idempotency key on each step is the derived effects key — assert it
	// is exactly that (not some other opaque value), so a consumer can rederive.
	byID := map[string]StepView{}
	for _, s := range resp.Steps {
		byID[s.ID] = s
	}
	for _, id := range []string{"draft", "crunch", "summarize", "boom"} {
		want := effects.Key(detailFixtureRunID, id)
		if byID[id].IdempotencyKey != want {
			t.Errorf("step %s idempotency_key = %q, want %q", id, byID[id].IdempotencyKey, want)
		}
	}

	// DoD-3: the reclaimed step names both workers in its attempt history.
	workers := map[string]bool{}
	for _, a := range byID["crunch"].Attempts {
		workers[a.WorkerID] = true
	}
	if !workers["worker-alpha"] || !workers["worker-bravo"] {
		t.Errorf("crunch attempt workers = %v, want both worker-alpha and worker-bravo", workers)
	}
}

func TestRunCostFixtureGolden(t *testing.T) {
	t.Parallel()
	runID := detailFixtureRunID
	t0 := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	run := gen.Run{
		ID: runID, Status: "succeeded", OnBudgetExceeded: "park",
		SpentNanoUsd: 4200000, SavedNanoUsd: 300000,
		BudgetNanoUsd: iptr(100000000),
	}
	byStep := []gen.AggregateCostByStepRow{
		{StepID: "draft", Entries: 3, SpentNanoUsd: 3000000, SavedNanoUsd: 0, OverheadNanoUsd: 1000000},
		{StepID: "summarize", Entries: 2, SpentNanoUsd: 1200000, SavedNanoUsd: 300000, OverheadNanoUsd: 0},
	}
	byResource := []gen.AggregateCostByResourceRow{
		{Resource: "mock:sim-1", Entries: 2, InputTokens: 52, OutputTokens: 19, SpentNanoUsd: 2000000},
		{Resource: "mock:cheap", Entries: 1, InputTokens: 30, OutputTokens: 6, SpentNanoUsd: 1200000, SavedNanoUsd: 300000},
	}
	entries := []gen.CostLedger{
		{
			RunID: runID, StepID: "draft", Attempt: 1, Entry: "0", Resource: "mock:sim-1",
			Usage: json.RawMessage(`{"input_tokens":12,"output_tokens":8}`), Rate: json.RawMessage(`{"input_per_mtok":0.5,"output_per_mtok":1.5}`),
			RateSource: "catalog", CostNanoUsd: 1000000, CreatedAt: t0.Add(4 * time.Second),
		},
		{
			RunID: runID, StepID: "draft", Attempt: 2, Entry: "0", Resource: "mock:sim-1",
			Usage: json.RawMessage(`{"input_tokens":40,"output_tokens":11}`), Rate: json.RawMessage(`{"input_per_mtok":0.5,"output_per_mtok":1.5}`),
			RateSource: "catalog", CostNanoUsd: 1000000, CreatedAt: t0.Add(10 * time.Second),
		},
		// An llm_judge overhead row on the same attempt (ADR-012 rule 4).
		{
			RunID: runID, StepID: "draft", Attempt: 2, Entry: "judge:0", Resource: "mock:cheap",
			Usage: json.RawMessage(`{"input_tokens":20,"output_tokens":4}`), Rate: json.RawMessage(`{"input_per_mtok":0.1,"output_per_mtok":0.2}`),
			RateSource: "catalog", Overhead: true, CostNanoUsd: 1000000, CreatedAt: t0.Add(10 * time.Second),
		},
		// A cache-hit row: cost 0, a counterfactual saved figure.
		{
			RunID: runID, StepID: "summarize", Attempt: 1, Entry: "0", Resource: "mock:cheap",
			Usage: json.RawMessage(`{"input_tokens":30,"output_tokens":6,"cache_hit":true}`), Rate: json.RawMessage(`{"input_per_mtok":0.1,"output_per_mtok":0.2}`),
			RateSource: "catalog", CacheHit: true, CostNanoUsd: 0, SavedNanoUsd: 300000, CreatedAt: t0.Add(24 * time.Second),
		},
	}
	resp := buildRunCostResponse(run, byStep, byResource, entries)
	assertGolden(t, "run_cost_fixture.json", resp)
}

func TestStepLogsFixtureGolden(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 8, 18, 9, 0, 1, 0, time.UTC)
	resp := StepLogsResponse{
		RunID:   detailFixtureRunID.String(),
		StepID:  "draft",
		Attempt: 2,
		Lines: []StepLogLineView{
			{Seq: 1, Level: "info", Message: "calling model", Fields: json.RawMessage(`{"model":"mock/sim-1"}`), TraceID: "0af7651916cd43dd8448eb211c80319c", LoggedAt: t0},
			{Seq: 2, Level: "debug", Message: "prompt assembled", Fields: json.RawMessage(`{"chars":41}`), LoggedAt: t0.Add(1 * time.Millisecond)},
			{Seq: 4, Level: "warn", Message: "retrying after transient error", LoggedAt: t0.Add(2 * time.Millisecond)},
		},
		Truncated:    true,
		DroppedLines: 1,
	}
	assertGolden(t, "step_logs_fixture.json", resp)
}
