//go:build integration

package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/tools"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// Ticket 13.6's run-graph introspection contract test: the canonical planner
// example runs a real expansion to completion on the mock fleet, and GET
// /v1/runs/{id}/graph returns the versioned graph with per-row provenance plus
// the per-version expansion delta — and the graph_expanded events and the API
// versions agree. planner.json injects two llm workers (work_a/work_b) that
// depend on the planner and fan into a pre-existing join, so the run reaches
// graph_version 2 / steps_total 5. Fully offline (the mock's structured echo
// returns the planner's prompt — itself a valid PlanOutput — verbatim).
func TestRunGraphIntrospection(t *testing.T) {
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

	d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{Interval: 10 * time.Millisecond, Batch: 16})
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
	toolReg, err := tools.NewBuiltins(tools.HTTPOptions{})
	if err != nil {
		t.Fatalf("tools.NewBuiltins: %v", err)
	}
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	validators, err := validate.NewBuiltins(providers)
	if err != nil {
		t.Fatalf("validate.NewBuiltins: %v", err)
	}
	eng, err := engine.New(s, exec.Builtins(providers, toolReg, retrievers), "graph-worker",
		engine.WithDispatchNudge(d.Nudge), engine.WithValidators(validators))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("graph-worker", eng.Handle, queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1})

	def, err := os.ReadFile("../../examples/definitions/planner.json")
	if err != nil {
		t.Fatalf("reading planner.json: %v", err)
	}
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, rootKey, submitBody(t, def, ""), &sub); status != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", status)
	}

	// Poll to completion.
	deadline := time.Now().Add(20 * time.Second)
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

	var graph api.RunGraphResponse
	if status := getJSON(t, srv, rootKey, "/v1/runs/"+sub.RunID+"/graph", &graph); status != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s/graph = %d, want 200", sub.RunID, status)
	}

	// One committed expansion: version 2, 5 steps (3 authored + 2 injected).
	if graph.GraphVersion != 2 {
		t.Errorf("graph_version = %d, want 2 (one expansion)", graph.GraphVersion)
	}
	if graph.StepsTotal != 5 {
		t.Errorf("steps_total = %d, want 5", graph.StepsTotal)
	}
	if len(graph.Nodes) != 5 {
		t.Fatalf("nodes = %d, want 5:\n%+v", len(graph.Nodes), graph.Nodes)
	}

	nodes := map[string]api.GraphNodeView{}
	for _, n := range graph.Nodes {
		nodes[n.ID] = n
	}
	// Authored nodes carry definition provenance at version 1, depth 0.
	for _, id := range []string{"plan", "gather", "report"} {
		n, ok := nodes[id]
		if !ok {
			t.Errorf("missing authored node %q", id)
			continue
		}
		if n.Origin.Kind != "definition" || n.Origin.Step != "" {
			t.Errorf("node %q origin = %+v, want definition", id, n.Origin)
		}
		if n.GraphVersion != 1 || n.Depth != 0 {
			t.Errorf("node %q version/depth = %d/%d, want 1/0", id, n.GraphVersion, n.Depth)
		}
		if !n.AddedAt.Equal(run.Run.CreatedAt) {
			t.Errorf("authored node %q added_at = %s, want run created_at %s", id, n.AddedAt, run.Run.CreatedAt)
		}
	}
	// Injected worker nodes carry planner provenance at version 2, depth 1.
	for _, id := range []string{"work_a", "work_b"} {
		n, ok := nodes[id]
		if !ok {
			t.Errorf("missing injected node %q", id)
			continue
		}
		if n.Origin.Kind != "planner" || n.Origin.Step != "plan" {
			t.Errorf("node %q origin = %+v, want planner/plan", id, n.Origin)
		}
		if n.GraphVersion != 2 || n.Depth != 1 {
			t.Errorf("node %q version/depth = %d/%d, want 2/1", id, n.GraphVersion, n.Depth)
		}
		if n.Status != store.StepStatusSucceeded {
			t.Errorf("injected node %q status = %q, want succeeded", id, n.Status)
		}
	}

	// Exactly one expansion delta, naming the planner and its injected steps.
	if len(graph.Expansions) != 1 {
		t.Fatalf("expansions = %d, want 1:\n%+v", len(graph.Expansions), graph.Expansions)
	}
	e := graph.Expansions[0]
	if e.Version != 2 || e.FromVersion != 1 {
		t.Errorf("expansion version/from = %d/%d, want 2/1", e.Version, e.FromVersion)
	}
	if e.OriginStep != "plan" || e.OriginKind != "planner" {
		t.Errorf("expansion origin = %s/%s, want plan/planner", e.OriginStep, e.OriginKind)
	}
	assertContains(t, "added_steps", e.AddedSteps, "work_a", "work_b")
	// The injected time on the delta matches the injected nodes' added_at.
	if wa := nodes["work_a"]; !wa.AddedAt.Equal(e.AddedAt) {
		t.Errorf("work_a added_at = %s, expansion added_at = %s (should match)", wa.AddedAt, e.AddedAt)
	}

	// Consistency: current version = 1 + number of expansions, and every node's
	// introducing version is 1 or belongs to an expansion.
	if graph.GraphVersion != 1+len(graph.Expansions) {
		t.Errorf("graph_version %d != 1 + expansions %d", graph.GraphVersion, len(graph.Expansions))
	}
	versions := map[int]bool{1: true}
	for _, exp := range graph.Expansions {
		versions[exp.Version] = true
	}
	for _, n := range graph.Nodes {
		if !versions[n.GraphVersion] {
			t.Errorf("node %q graph_version %d has no matching expansion", n.ID, n.GraphVersion)
		}
	}
	for _, ed := range graph.Edges {
		if !versions[ed.GraphVersion] {
			t.Errorf("edge %s->%s graph_version %d has no matching expansion", ed.From, ed.To, ed.GraphVersion)
		}
	}

	// Unknown run → 404.
	if status := getJSON(t, srv, rootKey, "/v1/runs/"+uuidStr()+"/graph", nil); status != http.StatusNotFound {
		t.Errorf("GET graph for unknown run = %d, want 404", status)
	}
}

// assertContains fails unless every wanted id appears in got.
func assertContains(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("%s missing %q; got %v", label, w, got)
		}
	}
}
