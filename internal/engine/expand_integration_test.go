//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 13.3 headline e2e (ADR-015): the planner executor. A planner is an
// llm-family step whose validated PlanOutput is spliced into the running graph
// atomically with its completion (store.ExpandRun composed in completeSuccess).
// These tests prove the three acceptance criteria — plan → injected steps
// execute → join continues → run succeeds; a malformed plan semantic-retries to
// a valid one with the rejection issues as feedback; a zombie planner cannot
// double-expand — plus cap-exceeded → permanent and expansion atomicity.

// The plan the happy-path planner returns: two llm workers spliced after the
// planner (plan → work_*) and fanning into the pre-existing gather join
// (work_* → gather, a "before" splice widening the join).
const plannerGoodPlan = `{"schema_version":1,` +
	`"steps":[` +
	`{"id":"work_a","type":"llm","config":{"model":"mock/sim-1","prompt":"analyze A","max_tokens":64,"temperature":0}},` +
	`{"id":"work_b","type":"llm","config":{"model":"mock/sim-1","prompt":"analyze B","max_tokens":64,"temperature":0}}` +
	`],"edges":[` +
	`{"from":"plan","to":"work_a"},{"from":"plan","to":"work_b"},` +
	`{"from":"work_a","to":"gather"},{"from":"work_b","to":"gather"}]}`

// A plan whose injected step id collides with an existing run step (gather):
// schema-valid, so it passes the implicit validator, but rejected by
// ValidateExpansion — plan-attributable (not a cap), so it semantic-retries.
const plannerBadPlan = `{"schema_version":1,` +
	`"steps":[{"id":"gather","type":"noop"}],` +
	`"edges":[{"from":"plan","to":"gather"}]}`

// plannerDef is the standard planner graph: plan (planner) → gather (join all)
// → report (echo). The planner splices two workers that also fan into gather.
const plannerDef = `{
	"schema_version": 1,
	"name": "planner-e2e",
	"steps": [
		{"id": "plan", "type": "planner",
		 "config": {"model": "mock/sim-1", "prompt": "make-a-plan", "max_tokens": 512, "temperature": 0},
		 "validation": {"max_attempts": 3}},
		{"id": "gather", "type": "join", "config": {"mode": "all"}},
		{"id": "report", "type": "echo", "config": {"input": {"status": "done"}}}
	],
	"edges": [
		{"from": "plan", "to": "gather"},
		{"from": "gather", "to": "report"}
	]
}`

// plannerFixture wires a store, harness, scripted mock, the planner + llm +
// echo + join executors, and the json_schema validator (the implicit plan
// validator) into one engine driven by a production dispatcher.
type plannerFixture struct {
	s *store.Store
	h *queuetest.Harness
}

func newPlannerFixture(t *testing.T, rules []llm.MockRule) *plannerFixture {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	mock, err := llm.NewMock(llm.MockConfig{Rules: rules})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(
		exec.NewPlannerExecutor(providers), exec.NewLLMExecutor(providers),
		exec.EchoExecutor{}, exec.JoinExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validate.NewJSONSchema())
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	eng, err := engine.New(s, reg, "planner-worker",
		engine.WithDispatchNudge(d.Nudge), engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("planner-worker", eng.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	return &plannerFixture{s: s, h: h}
}

func (f *plannerFixture) submit(t *testing.T, defJSON string) uuid.UUID {
	t.Helper()
	res, err := f.s.CreateRun(t.Context(), store.CreateRunArgs{Definition: mustDecode(t, defJSON), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return res.Run.ID
}

// planRule returns a mock rule that answers a structured request with the given
// plan JSON verbatim (native structured output), matched by call ordinal.
func planRule(onCall int, plan string) llm.MockRule {
	return llm.MockRule{OnCall: onCall, Respond: []llm.MockOutcome{{Structured: json.RawMessage(plan)}}}
}

// graphExpandedEventView is the subset of graph_expanded a test asserts.
// The full store.GraphExpandedEvent carries the delta as a dag.PlanOutput
// whose dag.Step.Config is an interface — not stdlib-unmarshalable — so the
// delta is read as raw and re-decoded with dag.DecodePlanOutput on demand.
type graphExpandedEventView struct {
	OriginStep  string          `json:"origin_step"`
	OriginKind  string          `json:"origin_kind"`
	FromVersion int32           `json:"from_version"`
	ToVersion   int32           `json:"to_version"`
	Depth       int32           `json:"depth"`
	Delta       json.RawMessage `json:"delta"`
	Readied     []string        `json:"readied"`
	Widened     []string        `json:"widened"`
}

// graphExpandedEvents returns the run's graph_expanded event views in seq
// order.
func graphExpandedEvents(t *testing.T, s *store.Store, runID uuid.UUID) []graphExpandedEventView {
	t.Helper()
	evs, err := s.Events().List(t.Context(), runID, 0, 500)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var out []graphExpandedEventView
	for _, ev := range evs {
		if ev.Type != store.EventGraphExpanded {
			continue
		}
		var ge graphExpandedEventView
		if err := json.Unmarshal(ev.Payload, &ge); err != nil {
			t.Fatalf("decoding graph_expanded payload: %v", err)
		}
		out = append(out, ge)
	}
	return out
}

// TestPlannerExpandsAndCompletes is criterion 1: a plan injects two worker
// steps that execute, the downstream join continues once both land, and the
// run succeeds — with the graph grown to version 2, one graph_expanded event,
// and the injected rows carrying planner provenance.
func TestPlannerExpandsAndCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newPlannerFixture(t, []llm.MockRule{planRule(1, plannerGoodPlan)})

	runID := f.submit(t, plannerDef)
	run := waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	// One committed expansion: graph_version 2, steps_total 5 (3 authored + 2
	// injected), one graph_expanded event stepping 1 → 2.
	if run.GraphVersion != 2 {
		t.Errorf("graph_version = %d, want 2 (one expansion)", run.GraphVersion)
	}
	if run.StepsTotal != 5 {
		t.Errorf("steps_total = %d, want 5", run.StepsTotal)
	}
	ges := graphExpandedEvents(t, f.s, runID)
	if len(ges) != 1 {
		t.Fatalf("graph_expanded events = %d, want 1", len(ges))
	}
	ge := ges[0]
	if ge.OriginStep != "plan" || ge.OriginKind != string(dag.OriginPlanner) {
		t.Errorf("graph_expanded origin = %s/%s, want plan/planner", ge.OriginStep, ge.OriginKind)
	}
	if ge.FromVersion != 1 || ge.ToVersion != 2 {
		t.Errorf("graph_expanded version = %d→%d, want 1→2", ge.FromVersion, ge.ToVersion)
	}

	// Every step succeeded, including the two injected workers.
	requireStepStatuses(t, f.s, runID, map[string]string{
		"plan":   store.StepStatusSucceeded,
		"gather": store.StepStatusSucceeded,
		"report": store.StepStatusSucceeded,
		"work_a": store.StepStatusSucceeded,
		"work_b": store.StepStatusSucceeded,
	})

	// The injected rows carry provenance: depth 1, origin plan/planner,
	// stamped at graph_version 2.
	steps := requireSteps(t, f.s, runID)
	for _, id := range []string{"work_a", "work_b"} {
		st := steps[id]
		if st.Depth != 1 {
			t.Errorf("%s depth = %d, want 1", id, st.Depth)
		}
		if st.OriginStep == nil || *st.OriginStep != "plan" {
			t.Errorf("%s origin_step = %v, want plan", id, st.OriginStep)
		}
		if st.OriginKind == nil || *st.OriginKind != string(dag.OriginPlanner) {
			t.Errorf("%s origin_kind = %v, want planner", id, st.OriginKind)
		}
		if st.GraphVersion != 2 {
			t.Errorf("%s graph_version = %d, want 2", id, st.GraphVersion)
		}
	}
	// The planner's own output carries the plan as audit provenance (in
	// output.json). Decoded with the plan codec — dag.Step.Config is an
	// interface, so stdlib unmarshal into dag.PlanOutput would fail.
	var planOut struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(steps["plan"].Output, &planOut); err != nil {
		t.Fatalf("decoding planner output envelope: %v", err)
	}
	auditPlan, err := dag.DecodePlanOutput(planOut.JSON)
	if err != nil {
		t.Fatalf("planner output.json is not a decodable plan: %v", err)
	}
	if len(auditPlan.Steps) != 2 {
		t.Errorf("planner output.json has %d steps, want 2", len(auditPlan.Steps))
	}

	// The graph_expanded event's delta round-trips to the same plan.
	if delta, derr := dag.DecodePlanOutput(ge.Delta); derr != nil || len(delta.Steps) != 2 {
		t.Errorf("graph_expanded delta = (%v, %v), want a 2-step plan", delta, derr)
	}

	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}

// TestPlannerExampleRunsOffline executes the canonical planner.json example
// against the UNSCRIPTED mock (no rules): the planner's prompt is itself a
// valid PlanOutput, which the mock's structured echo returns verbatim, so a
// real expansion runs offline exactly as `make up-app` would drive it.
func TestPlannerExampleRunsOffline(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newPlannerFixture(t, nil) // unscripted: default structured echo

	raw, err := os.ReadFile("../../examples/definitions/planner.json")
	if err != nil {
		t.Fatalf("reading planner.json: %v", err)
	}
	def, err := dag.Decode(raw)
	if err != nil {
		t.Fatalf("decoding planner.json: %v", err)
	}
	res, err := f.s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID

	run := waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	if run.GraphVersion != 2 {
		t.Errorf("graph_version = %d, want 2 (one expansion)", run.GraphVersion)
	}
	requireStepStatuses(t, f.s, runID, map[string]string{
		"plan": store.StepStatusSucceeded, "gather": store.StepStatusSucceeded,
		"report": store.StepStatusSucceeded, "work_a": store.StepStatusSucceeded,
		"work_b": store.StepStatusSucceeded,
	})
	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}

// TestPlannerMalformedPlanSemanticRetries is criterion 2: a first plan is
// rejected against the graph (an id collision — plan-attributable), the planner
// is re-prompted with the rejection issues as feedback, and its second plan
// succeeds — the graph moving exactly once.
func TestPlannerMalformedPlanSemanticRetries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newPlannerFixture(t, []llm.MockRule{
		planRule(1, plannerBadPlan),  // attempt 1: id collides with gather → rejected
		planRule(2, plannerGoodPlan), // attempt 2 (after feedback): valid
	})

	runID := f.submit(t, plannerDef)
	run := waitRun(t, f.s, runID, store.RunStatusSucceeded)
	f.h.WaitQuiescent(ctx)

	// Exactly one expansion committed (the valid plan), despite two attempts.
	if run.GraphVersion != 2 {
		t.Errorf("graph_version = %d, want 2 (one committed expansion)", run.GraphVersion)
	}
	if got := len(graphExpandedEvents(t, f.s, runID)); got != 1 {
		t.Errorf("graph_expanded events = %d, want 1", got)
	}

	// The planner's attempt history: attempt 1 validation_failed (the rejected
	// plan), attempt 2 succeeded.
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "plan")
	if err != nil {
		t.Fatalf("listing planner attempts: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("planner attempts = %d, want 2", len(atts))
	}
	if atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomeValidationFailed {
		t.Errorf("attempt 1 outcome = %v, want validation_failed", atts[0].Outcome)
	}
	if atts[1].Outcome == nil || *atts[1].Outcome != store.StepStatusSucceeded {
		t.Errorf("attempt 2 outcome = %v, want succeeded", atts[1].Outcome)
	}
	// The failing attempt's verdict carries the expansion issue.
	if len(atts[0].Verdict) == 0 || !strings.Contains(string(atts[0].Verdict), "expansion") {
		t.Errorf("attempt 1 verdict does not carry the expansion rejection: %s", atts[0].Verdict)
	}
	// The second attempt was given the critique as feedback (the plan rejection
	// rendered into the re-prompt).
	if len(atts[1].Feedback) == 0 {
		t.Error("attempt 2 carried no feedback — the semantic retry did not inject the critique")
	}

	requireStepStatuses(t, f.s, runID, map[string]string{
		"plan": store.StepStatusSucceeded, "gather": store.StepStatusSucceeded,
		"report": store.StepStatusSucceeded, "work_a": store.StepStatusSucceeded,
		"work_b": store.StepStatusSucceeded,
	})
	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}

// TestPlannerCapExceededPermanent: a plan exceeding the planner's per-expansion
// cap is a run-guard exhaustion (CapExceeded) → permanent dead-letter, never a
// semantic retry (a smaller plan is not re-prompted — the cap is the record).
// The graph never moves.
func TestPlannerCapExceededPermanent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// A 2-step plan against a planner capped at 1 added step.
	f := newPlannerFixture(t, []llm.MockRule{planRule(1, plannerGoodPlan)})

	const cappedDef = `{
		"schema_version": 1,
		"name": "planner-capped",
		"steps": [
			{"id": "plan", "type": "planner",
			 "config": {"model": "mock/sim-1", "prompt": "make-a-plan", "max_tokens": 512, "temperature": 0,
			   "max_added_steps": 1},
			 "validation": {"max_attempts": 3}},
			{"id": "gather", "type": "join", "config": {"mode": "all"}},
			{"id": "report", "type": "echo", "config": {"input": {"status": "done"}}}
		],
		"edges": [{"from": "plan", "to": "gather"}, {"from": "gather", "to": "report"}]
	}`
	runID := f.submit(t, cappedDef)
	waitRun(t, f.s, runID, store.RunStatusFailed)
	f.h.WaitQuiescent(ctx)

	// The graph never grew — the rejected expansion rolled the whole
	// completion back.
	run, err := f.s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.GraphVersion != 1 {
		t.Errorf("graph_version = %d, want 1 (no expansion committed)", run.GraphVersion)
	}
	if got := len(graphExpandedEvents(t, f.s, runID)); got != 0 {
		t.Errorf("graph_expanded events = %d, want 0", got)
	}
	// The planner dead-lettered permanently (source permanent), one attempt —
	// no semantic retry, since a better plan cannot lift a cap.
	atts, err := f.s.Attempts().ListByStep(ctx, runID, "plan")
	if err != nil {
		t.Fatalf("listing planner attempts: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("planner attempts = %d, want 1 (cap failure is not retried)", len(atts))
	}
	if atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomePermanent {
		t.Errorf("attempt outcome = %v, want permanent", atts[0].Outcome)
	}
	dls, err := f.s.DeadLetters().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 || !strings.Contains(string(dls[0].Error), "expansion_cap_exceeded") {
		t.Fatalf("dead letters = %+v, want one carrying expansion_cap_exceeded", dls)
	}
	f.h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, f.s)
}

// TestPlannerExpansionAtomicOnFailpoint: aborting the transaction right after
// ExpandRun (before commit) leaves NO injected rows and an unchanged
// graph_version — the expansion is atomic with the completion (ADR-015 crash
// cell E3). A single Handle drives the planner directly so the abort is
// deterministic.
func TestPlannerExpansionAtomicOnFailpoint(t *testing.T) {
	// Not parallel: arms the package-global completion failpoint.
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	mock, err := llm.NewMock(llm.MockConfig{Rules: []llm.MockRule{planRule(1, plannerGoodPlan)}})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(
		exec.NewPlannerExecutor(providers), exec.NewLLMExecutor(providers),
		exec.EchoExecutor{}, exec.JoinExecutor{})
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	vreg, err := validate.NewRegistry(validate.NewJSONSchema())
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "planner-fp", engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, plannerDef), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID

	// Abort right after ExpandRun; the whole completion must roll back.
	engine.SetCompleteFailpoint(t, func(stage string) error {
		if stage == engine.StageAfterExpand {
			return errors.New("injected: abort after expand")
		}
		return nil
	})

	// Drive the planner claim directly; the failpoint makes Handle return an
	// error (redeliver), committing nothing.
	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("lost drain dispatched %d rows, want 1", n)
	}
	h.Enqueue(ctx, stepEnvelope(runID, "plan"))
	msg := h.ReadOne(ctx, "fp-worker")
	err = eng.Handle(ctx, queue.Delivery{ID: msg.ID, Envelope: stepEnvelope(runID, "plan"), DeliveryCount: 1})
	if err == nil {
		t.Fatal("Handle returned nil — the failpoint abort should have surfaced as a redeliver error")
	}

	// Nothing was committed: the planner is still running (not succeeded), no
	// injected rows, graph_version unchanged.
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.GraphVersion != 1 {
		t.Errorf("graph_version = %d, want 1 (rolled back)", run.GraphVersion)
	}
	if run.StepsTotal != 3 {
		t.Errorf("steps_total = %d, want 3 (no injected rows)", run.StepsTotal)
	}
	steps := requireSteps(t, s, runID)
	if _, ok := steps["work_a"]; ok {
		t.Error("work_a exists — a rolled-back expansion must leave no injected rows")
	}
	if got := len(graphExpandedEvents(t, s, runID)); got != 0 {
		t.Errorf("graph_expanded events = %d, want 0", got)
	}
	plan := steps["plan"]
	if plan.Status == store.StepStatusSucceeded {
		t.Errorf("planner status = succeeded — the completion must have rolled back")
	}
}

// stallingPlanner is a planner-typed executor that stalls after signaling,
// ignoring ctx — the zombie in the double-expand test. It returns a valid plan
// output so the resumed zombie's completion would expand if not fenced.
type stallingPlanner struct {
	started chan struct{}
	release chan struct{}
}

func (*stallingPlanner) Type() string { return string(dag.StepPlanner) }

func (e *stallingPlanner) Execute(context.Context, exec.StepContext) (exec.Output, error) {
	close(e.started)
	<-e.release
	// The zombie's would-be plan output (shaped like the llm executor's json).
	return exec.Output{
		Data:  json.RawMessage(`{"model":"mock/sim-1","stop_reason":"end_turn","text":` + planTextJSON(plannerGoodPlan) + `,"json":` + plannerGoodPlan + `,"usage":{"input_tokens":5,"output_tokens":3}}`),
		Usage: &exec.Usage{InputTokens: 5, OutputTokens: 3},
	}, nil
}

func planTextJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestZombiePlannerCannotDoubleExpand is criterion 3 (ADR-015 crash cell E6):
// worker A claims the planner and stalls past its lease; worker B reclaims,
// takes over, expands, and completes the run. When A resumes, its completion —
// ExpandRun included — is fenced on the lost claim and abandoned. The graph
// expanded exactly once.
func TestZombiePlannerCannotDoubleExpand(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: mustDecode(t, plannerDef), Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID

	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("lost drain dispatched %d rows, want 1", n)
	}
	h.Enqueue(ctx, stepEnvelope(runID, "plan"))
	msg := h.ReadOne(ctx, "worker-a")

	// The validator registry both workers need: a planner step always carries
	// the implicit plan-schema validator, resolved (and run) on both the claim
	// and completion paths — so worker A needs it too, or its planner claim
	// would fail permanently at resolveChain before ever stalling.
	vreg, err := validate.NewRegistry(validate.NewJSONSchema())
	if err != nil {
		t.Fatalf("validate.NewRegistry: %v", err)
	}

	// Worker A: a stalling planner executor. Its Handle runs directly (no
	// heartbeats — the stall).
	stall := &stallingPlanner{started: make(chan struct{}), release: make(chan struct{})}
	regA, err := exec.NewRegistry(stall, exec.EchoExecutor{}, exec.JoinExecutor{})
	if err != nil {
		t.Fatalf("NewRegistry A: %v", err)
	}
	engineA, err := engine.New(s, regA, "worker-a", engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New A: %v", err)
	}
	handleErr := make(chan error, 1)
	go func() {
		handleErr <- engineA.Handle(ctx, queue.Delivery{
			ID: msg.ID, Envelope: stepEnvelope(runID, "plan"), DeliveryCount: 1,
		})
	}()
	<-stall.started

	plan, err := s.Steps().Get(ctx, runID, "plan")
	if err != nil {
		t.Fatalf("reading plan step: %v", err)
	}
	if plan.ClaimID == nil {
		t.Fatal("planner not claimed by worker A")
	}
	claimA := *plan.ClaimID

	// Worker B: a real consumer over the full planner registry with a short
	// lease. It reclaims the silent entry, takes over, expands, and completes.
	mock, err := llm.NewMock(llm.MockConfig{Rules: []llm.MockRule{planRule(1, plannerGoodPlan)}})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry providers: %v", err)
	}
	regB, err := exec.NewRegistry(
		exec.NewPlannerExecutor(providers), exec.NewLLMExecutor(providers),
		exec.EchoExecutor{}, exec.JoinExecutor{})
	if err != nil {
		t.Fatalf("NewRegistry B: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	engineB, err := engine.New(s, regB, "worker-b",
		engine.WithDispatchNudge(d.Nudge), engine.WithValidators(vreg))
	if err != nil {
		t.Fatalf("engine.New B: %v", err)
	}
	h.Spawn("worker-b", engineB.Handle, queuetest.LeaseConfig(400*time.Millisecond))
	run := waitRun(t, s, runID, store.RunStatusSucceeded)

	// B's takeover minted a new claim.
	plan, err = s.Steps().Get(ctx, runID, "plan")
	if err != nil {
		t.Fatalf("reading plan step: %v", err)
	}
	if plan.ClaimID == nil || *plan.ClaimID == claimA {
		t.Fatal("planner still carries worker A's claim after takeover")
	}
	claimB := *plan.ClaimID

	// The graph expanded exactly once (B's expansion).
	if run.GraphVersion != 2 {
		t.Errorf("graph_version = %d, want 2 (single expansion)", run.GraphVersion)
	}
	if got := len(graphExpandedEvents(t, s, runID)); got != 1 {
		t.Errorf("graph_expanded events = %d, want exactly 1", got)
	}

	// Resume the zombie: its completion (ExpandRun included) must be fenced.
	close(stall.release)
	select {
	case err = <-handleErr:
	case <-time.After(15 * time.Second):
		t.Fatal("worker A's Handle did not return after release")
	}
	if err == nil {
		t.Fatal("zombie planner completion returned nil — it would have ACKed and double-expanded")
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("zombie completion error = %v, want a transition conflict (fence)", err)
	}
	if !strings.Contains(err.Error(), claimA.String()) || !strings.Contains(err.Error(), claimB.String()) {
		t.Errorf("fencing rejection %q does not carry both claim IDs (%s, %s)", err, claimA, claimB)
	}

	// Still exactly one expansion; the planner history is [lost, succeeded].
	run, err = s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("re-reading run: %v", err)
	}
	if run.GraphVersion != 2 {
		t.Errorf("graph_version = %d after zombie resume, want 2 (no double-expand)", run.GraphVersion)
	}
	if got := len(graphExpandedEvents(t, s, runID)); got != 1 {
		t.Errorf("graph_expanded events = %d after zombie resume, want 1", got)
	}
	requireAttemptHistory(t, s, runID, "plan", []struct {
		claim   uuid.UUID
		outcome string
	}{
		{claim: claimA, outcome: store.AttemptOutcomeLost},
		{claim: claimB, outcome: store.StepStatusSucceeded},
	})

	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, s)
}

// _ is a compile-time guard that plannerGoodPlan/plannerBadPlan are valid plan
// documents, so a fixture typo fails fast rather than mid-run.
var _ = func() bool {
	for _, p := range []string{plannerGoodPlan, plannerBadPlan} {
		if _, err := dag.DecodePlanOutput([]byte(p)); err != nil {
			panic("planner test plan is not a valid PlanOutput: " + err.Error())
		}
	}
	return true
}()
