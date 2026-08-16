//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// noopStep / plannerAnchorDef build the tight graphs the expansion tests need.

func noopStep(id string) dag.Step { return dag.Step{ID: id, Type: dag.StepNoop} }

// instantiateDef instantiates an inline definition literal and returns the run.
func instantiateDef(t *testing.T, s *store.Store, doc string) gen.Run {
	t.Helper()
	return instantiate(t, s, decodeDef(t, doc))
}

// expandRun runs one ExpandRun in its own transaction (the way 13.3's
// completion transaction will, composed with SucceedStep + fan-out).
func expandRun(t *testing.T, s *store.Store, args store.ExpandRunArgs) (store.ExpandRunResult, error) {
	t.Helper()
	var res store.ExpandRunResult
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var e error
		res, e = store.ExpandRun(ctx, q, args)
		return e
	})
	return res, err
}

func plannerOriginArgs(runID uuid.UUID, origin string, plan dag.PlanOutput) store.ExpandRunArgs {
	return store.ExpandRunArgs{
		RunID:  runID,
		Origin: dag.ExpansionOrigin{Kind: dag.OriginPlanner, StepID: origin},
		Plan:   plan,
		Now:    testNow,
	}
}

// graphExpandedEvents returns the run's graph_expanded event payloads in seq
// order.
func graphExpandedEvents(t *testing.T, s *store.Store, runID uuid.UUID) []store.GraphExpandedEvent {
	t.Helper()
	evs, err := s.Events().List(t.Context(), runID, 0, 500)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var out []store.GraphExpandedEvent
	for _, ev := range evs {
		if ev.Type != store.EventGraphExpanded {
			continue
		}
		var ge store.GraphExpandedEvent
		if err := json.Unmarshal(ev.Payload, &ge); err != nil {
			t.Fatalf("decoding graph_expanded payload: %v", err)
		}
		out = append(out, ge)
	}
	return out
}

// singleDef is a one-step planner-origin definition: the origin is a ready
// entry step, so ExpandRun can splice off it with no prior completion.
const singleOriginDef = `{
  "schema_version": 1, "name": "expand-single",
  "steps": [{"id": "root", "type": "noop"}],
  "edges": []
}`

// TestExpandRunAfterSplice: origin → new. The injected step is inserted pending
// with one dependency; the origin's fan-out (emulated here) readies it.
func TestExpandRunAfterSplice(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run := instantiateDef(t, s, singleOriginDef)

	plan := dag.PlanOutput{
		SchemaVersion: 1,
		Steps:         []dag.Step{noopStep("x")},
		Edges:         []dag.Edge{{From: "root", To: "x"}},
	}
	res, err := expandRun(t, s, plannerOriginArgs(run.ID, "root", plan))
	if err != nil {
		t.Fatalf("ExpandRun: %v", err)
	}
	if len(res.Readied) != 0 {
		t.Errorf("an after-splice step must not ready in ExpandRun; readied=%v", res.Readied)
	}
	if res.GraphVersion != 2 {
		t.Errorf("graph_version = %d, want 2", res.GraphVersion)
	}

	x := stepsByID(t, s, run.ID)["x"]
	if x.Status != store.StepStatusPending || x.RemainingDeps != 1 {
		t.Errorf("x status=%q remaining=%d, want pending/1", x.Status, x.RemainingDeps)
	}
	if x.Depth != 1 || x.OriginStep == nil || *x.OriginStep != "root" || x.OriginKind == nil || *x.OriginKind != "planner" {
		t.Errorf("x provenance wrong: depth=%d origin=%v/%v", x.Depth, x.OriginStep, x.OriginKind)
	}
	if x.GraphVersion != 2 {
		t.Errorf("x graph_version = %d, want 2", x.GraphVersion)
	}

	run2, err := s.Runs().Get(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if run2.GraphVersion != 2 || run2.StepsTotal != 2 {
		t.Errorf("run graph_version=%d steps_total=%d, want 2/2", run2.GraphVersion, run2.StepsTotal)
	}

	ges := graphExpandedEvents(t, s, run.ID)
	if len(ges) != 1 {
		t.Fatalf("want 1 graph_expanded event, got %d", len(ges))
	}
	if ges[0].FromVersion != 1 || ges[0].ToVersion != 2 || ges[0].OriginStep != "root" || ges[0].Depth != 1 {
		t.Errorf("graph_expanded payload wrong: %+v", ges[0])
	}
	if len(ges[0].Delta.Steps) != 1 || ges[0].Delta.Steps[0].ID != "x" {
		t.Errorf("delta did not carry the injected step: %+v", ges[0].Delta)
	}

	// Emulate the origin's fan-out: resolve root → x (fired) and ready x.
	edge := edgeByEndpoints(t, s, run.ID, "root", "x")
	err = s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		res, err := store.ResolveEdge(ctx, q, store.ResolveEdgeArgs{RunID: run.ID, Ordinal: edge.Ordinal, Fired: true, Now: testNow})
		if err != nil {
			return err
		}
		_, err = store.ReadyStep(ctx, q, store.ReadyStepArgs{RunID: run.ID, StepID: res.Edge.ToStep, Now: testNow})
		return err
	})
	if err != nil {
		t.Fatalf("emulated fan-out: %v", err)
	}
	if got := stepsByID(t, s, run.ID)["x"]; got.Status != store.StepStatusReady {
		t.Errorf("after fan-out x status=%q, want ready", got.Status)
	}
	assertCountersMatchEdges(t, s, run.ID)
}

// TestExpandRunBeforeSplice: new → existing-pending. The injected step has no
// incoming edges, so ExpandRun readies + outboxes it directly; the existing
// pending anchor gains one dependency.
func TestExpandRunBeforeSplice(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	// root (entry) → sink (pending, remaining_deps 1).
	run := instantiateDef(t, s, `{
      "schema_version": 1, "name": "expand-before",
      "steps": [{"id": "root", "type": "noop"}, {"id": "sink", "type": "noop"}],
      "edges": [{"from": "root", "to": "sink"}]
    }`)

	plan := dag.PlanOutput{
		SchemaVersion: 1,
		Steps:         []dag.Step{noopStep("x")},
		Edges:         []dag.Edge{{From: "x", To: "sink"}},
	}
	res, err := expandRun(t, s, plannerOriginArgs(run.ID, "root", plan))
	if err != nil {
		t.Fatalf("ExpandRun: %v", err)
	}
	if len(res.Readied) != 1 || res.Readied[0] != "x" {
		t.Errorf("zero-indegree x should ready; readied=%v", res.Readied)
	}
	if len(res.Widened) != 1 || res.Widened[0] != "sink" {
		t.Errorf("sink should be widened; widened=%v", res.Widened)
	}

	byID := stepsByID(t, s, run.ID)
	if x := byID["x"]; x.Status != store.StepStatusReady || x.RemainingDeps != 0 {
		t.Errorf("x status=%q remaining=%d, want ready/0", x.Status, x.RemainingDeps)
	}
	if sink := byID["sink"]; sink.RemainingDeps != 2 {
		t.Errorf("sink remaining_deps = %d, want 2 (root + x)", sink.RemainingDeps)
	}
	// x must have an outbox row (it was readied).
	if !hasPendingOutbox(t, s, run.ID, "x") {
		t.Error("readied step x has no outbox row")
	}
	// The event log carries graph_expanded then step_ready(x).
	if ges := graphExpandedEvents(t, s, run.ID); len(ges) != 1 || len(ges[0].Readied) != 1 || ges[0].Readied[0] != "x" {
		t.Errorf("graph_expanded readied wrong: %+v", ges)
	}
}

// TestExpandRunParallelSplice: existing-active → new → existing-join. The
// injected step runs beside an existing branch and fans into an existing join.
func TestExpandRunParallelSplice(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	// root (entry), a (entry, active/ready), gather (join-all, pending on a).
	run := instantiateDef(t, s, `{
      "schema_version": 1, "name": "expand-parallel",
      "steps": [
        {"id": "root", "type": "noop"},
        {"id": "a", "type": "noop"},
        {"id": "gather", "type": "join", "config": {"mode": "all"}}
      ],
      "edges": [{"from": "a", "to": "gather"}]
    }`)

	plan := dag.PlanOutput{
		SchemaVersion: 1,
		Steps:         []dag.Step{noopStep("x")},
		Edges:         []dag.Edge{{From: "a", To: "x"}, {From: "x", To: "gather"}},
	}
	res, err := expandRun(t, s, plannerOriginArgs(run.ID, "root", plan))
	if err != nil {
		t.Fatalf("ExpandRun: %v", err)
	}
	if len(res.Readied) != 0 {
		t.Errorf("x depends on active a; must not ready in ExpandRun; readied=%v", res.Readied)
	}
	byID := stepsByID(t, s, run.ID)
	if x := byID["x"]; x.Status != store.StepStatusPending || x.RemainingDeps != 1 {
		t.Errorf("x status=%q remaining=%d, want pending/1", x.Status, x.RemainingDeps)
	}
	if g := byID["gather"]; g.RemainingDeps != 2 {
		t.Errorf("gather remaining_deps = %d, want 2 (a + x)", g.RemainingDeps)
	}
}

// TestExpandRunConcurrent: two planners expand the same run at once. They
// serialize on the run lock, versions come out strictly ordered, and both
// deltas land.
func TestExpandRunConcurrent(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run := instantiateDef(t, s, singleOriginDef)

	mkPlan := func(id string) dag.PlanOutput {
		return dag.PlanOutput{
			SchemaVersion: 1,
			Steps:         []dag.Step{noopStep(id)},
			Edges:         []dag.Edge{{From: "root", To: id}},
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	ids := []string{"alpha", "beta"}
	start := make(chan struct{})
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			<-start
			_, errs[i] = expandRun(t, s, plannerOriginArgs(run.ID, "root", mkPlan(id)))
		}(i, id)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent expansion %d failed: %v", i, err)
		}
	}
	run2, _ := s.Runs().Get(t.Context(), run.ID)
	if run2.GraphVersion != 3 || run2.StepsTotal != 3 {
		t.Fatalf("after two expansions: graph_version=%d steps_total=%d, want 3/3", run2.GraphVersion, run2.StepsTotal)
	}
	byID := stepsByID(t, s, run.ID)
	versions := []int32{byID["alpha"].GraphVersion, byID["beta"].GraphVersion}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	if versions[0] != 2 || versions[1] != 3 {
		t.Errorf("injected step graph_versions = %v, want strictly ordered 2,3", versions)
	}
	// The two graph_expanded events step the version 1→2 then 2→3, in seq order.
	ges := graphExpandedEvents(t, s, run.ID)
	if len(ges) != 2 {
		t.Fatalf("want 2 graph_expanded events, got %d", len(ges))
	}
	if ges[0].FromVersion != 1 || ges[0].ToVersion != 2 || ges[1].FromVersion != 2 || ges[1].ToVersion != 3 {
		t.Errorf("graph_expanded versions not linear: %+v", ges)
	}
}

// TestExpandRunIDCollisionRejected: a second expansion reusing a committed
// step id is rejected (not a cap — a plan-attributable failure), nothing added.
func TestExpandRunIDCollisionRejected(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run := instantiateDef(t, s, singleOriginDef)

	plan := dag.PlanOutput{SchemaVersion: 1, Steps: []dag.Step{noopStep("dup")}, Edges: []dag.Edge{{From: "root", To: "dup"}}}
	if _, err := expandRun(t, s, plannerOriginArgs(run.ID, "root", plan)); err != nil {
		t.Fatalf("first expansion: %v", err)
	}
	_, err := expandRun(t, s, plannerOriginArgs(run.ID, "root", plan))
	var rej *store.ExpansionRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("want *ExpansionRejectedError for id collision, got %v", err)
	}
	if rej.CapExceeded() {
		t.Error("an id collision is not a cap exhaustion")
	}
	if run2, _ := s.Runs().Get(t.Context(), run.ID); run2.GraphVersion != 2 || run2.StepsTotal != 2 {
		t.Errorf("rejected expansion mutated the run: version=%d total=%d, want 2/2", run2.GraphVersion, run2.StepsTotal)
	}
}

// TestExpandRunCapExceeded: an expansion past a run guard is a permanent
// (CapExceeded) rejection, atomically rolled back.
func TestExpandRunCapExceeded(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	// max_expansions 1: the first expansion is fine, the second exhausts it.
	run := instantiateDef(t, s, `{
      "schema_version": 1, "name": "expand-cap",
      "expansion": {"max_expansions": 1},
      "steps": [{"id": "root", "type": "noop"}],
      "edges": []
    }`)

	first := dag.PlanOutput{SchemaVersion: 1, Steps: []dag.Step{noopStep("a")}, Edges: []dag.Edge{{From: "root", To: "a"}}}
	if _, err := expandRun(t, s, plannerOriginArgs(run.ID, "root", first)); err != nil {
		t.Fatalf("first expansion: %v", err)
	}
	second := dag.PlanOutput{SchemaVersion: 1, Steps: []dag.Step{noopStep("b")}, Edges: []dag.Edge{{From: "root", To: "b"}}}
	_, err := expandRun(t, s, plannerOriginArgs(run.ID, "root", second))
	var rej *store.ExpansionRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("want *ExpansionRejectedError, got %v", err)
	}
	if !rej.CapExceeded() {
		t.Error("max_expansions exhaustion should be CapExceeded")
	}
	if _, ok := stepsByID(t, s, run.ID)["b"]; ok {
		t.Error("cap-rejected step b was inserted")
	}
	if run2, _ := s.Runs().Get(t.Context(), run.ID); run2.GraphVersion != 2 {
		t.Errorf("cap rejection changed graph_version to %d, want 2", run2.GraphVersion)
	}
}

// TestExpandRunRejectionRollsBackCompletion: a rejected ExpandRun composed with
// SucceedStep in one transaction rolls both back — the origin stays running,
// nothing is added.
func TestExpandRunRejectionRollsBackCompletion(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run := instantiateDef(t, s, singleOriginDef)
	claim := *mustClaim(t, s, run.ID, "root").ClaimID

	// A plan referencing an unknown step — a plan-attributable rejection.
	badPlan := dag.PlanOutput{
		SchemaVersion: 1,
		Steps:         []dag.Step{{ID: "x", Type: dag.StepLLM, Config: &dag.LLMConfig{Model: "mock/sim-1", Prompt: "on ${{ steps.ghost.output.text }}"}}},
		Edges:         []dag.Edge{{From: "root", To: "x"}},
	}
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		if _, err := store.SucceedStep(ctx, q, store.SucceedStepArgs{
			RunID: run.ID, StepID: "root", ClaimID: claim, Output: json.RawMessage(`{"ok":true}`), Now: testNow,
		}); err != nil {
			return err
		}
		_, err := store.ExpandRun(ctx, q, plannerOriginArgs(run.ID, "root", badPlan))
		return err
	})
	var rej *store.ExpansionRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("want *ExpansionRejectedError, got %v", err)
	}
	if rej.CapExceeded() {
		t.Error("an unknown-ref rejection is not a cap")
	}
	// SucceedStep rolled back with the rejected expansion: root is still running.
	root := stepsByID(t, s, run.ID)["root"]
	if root.Status != store.StepStatusRunning {
		t.Errorf("root status = %q, want running (SucceedStep should have rolled back)", root.Status)
	}
	if run2, _ := s.Runs().Get(t.Context(), run.ID); run2.GraphVersion != 1 || run2.StepsTotal != 1 {
		t.Errorf("rejected completion changed run to version=%d total=%d, want 1/1", run2.GraphVersion, run2.StepsTotal)
	}
	if len(graphExpandedEvents(t, s, run.ID)) != 0 {
		t.Error("a rejected expansion appended a graph_expanded event")
	}
}

// TestExpandRunGuards covers the input/state guards.
func TestExpandRunGuards(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run := instantiateDef(t, s, singleOriginDef)
	plan := dag.PlanOutput{SchemaVersion: 1, Steps: []dag.Step{noopStep("x")}, Edges: []dag.Edge{{From: "root", To: "x"}}}

	// ErrNoTx: ExpandRun outside a transaction.
	if _, err := store.ExpandRun(t.Context(), s, plannerOriginArgs(run.ID, "root", plan)); !errors.Is(err, store.ErrNoTx) {
		t.Errorf("ExpandRun outside a tx: got %v, want ErrNoTx", err)
	}
	// Missing origin.
	if _, err := expandRun(t, s, plannerOriginArgs(run.ID, "nope", plan)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing origin: got %v, want ErrNotFound", err)
	}

	// A run that is not running|parked refuses expansion — a cancelling run
	// must quiesce, not grow.
	if err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.CancelRun(ctx, q, store.CancelRunArgs{RunID: run.ID, Reason: store.RunCancelReasonManual, Now: testNow})
		return err
	}); err != nil {
		t.Fatalf("cancelling the run: %v", err)
	}
	_, err := expandRun(t, s, plannerOriginArgs(run.ID, "root", plan))
	wantConflict(t, err, store.ConflictRunNotRunning)
}

// TestExpandRunDepthPropagates: a planner injected at depth d produces depth
// d+1 steps, and the run's max_depth guard bites the deepest.
func TestExpandRunDepthPropagates(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run := instantiateDef(t, s, `{
      "schema_version": 1, "name": "expand-depth",
      "expansion": {"max_depth": 2},
      "steps": [{"id": "root", "type": "noop"}],
      "edges": []
    }`)

	// Depth 0 origin → depth-1 child "c1".
	if _, err := expandRun(t, s, plannerOriginArgs(run.ID, "root",
		dag.PlanOutput{SchemaVersion: 1, Steps: []dag.Step{noopStep("c1")}, Edges: []dag.Edge{{From: "root", To: "c1"}}})); err != nil {
		t.Fatalf("depth-1 expansion: %v", err)
	}
	if got := stepsByID(t, s, run.ID)["c1"].Depth; got != 1 {
		t.Fatalf("c1 depth = %d, want 1", got)
	}
	// c1 (depth 1) → depth-2 child "c2": allowed (max_depth 2).
	if _, err := expandRun(t, s, plannerOriginArgs(run.ID, "c1",
		dag.PlanOutput{SchemaVersion: 1, Steps: []dag.Step{noopStep("c2")}, Edges: []dag.Edge{{From: "c1", To: "c2"}}})); err != nil {
		t.Fatalf("depth-2 expansion: %v", err)
	}
	if got := stepsByID(t, s, run.ID)["c2"].Depth; got != 2 {
		t.Fatalf("c2 depth = %d, want 2", got)
	}
	// c2 (depth 2) → depth-3 child: exceeds max_depth 2, permanent.
	_, err := expandRun(t, s, plannerOriginArgs(run.ID, "c2",
		dag.PlanOutput{SchemaVersion: 1, Steps: []dag.Step{noopStep("c3")}, Edges: []dag.Edge{{From: "c2", To: "c3"}}}))
	var rej *store.ExpansionRejectedError
	if !errors.As(err, &rej) || !rej.CapExceeded() {
		t.Fatalf("depth-3 expansion should be a cap rejection, got %v", err)
	}
}

// edgeByEndpoints finds the run edge between from and to.
func edgeByEndpoints(t *testing.T, s *store.Store, runID uuid.UUID, from, to string) gen.RunEdge {
	t.Helper()
	edges, err := s.Steps().ListEdgesByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("listing edges: %v", err)
	}
	for _, e := range edges {
		if e.FromStep == from && e.ToStep == to {
			return e
		}
	}
	t.Fatalf("no edge %s → %s", from, to)
	return gen.RunEdge{}
}

// hasPendingOutbox reports whether the run has a pending outbox row for stepID.
func hasPendingOutbox(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) bool {
	t.Helper()
	tasks, err := s.Outbox().List(t.Context(), 100)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	for _, task := range tasks {
		if task.RunID == runID && task.StepID == stepID {
			return true
		}
	}
	return false
}
