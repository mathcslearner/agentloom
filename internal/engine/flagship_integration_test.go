//go:build integration

package engine_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/blackboard/pgboard"
	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// decideFlagshipViaAPI stands up the real HTTP decision API over the store and
// POSTs a decision (ticket 15.5 DoD-2: "CI decides via API"). It authenticates
// with a root (admin) key, which implies the approve scope. The httptest
// server shares the run's store, so the decision resumes the same run the
// fleet is executing.
func decideFlagshipViaAPI(t *testing.T, s *store.Store, hq *queuetest.Harness, approvalID uuid.UUID, req api.DecideApprovalRequest) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("entropy: %v", err)
	}
	rootKey := "sk_" + base64.RawURLEncoding.EncodeToString(raw)
	// Wire the delayed-delivery expiry canceller, exactly as cmd/api does: an
	// early human decision ZREMs the approval's pending timeout expiry so it
	// never fires (and the delayed set drains to quiescence).
	h, err := api.New(s, time.Now, nil, rootKey, api.RateLimitOptions{},
		api.WithExpiryCanceller(hq.Delayed()))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal decide request: %v", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/approvals/"+approvalID.String()+":decide", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+rootKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST decide: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("decide via API returned %d: %s", resp.StatusCode, msg)
	}
}

// The M14 flagship e2e (ticket 14.5, ADR-016): the canonical
// research-critic-writer.json runs to convergence through the PRODUCTION
// executor/validator/pricing/blackboard/context stack — a pg_fulltext retrieve
// step grounds a researcher agent, a writer agent drafts under a cost-bearing
// llm_judge with a semantic-retry feedback loop, a critic agent rejects twice
// then approves (unrolling the loop via ExpandRun), an editor finalizes the
// blackboard-head draft, and publish emits the article. Fully offline: local
// Postgres + the deterministic mock (scripted for the critic verdicts and the
// judge scores), zero network. This is the DoD's "runs green in CI (mock)".

// flagshipMock scripts the two agents whose content the loop and the judge
// branch on; the researcher, writer, and editor fall through to the echo mock
// (which passes the assembled context through, so threaded feedback and the
// pinned research brief are visible in each draft). The critic rejects twice
// then approves; the judge fails the writer's first draft (0.4) then passes
// everything (0.9), so the first draft does one semantic retry and every loop
// revision passes the judge on its first attempt.
func flagshipMock() *llm.MockConfig {
	return &llm.MockConfig{Rules: []llm.MockRule{
		{Substring: "editorial critic", Respond: []llm.MockOutcome{
			{Structured: json.RawMessage(`{"verdict":"revise","notes":["ground the claims in the brief"]}`)},
			{Structured: json.RawMessage(`{"verdict":"revise","notes":["tighten the conclusion"]}`)},
			{Structured: json.RawMessage(`{"verdict":"approve"}`)},
		}},
		{Substring: "output-quality judge", Respond: []llm.MockOutcome{
			{Text: `{"score": 0.4, "rationale": "too terse; add detail and cite the brief"}`},
			{Text: `{"score": 0.9, "rationale": "detailed and well grounded"}`},
		}},
	}}
}

// flagshipSpawn wires an engine with the whole production AI-native stack over
// a scripted mock: the retrieve/agent/echo executors, the deterministic +
// llm_judge validators, the pricing catalog, the blackboard (auto-thread +
// context sources), the retriever registry (for retrieval context sources),
// and the summarizer (for summarize compaction). Mirrors cmd/worker.
func flagshipSpawn(t *testing.T, s *store.Store, h *queuetest.Harness, d *engine.Dispatcher, id string, providers *llm.Registry) {
	t.Helper()
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	reg, err := exec.NewRegistry(
		exec.NewRetrieveExecutor(retrievers),
		exec.NewAgentExecutor(providers),
		exec.EchoExecutor{},
		exec.HumanApprovalExecutor{},
	)
	if err != nil {
		t.Fatalf("exec.NewRegistry: %v", err)
	}
	validators, err := validate.NewBuiltins(providers)
	if err != nil {
		t.Fatalf("validate.NewBuiltins: %v", err)
	}
	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	catalog, err := cost.Default()
	if err != nil {
		t.Fatalf("cost.Default: %v", err)
	}
	w, err := engine.New(s, reg, id,
		engine.WithDispatchNudge(d.Nudge),
		engine.WithRetryScheduler(h.Delayed()),
		engine.WithValidators(validators),
		engine.WithPricing(catalog),
		engine.WithBlackboard(board),
		engine.WithRetrievers(retrievers),
		engine.WithSummarizer(contextmgr.NewLLMSummarizer(providers)),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn(id, w.Handle, queuetest.LeaseConfig(500*time.Millisecond))
}

func flagshipDef(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "definitions", "research-critic-writer.json"))
	if err != nil {
		t.Fatalf("reading research-critic-writer.json: %v", err)
	}
	return string(data)
}

// setupFlagship instantiates a run with a REAL created_at (unlike
// setupWithParams, which stamps the frozen testNow): the fixture's
// max_wall_clock guard is enforced against the engine's real clock at claim
// time, so a created_at days in the past would cancel the run before it starts.
func setupFlagship(t *testing.T, defJSON string, params json.RawMessage) (*store.Store, *queuetest.Harness, uuid.UUID) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("decoding definition: %v", err)
	}
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Params: params, Now: time.Now()})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	h.EnsureGroup(t.Context())
	return s, h, res.Run.ID
}

// TestFlagshipResearchCriticWriter is the ticket 14.5 headline: the flagship
// example runs to success on the scripted mock, the writer⇄critic loop unrolls
// two revisions, the judge's semantic retry repairs the first draft, feedback
// threads into each revision, the judge's calls are ledgered as overhead, and
// the run's cost accounting is consistent.
func TestFlagshipResearchCriticWriter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, h, runID := setupFlagship(t, flagshipDef(t), json.RawMessage(`{"topic": "turtles"}`))

	// Seed the retrieval corpus before the run (Ingest is off the step path).
	// ragCorpus is defined in retrieve_integration_test.go (same package).
	if err := pgfts.New(s).Ingest(ctx, ragCorpus); err != nil {
		t.Fatalf("seeding corpus: %v", err)
	}

	d := startDispatcher(t, s, h.Queue())
	mock, err := llm.NewMock(*flagshipMock())
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	providers, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	flagshipSpawn(t, s, h, d, "flagship-a", providers)
	flagshipSpawn(t, s, h, d, "flagship-b", providers)

	// The pipeline runs to the human-approval gate (ticket 15.5): approve_publish
	// parks the run in awaiting_human while the fleet keeps executing. CI then
	// decides through the real HTTP decision API — approve, editing the payload —
	// and the run resumes through publish to success.
	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	approvals, err := s.Approvals().List(ctx, store.ListApprovalsArgs{Status: store.ApprovalStatusPending, Limit: 10})
	if err != nil {
		t.Fatalf("listing pending approvals: %v", err)
	}
	if len(approvals) != 1 {
		t.Fatalf("pending approvals = %d, want 1 (the flagship gate)", len(approvals))
	}
	const approvedText = "The final, human-approved article about turtles."
	decideFlagshipViaAPI(t, s, h, approvals[0].ID, api.DecideApprovalRequest{
		Decision:      string(dag.ApprovalApprove),
		EditedPayload: json.RawMessage(`{"text": "` + approvedText + `"}`),
		Comment:       "Looks good — publishing with a tightened headline.",
	})

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	// The full pipeline ran: research, three writer/critic iterations (loop
	// unrolled twice), the editor finalize, the approval gate, and publish.
	requireStepStatuses(t, s, runID, map[string]string{
		"search":          store.StepStatusSucceeded,
		"research":        store.StepStatusSucceeded,
		"draft":           store.StepStatusSucceeded,
		"draft#1":         store.StepStatusSucceeded,
		"draft#2":         store.StepStatusSucceeded,
		"critique":        store.StepStatusSucceeded,
		"critique#1":      store.StepStatusSucceeded,
		"critique#2":      store.StepStatusSucceeded,
		"finalize":        store.StepStatusSucceeded,
		"approve_publish": store.StepStatusSucceeded,
		"publish":         store.StepStatusSucceeded,
	})

	// The approved, edited payload flowed through the gate into publish.
	if !hasEventType(t, s, runID, store.EventApprovalDecided) {
		t.Error("no approval_decided event after the API decision")
	}

	// Two loop expansions moved the graph to version 3; the injected instances
	// carry loop provenance at constant depth (loops do not nest — ticket 14.3).
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.GraphVersion != 3 {
		t.Errorf("graph_version = %d, want 3 (two loop iterations)", run.GraphVersion)
	}
	steps := requireSteps(t, s, runID)
	for _, id := range []string{"draft#1", "critique#1", "draft#2", "critique#2"} {
		st := steps[id]
		if st.OriginKind == nil || *st.OriginKind != string(dag.OriginLoop) {
			t.Errorf("step %q origin_kind = %v, want loop", id, st.OriginKind)
		}
		if st.Depth != 1 {
			t.Errorf("step %q depth = %d, want 1", id, st.Depth)
		}
	}

	// Feedback threaded: each revised draft's assembled context (echoed back as
	// its output) carries the critic's prior verdict from the thread source.
	for _, id := range []string{"draft#1", "draft#2"} {
		var out struct {
			Text string `json:"text"`
		}
		unmarshalStepOutput(t, s, runID, id, &out)
		if !strings.Contains(out.Text, "revise") {
			t.Errorf("draft %q did not carry the critic's threaded feedback: %q", id, out.Text)
		}
	}

	// The first draft did one semantic retry (judge 0.4 → 0.9); its history is
	// [validation_failed, succeeded]. The loop revisions passed on first attempt.
	atts, err := s.Attempts().ListByStep(ctx, runID, "draft")
	if err != nil {
		t.Fatalf("listing draft attempts: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("draft attempts = %d, want 2 (validation_failed, succeeded)", len(atts))
	}
	if atts[0].Outcome == nil || *atts[0].Outcome != store.AttemptOutcomeValidationFailed {
		t.Errorf("draft attempt 1 outcome = %v, want validation_failed", atts[0].Outcome)
	}
	if atts[1].Outcome == nil || *atts[1].Outcome != store.StepStatusSucceeded {
		t.Errorf("draft attempt 2 outcome = %v, want succeeded", atts[1].Outcome)
	}

	// The judge's provider calls are ledgered as OVERHEAD (ADR-012 rule 4) on
	// the serving writer steps, under judge:* entries, attributed to the judge
	// resource. Every writer instance invokes the judge.
	rows, err := s.Ledger().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("Ledger().ListByRun: %v", err)
	}
	var judgeOverhead, productiveWriter int
	for _, r := range rows {
		switch {
		case r.Overhead:
			if !strings.HasPrefix(r.Entry, "judge:") {
				t.Errorf("overhead row entry = %q, want judge:*", r.Entry)
			}
			if r.Resource != "mock:judge-1" {
				t.Errorf("judge overhead resource = %q, want mock:judge-1", r.Resource)
			}
			judgeOverhead++
		case r.Resource == "mock:sim-1":
			productiveWriter++
		}
	}
	if judgeOverhead == 0 {
		t.Error("no judge overhead ledger rows — the judge's spend was not metered")
	}
	if productiveWriter == 0 {
		t.Error("no productive mock:sim-1 ledger rows")
	}

	// Cost accounting is consistent: run aggregate == exact ledger sum, and the
	// cost_updated event stream is monotonic and lands on the aggregate.
	sum, err := s.Ledger().SumByRun(ctx, runID)
	if err != nil {
		t.Fatalf("Ledger().SumByRun: %v", err)
	}
	if run.SpentNanoUsd != sum.SpentNanoUsd {
		t.Errorf("run spent %d != ledger sum %d", run.SpentNanoUsd, sum.SpentNanoUsd)
	}
	assertCostUpdatedMatchesAggregate(t, s, runID)

	// No budget park and no model downgrade fired (the mock spend is far below
	// the run's budget and the fallback threshold).
	assertNoEvent(t, s, runID, store.EventModelDowngraded)
	assertNoEvent(t, s, runID, store.EventBudgetExceeded)

	// The research brief was pinned to the blackboard, the writer's draft key
	// versioned once per writer instance, and every agent turn auto-appended to
	// the 14.2 handoff thread (research + 3 writer + 3 critic + finalize = 8).
	board, err := pgboard.New(s, pgboard.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("pgboard.New: %v", err)
	}
	rb := board.RunBoard(runID)
	if _, ok, err := rb.Get(ctx, "research_brief"); err != nil {
		t.Fatalf("reading research_brief: %v", err)
	} else if !ok {
		t.Error("research_brief blackboard entry missing")
	}
	draftHist, err := rb.History(ctx, "draft")
	if err != nil {
		t.Fatalf("reading draft history: %v", err)
	}
	if len(draftHist) != 3 {
		t.Errorf("draft blackboard versions = %d, want 3 (one per writer instance)", len(draftHist))
	}
	threadHist, err := rb.History(ctx, blackboard.DefaultThreadKey)
	if err != nil {
		t.Fatalf("reading thread history: %v", err)
	}
	if len(threadHist) != 8 {
		t.Errorf("thread versions = %d, want 8 (one per agent turn)", len(threadHist))
	}

	var pub struct {
		Article string `json:"article"`
		Status  string `json:"status"`
	}
	unmarshalStepOutput(t, s, runID, "publish", &pub)
	if pub.Status != "published" || pub.Article != approvedText {
		t.Errorf("publish output = %+v, want the human-approved edited article %q", pub, approvedText)
	}

	// The writer's context preset was assembled for every writer turn (draft
	// attempts 1 & 2, draft#1, draft#2) and the editor's finalize — the five
	// context-bearing agent turns — each measured against the writer's
	// budget_tokens. (The mock's terse outputs stay under budget, so the
	// declared summarize/drop compaction pipeline does not need to fire here; it
	// engages on the longer conversations a real model produces — see the
	// narrative doc.)
	var asmCount int
	for _, e := range mustEvents(t, s, runID) {
		if e.Type == store.EventContextAssembled {
			asmCount++
		}
	}
	if asmCount != 5 {
		t.Errorf("context_assembled events = %d, want 5 (writer turns + finalize)", asmCount)
	}
}

// mustEvents lists a run's full event log.
func mustEvents(t *testing.T, s *store.Store, runID uuid.UUID) []gen.Event {
	t.Helper()
	evs, err := s.Events().List(t.Context(), runID, 0, 2000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	return evs
}

// assertNoEvent fails if any event of the given type appears in the run's log.
func assertNoEvent(t *testing.T, s *store.Store, runID uuid.UUID, typ string) {
	t.Helper()
	evs, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	for _, e := range evs {
		if e.Type == typ {
			t.Errorf("unexpected %q event", typ)
		}
	}
}

// TestFlagshipLive runs the same flagship example against real providers,
// gated by LIVE_LLM_TESTS=1 and a configured Anthropic key (the internal/llm
// live-smoke precedent). It rewrites the mock/* model ids to real catalog
// models and asserts the pipeline reaches a terminal success with a published
// article — the DoD's "live mode ... smoke-tested once". Iteration count is not
// asserted (a real critic may approve immediately).
func TestFlagshipLive(t *testing.T) {
	if os.Getenv("LIVE_LLM_TESTS") != "1" {
		t.Skip("live provider tests disabled; set LIVE_LLM_TESTS=1 to enable")
	}
	key := firstNonEmpty(os.Getenv("AGENTLOOM_ANTHROPIC_API_KEY"), os.Getenv("ANTHROPIC_API_KEY"))
	if key == "" {
		t.Skip("LIVE_LLM_TESTS=1 but no AGENTLOOM_ANTHROPIC_API_KEY / ANTHROPIC_API_KEY in the environment")
	}
	ctx := t.Context()

	// Rewrite the offline mock ids to real models: the productive agents to
	// Sonnet, the judge and the cheap fallback/summarizer to Haiku.
	defJSON := flagshipDef(t)
	defJSON = strings.ReplaceAll(defJSON, "mock/sim-1", "anthropic/claude-sonnet-5")
	defJSON = strings.ReplaceAll(defJSON, "mock/cheap", "anthropic/claude-haiku-4-5")
	defJSON = strings.ReplaceAll(defJSON, "mock/judge-1", "anthropic/claude-haiku-4-5")
	defJSON = strings.ReplaceAll(defJSON, "mock/judge-2", "anthropic/claude-haiku-4-5")

	s, h, runID := setupFlagship(t, defJSON, json.RawMessage(`{"topic": "the migration patterns of sea turtles"}`))
	if err := pgfts.New(s).Ingest(ctx, ragCorpus); err != nil {
		t.Fatalf("seeding corpus: %v", err)
	}

	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Anthropic: key})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	d := startDispatcher(t, s, h.Queue())
	flagshipSpawn(t, s, h, d, "flagship-live", providers)

	// Real provider latency: allow more time than the mock path. The pipeline
	// parks at the approval gate; approve it via the API to let it finish.
	waitStepTimeout(t, s, runID, "approve_publish", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	}, 4*time.Minute)
	approvals, err := s.Approvals().List(ctx, store.ListApprovalsArgs{Status: store.ApprovalStatusPending, Limit: 10})
	if err != nil {
		t.Fatalf("listing pending approvals: %v", err)
	}
	if len(approvals) != 1 {
		t.Fatalf("pending approvals = %d, want 1", len(approvals))
	}
	decideFlagshipViaAPI(t, s, h, approvals[0].ID, api.DecideApprovalRequest{Decision: string(dag.ApprovalApprove)})

	waitRunTimeout(t, s, runID, store.RunStatusSucceeded, 1*time.Minute)
	h.WaitQuiescent(ctx)

	var pub struct {
		Article string `json:"article"`
		Status  string `json:"status"`
	}
	unmarshalStepOutput(t, s, runID, "publish", &pub)
	if pub.Status != "published" || pub.Article == "" {
		t.Errorf("live publish output = %+v, want a non-empty published article", pub)
	}
	t.Logf("live flagship article (%d chars):\n%s", len(pub.Article), pub.Article)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// waitRunTimeout is waitRun with a caller-chosen deadline, for the live path
// where real provider latency exceeds the mock waiter's 15s budget.
func waitRunTimeout(t *testing.T, s *store.Store, runID uuid.UUID, want string, timeout time.Duration) {
	t.Helper()
	ctx := t.Context()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run, err := s.Runs().Get(ctx, runID)
		if err != nil {
			t.Fatalf("reading run: %v", err)
		}
		if run.Status == want {
			return
		}
		if run.Status != store.RunStatusRunning && run.Status != store.RunStatusParked {
			t.Fatalf("run reached %q, want %q", run.Status, want)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("run never reached %q within %s", want, timeout)
}

// waitStepTimeout is waitStep with a caller-supplied deadline (the live test
// tolerates real provider latency reaching the gate).
func waitStepTimeout(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, cond func(gen.RunStep) bool, timeout time.Duration) {
	t.Helper()
	ctx := t.Context()
	deadline := time.Now().Add(timeout)
	var step gen.RunStep
	for time.Now().Before(deadline) {
		var err error
		step, err = s.Steps().Get(ctx, runID, stepID)
		if err == nil && cond(step) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("step %q never satisfied condition within %s (status %q)", stepID, timeout, step.Status)
}
