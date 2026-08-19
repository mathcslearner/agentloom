//go:build integration

package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

var fixturesDir = filepath.Join("..", "..", "examples", "definitions")

// loadFixture decodes one canonical example definition.
func loadFixture(t *testing.T, name string) *dag.Definition {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, name)) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	def, err := dag.Decode(data)
	if err != nil {
		t.Fatalf("decoding fixture %s: %v", name, err)
	}
	return def
}

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// stepsByID indexes a run's steps for assertion convenience.
func stepsByID(t *testing.T, s *store.Store, runID uuid.UUID) map[string]gen.RunStep {
	t.Helper()
	steps, err := s.Steps().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	byID := make(map[string]gen.RunStep, len(steps))
	for _, st := range steps {
		byID[st.StepID] = st
	}
	return byID
}

// countRows counts the rows each instantiation-touched table holds.
func countRows(t *testing.T, s *store.Store) map[string]int {
	t.Helper()
	ctx := t.Context()
	counts := map[string]int{}
	runs, err := s.Runs().List(ctx, 100)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	counts["runs"] = len(runs)
	for _, run := range runs {
		steps, err := s.Steps().ListByRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("listing steps: %v", err)
		}
		edges, err := s.Steps().ListEdgesByRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("listing edges: %v", err)
		}
		events, err := s.Events().List(ctx, run.ID, 0, 100)
		if err != nil {
			t.Fatalf("listing events: %v", err)
		}
		counts["run_steps"] += len(steps)
		counts["run_edges"] += len(edges)
		counts["events"] += len(events)
	}
	outbox, err := s.Outbox().List(ctx, 100)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	counts["task_outbox"] = len(outbox)
	return counts
}

func TestCreateRunFanout(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	def := loadFixture(t, "fanout.json")

	defID := mustCreateDefinition(t, s, def)
	res, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition:   def,
		DefinitionID: &defID,
		Params:       json.RawMessage(`{"topic":"durable execution"}`),
		Now:          testNow,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if res.Reused {
		t.Fatal("fresh submission reported Reused")
	}
	if want := []string{"start"}; !slices.Equal(res.EntrySteps, want) {
		t.Fatalf("entry steps = %q, want %q", res.EntrySteps, want)
	}

	run := res.Run
	if run.Status != store.RunStatusRunning {
		t.Errorf("run status = %q, want running", run.Status)
	}
	if run.StepsTotal != 6 {
		t.Errorf("steps_total = %d, want 6", run.StepsTotal)
	}
	if run.StartedAt == nil || !run.StartedAt.Equal(testNow) {
		t.Errorf("started_at = %v, want %v", run.StartedAt, testNow)
	}
	if run.DefinitionID == nil || *run.DefinitionID != defID {
		t.Errorf("definition_id = %v, want %v", run.DefinitionID, defID)
	}
	snapshot, err := dag.Encode(def)
	if err != nil {
		t.Fatalf("encoding definition: %v", err)
	}
	if !jsonEqual(t, run.Definition, snapshot) {
		t.Error("stored definition snapshot differs from the submitted definition")
	}

	// Dependency bookkeeping: entry 0, fan-out branches 1 each, the join
	// its three parents, synthesize its one.
	byID := stepsByID(t, s, run.ID)
	wantDeps := map[string]int32{
		"start": 0, "search_docs": 1, "fetch_news": 1, "brainstorm": 1,
		"gather": 3, "synthesize": 1,
	}
	for id, want := range wantDeps {
		step, ok := byID[id]
		if !ok {
			t.Fatalf("step %q not instantiated", id)
		}
		if step.RemainingDeps != want {
			t.Errorf("step %q remaining_deps = %d, want %d", id, step.RemainingDeps, want)
		}
		if step.FiredDeps != 0 {
			t.Errorf("step %q fired_deps = %d, want 0", id, step.FiredDeps)
		}
		wantStatus := store.StepStatusPending
		if id == "start" {
			wantStatus = store.StepStatusReady
		}
		if step.Status != wantStatus {
			t.Errorf("step %q status = %q, want %q", id, step.Status, wantStatus)
		}
		if step.GraphVersion != 1 {
			t.Errorf("step %q graph_version = %d, want 1", id, step.GraphVersion)
		}
		// updated_at is app-written from the injected clock (ADR-004
		// timestamp policy) — the reconciler's staleness scan depends on
		// tests being able to control it, so a database now() here is a bug.
		if !step.UpdatedAt.Equal(testNow) {
			t.Errorf("step %q updated_at = %v, want the injected %v", id, step.UpdatedAt, testNow)
		}
	}
	if byID["gather"].StepType != string(dag.StepJoin) {
		t.Errorf("gather step_type = %q, want join", byID["gather"].StepType)
	}
	if byID["start"].Config != nil {
		t.Errorf("noop step config = %s, want NULL", byID["start"].Config)
	}
	if byID["search_docs"].Config == nil {
		t.Error("retrieve step config not materialized")
	}

	// Edges mirror the definition in declaration (ordinal) order.
	edges, err := s.Steps().ListEdgesByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("listing edges: %v", err)
	}
	if len(edges) != len(def.Edges) {
		t.Fatalf("instantiated %d edges, want %d", len(edges), len(def.Edges))
	}
	for i, e := range edges {
		if e.Ordinal != int32(i) {
			t.Errorf("edge %d ordinal = %d", i, e.Ordinal)
		}
		if e.FromStep != def.Edges[i].From || e.ToStep != def.Edges[i].To {
			t.Errorf("edge %d = %s->%s, want %s->%s", i, e.FromStep, e.ToStep, def.Edges[i].From, def.Edges[i].To)
		}
		if e.Resolution != store.EdgeResolutionUnresolved {
			t.Errorf("edge %d resolution = %q, want unresolved", i, e.Resolution)
		}
	}

	// Events: run_created at seq 1, then one step_ready for the entry step.
	events, err := s.Events().List(ctx, run.ID, 0, 10)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Seq != 1 || events[0].Type != store.EventRunCreated {
		t.Errorf("event 1 = (%d, %q), want (1, run_created)", events[0].Seq, events[0].Type)
	}
	wantCreated := json.RawMessage(fmt.Sprintf(`{"name":"fanout-fanin","definition_id":%q,"steps_total":6}`, defID))
	if !jsonEqual(t, events[0].Payload, wantCreated) {
		t.Errorf("run_created payload = %s", events[0].Payload)
	}
	if events[1].Seq != 2 || events[1].Type != store.EventStepReady {
		t.Errorf("event 2 = (%d, %q), want (2, step_ready)", events[1].Seq, events[1].Type)
	}
	if !jsonEqual(t, events[1].Payload, json.RawMessage(`{"step_id":"start"}`)) {
		t.Errorf("step_ready payload = %s", events[1].Payload)
	}

	// Outbox: exactly one dispatch, for the entry step.
	outbox, err := s.Outbox().List(ctx, 10)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	if len(outbox) != 1 {
		t.Fatalf("got %d outbox rows, want 1", len(outbox))
	}
	if outbox[0].RunID != run.ID || outbox[0].StepID != "start" || outbox[0].Reason != store.OutboxReasonStepReady {
		t.Errorf("outbox row = (%s, %q, %q)", outbox[0].RunID, outbox[0].StepID, outbox[0].Reason)
	}
}

// mustCreateDefinition registers def in workflow_definitions and returns
// the row id.
func mustCreateDefinition(t *testing.T, s *store.Store, def *dag.Definition) uuid.UUID {
	t.Helper()
	spec, err := dag.Encode(def)
	if err != nil {
		t.Fatalf("encoding definition: %v", err)
	}
	row, err := s.Definitions().Create(t.Context(), gen.CreateDefinitionParams{
		Name: def.Name, Version: 1, Spec: spec,
	})
	if err != nil {
		t.Fatalf("creating definition: %v", err)
	}
	return row.ID
}

func TestCreateRunLoopEdgesExcluded(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	def := loadFixture(t, "critic_loop.json")

	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if want := []string{"draft"}; !slices.Equal(res.EntrySteps, want) {
		t.Fatalf("entry steps = %q, want %q", res.EntrySteps, want)
	}

	// draft's only incoming edge is the loop edge — excluded from
	// bookkeeping, so draft is an entry step with zero deps.
	byID := stepsByID(t, s, res.Run.ID)
	if byID["draft"].RemainingDeps != 0 || byID["draft"].Status != store.StepStatusReady {
		t.Errorf("draft = (deps %d, %q), want (0, ready)", byID["draft"].RemainingDeps, byID["draft"].Status)
	}
	if byID["critique"].RemainingDeps != 1 || byID["publish"].RemainingDeps != 1 {
		t.Errorf("critique/publish deps = %d/%d, want 1/1", byID["critique"].RemainingDeps, byID["publish"].RemainingDeps)
	}

	// The loop edge row carries its predicates; the when-edge its own.
	edges, err := s.Steps().ListEdgesByRun(t.Context(), res.Run.ID)
	if err != nil {
		t.Fatalf("listing edges: %v", err)
	}
	loop := edges[1]
	if loop.EdgeType != store.EdgeTypeLoop {
		t.Fatalf("edge 1 type = %q, want loop", loop.EdgeType)
	}
	if loop.Condition == nil || *loop.Condition != "output.json.verdict == 'revise'" {
		t.Errorf("loop condition = %v", loop.Condition)
	}
	if loop.MaxIterations == nil || *loop.MaxIterations != 5 {
		t.Errorf("loop max_iterations = %v", loop.MaxIterations)
	}
	// The exit edge (critique -> publish) is unconditioned: the loop condition
	// is the sole brancher, and the before-splice keeps publish pending until the
	// terminal iteration fires this edge (ticket 14.3).
	if when := edges[2].WhenExpr; when != nil {
		t.Errorf("edge 2 (exit) when = %v, want unconditioned", when)
	}
	if edges[0].WhenExpr != nil || edges[0].Condition != nil || edges[0].MaxIterations != nil {
		t.Error("unconditioned normal edge has non-NULL predicates")
	}
}

func TestCreateRunAllFixtures(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	files, err := filepath.Glob(filepath.Join(fixturesDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing fixtures: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		def := loadFixture(t, filepath.Base(f))
		res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
		if err != nil {
			t.Errorf("%s: CreateRun: %v", filepath.Base(f), err)
			continue
		}
		if len(res.EntrySteps) == 0 {
			t.Errorf("%s: no entry steps", filepath.Base(f))
		}
		if int(res.Run.StepsTotal) != len(def.Steps) {
			t.Errorf("%s: steps_total = %d, want %d", filepath.Base(f), res.Run.StepsTotal, len(def.Steps))
		}
	}
}

func TestCreateRunIdempotency(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	def := loadFixture(t, "linear.json")

	first, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: def, IdempotencyToken: "tok-1", Now: testNow,
	})
	if err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}
	second, err := s.CreateRun(ctx, store.CreateRunArgs{
		Definition: def, IdempotencyToken: "tok-1", Now: testNow.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("second CreateRun: %v", err)
	}
	if !second.Reused {
		t.Error("duplicate submission not reported Reused")
	}
	if second.Run.ID != first.Run.ID {
		t.Errorf("duplicate returned run %s, want original %s", second.Run.ID, first.Run.ID)
	}
	if !slices.Equal(second.EntrySteps, first.EntrySteps) {
		t.Errorf("reused entry steps = %q, want %q", second.EntrySteps, first.EntrySteps)
	}
	if got := countRows(t, s); got["runs"] != 1 {
		t.Errorf("runs = %d, want 1", got["runs"])
	}

	// A different token and no token both create fresh runs.
	other, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, IdempotencyToken: "tok-2", Now: testNow})
	if err != nil {
		t.Fatalf("distinct-token CreateRun: %v", err)
	}
	if other.Reused || other.Run.ID == first.Run.ID {
		t.Error("distinct token did not create a fresh run")
	}
	for range 2 { // two tokenless runs coexist: NULLs are unique-exempt
		res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
		if err != nil {
			t.Fatalf("tokenless CreateRun: %v", err)
		}
		if res.Reused {
			t.Error("tokenless submission reported Reused")
		}
	}
}

func TestCreateRunIdempotencyRace(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	def := loadFixture(t, "linear.json")

	const racers = 8
	results := make([]store.CreateRunResult, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = s.CreateRun(t.Context(), store.CreateRunArgs{
				Definition: def, IdempotencyToken: "race-tok", Now: testNow,
			})
		}()
	}
	close(start)
	wg.Wait()

	var winners int
	for i := range racers {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if !results[i].Reused {
			winners++
		}
		if results[i].Run.ID != results[0].Run.ID {
			t.Errorf("racer %d got run %s, want %s", i, results[i].Run.ID, results[0].Run.ID)
		}
	}
	if winners != 1 {
		t.Errorf("%d racers created the run, want exactly 1", winners)
	}
	if got := countRows(t, s); got["runs"] != 1 {
		t.Errorf("runs = %d, want 1", got["runs"])
	}
}

// TestCreateRunAllOrNothing arms the failpoint at every stage and asserts
// an aborted instantiation leaves zero rows anywhere. Not parallel: the
// failpoint is a package global.
func TestCreateRunAllOrNothing(t *testing.T) {
	s := newStore(t)
	def := loadFixture(t, "fanout.json")
	injected := errors.New("injected mid-transaction failure")

	stages := []string{
		store.StageAfterRunInsert, store.StageAfterSteps,
		store.StageAfterEdges, store.StageAfterEvents, store.StageAfterOutbox,
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			store.SetInstantiateFailpoint(t, func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			})
			_, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
			if !errors.Is(err, injected) {
				t.Fatalf("CreateRun error = %v, want the injected failure", err)
			}
			for table, n := range countRows(t, s) {
				if n != 0 {
					t.Errorf("%s has %d rows after aborted instantiation, want 0", table, n)
				}
			}
		})
	}

	t.Run("panic", func(t *testing.T) {
		store.SetInstantiateFailpoint(t, func(s string) error {
			if s == store.StageAfterEdges {
				panic("injected panic")
			}
			return nil
		})
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("CreateRun did not propagate the panic")
				}
			}()
			_, _ = s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
		}()
		for table, n := range countRows(t, s) {
			if n != 0 {
				t.Errorf("%s has %d rows after panicked instantiation, want 0", table, n)
			}
		}
	})

	// The failpoint must not have leaked into the store's success path.
	t.Run("recovers", func(t *testing.T) {
		if _, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow}); err != nil {
			t.Fatalf("CreateRun after failpoint tests: %v", err)
		}
	})
}

func TestCreateRunRejections(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	if _, err := s.CreateRun(ctx, store.CreateRunArgs{Now: testNow}); err == nil {
		t.Error("nil definition accepted")
	}
	if _, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: loadFixture(t, "linear.json")}); err == nil {
		t.Error("zero Now accepted")
	}
	invalid := &dag.Definition{
		SchemaVersion: dag.CurrentSchemaVersion,
		Name:          "bad",
		Steps:         []dag.Step{{ID: "a", Type: dag.StepNoop}},
		Edges:         []dag.Edge{{From: "a", To: "missing"}},
	}
	if _, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: invalid, Now: testNow}); err == nil {
		t.Error("invalid definition accepted")
	}
	if got := countRows(t, s); got["runs"] != 0 {
		t.Errorf("rejected submissions wrote %d runs", got["runs"])
	}
}
