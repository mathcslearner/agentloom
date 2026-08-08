//go:build integration

package store_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return store.NewFromPool(storetest.NewDB(t))
}

// jsonEqual compares two JSON documents semantically: Postgres jsonb does
// not preserve formatting (or key order), only content.
func jsonEqual(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshaling %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshaling %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

// mustCreateRun inserts a minimal running run and returns it.
func mustCreateRun(t *testing.T, s *store.Store, mutate func(*gen.CreateRunParams)) gen.Run {
	t.Helper()
	arg := gen.CreateRunParams{
		Definition: json.RawMessage(`{"name":"wf","steps":[]}`),
		Status:     store.RunStatusRunning,
	}
	if mutate != nil {
		mutate(&arg)
	}
	run, err := s.Runs().Create(t.Context(), arg)
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	return run
}

func TestDefinitionsCRUD(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	defs := s.Definitions()

	spec := json.RawMessage(`{"name":"pipeline","steps":[{"id":"a"}]}`)
	created, err := defs.Create(ctx, gen.CreateDefinitionParams{Name: "pipeline", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == uuid.Nil || created.Name != "pipeline" || created.Version != 1 {
		t.Fatalf("created row mismatch: %+v", created)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("created_at not set by the database")
	}

	got, err := defs.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !jsonEqual(t, got.Spec, spec) {
		t.Fatalf("spec round-trip: got %s, want %s", got.Spec, spec)
	}

	byNV, err := defs.GetByNameVersion(ctx, "pipeline", 1)
	if err != nil {
		t.Fatalf("get by name+version: %v", err)
	}
	if byNV.ID != created.ID {
		t.Fatalf("get by name+version returned %s, want %s", byNV.ID, created.ID)
	}

	if _, err := defs.Get(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get of missing id: got %v, want ErrNotFound", err)
	}

	// A second version is a new row; a duplicate (name, version) conflicts.
	if _, err := defs.Create(ctx, gen.CreateDefinitionParams{Name: "pipeline", Version: 2, Spec: spec}); err != nil {
		t.Fatalf("create v2: %v", err)
	}
	_, err = defs.Create(ctx, gen.CreateDefinitionParams{Name: "pipeline", Version: 1, Spec: spec})
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate (name, version): got %v, want ConflictError", err)
	}
	if conflict.Constraint != "workflow_definitions_name_version_key" {
		t.Fatalf("conflict constraint: got %q", conflict.Constraint)
	}

	versions, err := defs.ListVersions(ctx, "pipeline")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("list versions: got %+v", versions)
	}

	all, err := defs.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list: got %d rows, want 2", len(all))
	}

	if err := defs.Delete(ctx, versions[1].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := defs.Delete(ctx, versions[1].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: got %v, want ErrNotFound", err)
	}
}

func TestDefinitionDeleteRestrictedByRuns(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	def, err := s.Definitions().Create(ctx, gen.CreateDefinitionParams{
		Name: "wf", Version: 1, Spec: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create definition: %v", err)
	}
	mustCreateRun(t, s, func(arg *gen.CreateRunParams) { arg.DefinitionID = &def.ID })

	err = s.Definitions().Delete(ctx, def.ID)
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("delete with runs on record: got %v, want ConflictError (RESTRICT)", err)
	}
}

func TestRunsCRUD(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runs := s.Runs()

	token := "submit-1"
	started := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	params := json.RawMessage(`{"topic":"go"}`)
	created := mustCreateRun(t, s, func(arg *gen.CreateRunParams) {
		arg.Params = params
		arg.IdempotencyToken = &token
		arg.StepsTotal = 3
		arg.StartedAt = &started
	})
	if created.ID == uuid.Nil {
		t.Fatal("zero-ID create did not generate a run id")
	}

	got, err := runs.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.RunStatusRunning || !jsonEqual(t, got.Params, params) ||
		got.StepsTotal != 3 || got.GraphVersion != 1 || got.NextSeq != 0 {
		t.Fatalf("run round-trip mismatch: %+v", got)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Fatalf("started_at round-trip: got %v, want %v", got.StartedAt, started)
	}

	byToken, err := runs.GetByIdempotencyToken(ctx, token)
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if byToken.ID != created.ID {
		t.Fatalf("get by token returned %s, want %s", byToken.ID, created.ID)
	}
	if _, err := runs.GetByIdempotencyToken(ctx, "no-such-token"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get by missing token: got %v, want ErrNotFound", err)
	}

	// Duplicate token conflicts; runs without tokens never do.
	_, err = runs.Create(ctx, gen.CreateRunParams{
		Definition: json.RawMessage(`{}`), Status: store.RunStatusRunning, IdempotencyToken: &token,
	})
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate token: got %v, want ConflictError", err)
	}
	mustCreateRun(t, s, nil)
	mustCreateRun(t, s, nil)

	// Caller-supplied id is honored (2.5's idempotent submission needs it).
	wantID := uuid.New()
	supplied := mustCreateRun(t, s, func(arg *gen.CreateRunParams) { arg.ID = wantID })
	if supplied.ID != wantID {
		t.Fatalf("caller-supplied id: got %s, want %s", supplied.ID, wantID)
	}

	all, err := runs.List(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("list: got %d runs, want 4", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.After(all[i-1].CreatedAt) {
			t.Fatal("list not ordered newest-first")
		}
	}
	byStatus, err := runs.ListByStatus(ctx, store.RunStatusSucceeded, 10)
	if err != nil {
		t.Fatalf("list by status: %v", err)
	}
	if len(byStatus) != 0 {
		t.Fatalf("list by status succeeded: got %d runs, want 0", len(byStatus))
	}

	if err := runs.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := runs.Get(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: got %v, want ErrNotFound", err)
	}
	if err := runs.Delete(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: got %v, want ErrNotFound", err)
	}
}

func TestRunDeleteCascadesSubtree(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	run := mustCreateRun(t, s, nil)
	step, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
		RunID: run.ID, StepID: "a", StepType: "llm", Status: store.StepStatusReady, UpdatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	if _, err := s.Steps().CreateEdge(ctx, gen.CreateRunEdgeParams{
		RunID: run.ID, Ordinal: 0, FromStep: "a", ToStep: "b",
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	if _, err := s.Attempts().Create(ctx, gen.CreateStepAttemptParams{
		RunID: run.ID, StepID: step.StepID, AttemptNo: 1, ClaimID: uuid.New(),
	}); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	seq, err := s.Runs().AllocateEventSeq(ctx, run.ID)
	if err != nil {
		t.Fatalf("allocate seq: %v", err)
	}
	if _, err := s.Events().Append(ctx, gen.AppendEventParams{
		RunID: run.ID, Seq: seq, Type: "run_created",
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	if err := s.Runs().Delete(ctx, run.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if steps, err := s.Steps().ListByRun(ctx, run.ID); err != nil || len(steps) != 0 {
		t.Fatalf("steps after cascade: %v, %v", steps, err)
	}
	if edges, err := s.Steps().ListEdgesByRun(ctx, run.ID); err != nil || len(edges) != 0 {
		t.Fatalf("edges after cascade: %v, %v", edges, err)
	}
	if atts, err := s.Attempts().ListByStep(ctx, run.ID, "a"); err != nil || len(atts) != 0 {
		t.Fatalf("attempts after cascade: %v, %v", atts, err)
	}
	if evs, err := s.Events().List(ctx, run.ID, 0, 10); err != nil || len(evs) != 0 {
		t.Fatalf("events after cascade: %v, %v", evs, err)
	}
}

func TestGraphRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)

	// Repo defaults: empty status → pending, zero graph_version → 1.
	stepA, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
		RunID: run.ID, StepID: "a", StepType: "llm", Config: json.RawMessage(`{"model":"mock"}`),
		UpdatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("create step a: %v", err)
	}
	if stepA.Status != store.StepStatusPending || stepA.GraphVersion != 1 {
		t.Fatalf("step defaults: %+v", stepA)
	}
	if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
		RunID: run.ID, StepID: "b", StepType: "tool", RemainingDeps: 2, FiredDeps: 0,
		UpdatedAt: testNow,
	}); err != nil {
		t.Fatalf("create step b: %v", err)
	}
	// Duplicate step id in the same run conflicts on the primary key.
	_, err = s.Steps().Create(ctx, gen.CreateRunStepParams{RunID: run.ID, StepID: "a", StepType: "llm", UpdatedAt: testNow})
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate step: got %v, want ConflictError", err)
	}

	got, err := s.Steps().Get(ctx, run.ID, "b")
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got.RemainingDeps != 2 || got.FiredDeps != 0 {
		t.Fatalf("dependency counters round-trip: %+v", got)
	}
	if _, err := s.Steps().Get(ctx, run.ID, "zzz"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get missing step: got %v, want ErrNotFound", err)
	}

	when := `output.score > 3`
	cond := `iterations < 5`
	maxIter := int32(5)
	// Insert out of ordinal order; the list must come back in it.
	if _, err := s.Steps().CreateEdge(ctx, gen.CreateRunEdgeParams{
		RunID: run.ID, Ordinal: 1, FromStep: "b", ToStep: "a",
		EdgeType: store.EdgeTypeLoop, Condition: &cond, MaxIterations: &maxIter,
	}); err != nil {
		t.Fatalf("create loop edge: %v", err)
	}
	edge0, err := s.Steps().CreateEdge(ctx, gen.CreateRunEdgeParams{
		RunID: run.ID, Ordinal: 0, FromStep: "a", ToStep: "b", WhenExpr: &when,
	})
	if err != nil {
		t.Fatalf("create normal edge: %v", err)
	}
	if edge0.EdgeType != store.EdgeTypeNormal || edge0.Resolution != store.EdgeResolutionUnresolved || edge0.GraphVersion != 1 {
		t.Fatalf("edge defaults: %+v", edge0)
	}

	edges, err := s.Steps().ListEdgesByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 2 || edges[0].Ordinal != 0 || edges[1].Ordinal != 1 {
		t.Fatalf("edges not in ordinal order: %+v", edges)
	}
	if edges[0].WhenExpr == nil || *edges[0].WhenExpr != when {
		t.Fatalf("when_expr round-trip: %+v", edges[0])
	}
	if edges[1].Condition == nil || *edges[1].Condition != cond ||
		edges[1].MaxIterations == nil || *edges[1].MaxIterations != maxIter {
		t.Fatalf("loop predicate round-trip: %+v", edges[1])
	}

	steps, err := s.Steps().ListByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("list steps: got %d, want 2", len(steps))
	}
}

func TestAttemptsRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)
	if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
		RunID: run.ID, StepID: "a", StepType: "llm", UpdatedAt: testNow,
	}); err != nil {
		t.Fatalf("create step: %v", err)
	}

	started := time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC)
	claim1, claim2 := uuid.New(), uuid.New()
	for i, claim := range []uuid.UUID{claim1, claim2} {
		if _, err := s.Attempts().Create(ctx, gen.CreateStepAttemptParams{
			RunID: run.ID, StepID: "a", AttemptNo: int32(i + 1), ClaimID: claim, StartedAt: &started,
		}); err != nil {
			t.Fatalf("create attempt %d: %v", i+1, err)
		}
	}
	// Attempts require their step row (composite FK).
	_, err := s.Attempts().Create(ctx, gen.CreateStepAttemptParams{
		RunID: run.ID, StepID: "ghost", AttemptNo: 1, ClaimID: uuid.New(),
	})
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("attempt without step: got %v, want ConflictError", err)
	}

	atts, err := s.Attempts().ListByStep(ctx, run.ID, "a")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(atts) != 2 || atts[0].AttemptNo != 1 || atts[1].AttemptNo != 2 {
		t.Fatalf("attempts not in attempt_no order: %+v", atts)
	}
	if atts[0].ClaimID != claim1 || atts[0].Outcome != nil {
		t.Fatalf("attempt round-trip: %+v", atts[0])
	}
}

func TestEventSeqAllocationAndBackfill(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)

	for i := 1; i <= 3; i++ {
		seq, err := s.Runs().AllocateEventSeq(ctx, run.ID)
		if err != nil {
			t.Fatalf("allocate seq: %v", err)
		}
		if seq != int64(i) {
			t.Fatalf("allocated seq %d, want %d", seq, i)
		}
		if _, err := s.Events().Append(ctx, gen.AppendEventParams{
			RunID: run.ID, Seq: seq, Type: "step_ready", Payload: json.RawMessage(`{"step":"a"}`),
		}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
	if _, err := s.Runs().AllocateEventSeq(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("allocate seq for missing run: got %v, want ErrNotFound", err)
	}

	// Duplicate (run_id, seq) conflicts — the appends-are-serialized invariant.
	_, err := s.Events().Append(ctx, gen.AppendEventParams{RunID: run.ID, Seq: 2, Type: "dup"})
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate seq: got %v, want ConflictError", err)
	}

	// Backfill from seq > 1 (the M16 shape).
	evs, err := s.Events().List(ctx, run.ID, 1, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) != 2 || evs[0].Seq != 2 || evs[1].Seq != 3 {
		t.Fatalf("backfill from seq>1: %+v", evs)
	}
	if !jsonEqual(t, evs[0].Payload, json.RawMessage(`{"step":"a"}`)) {
		t.Fatalf("payload round-trip: %s", evs[0].Payload)
	}
}

func TestOutboxRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := mustCreateRun(t, s, nil)

	var ids []int64
	for _, stepID := range []string{"a", "b", "c"} {
		task, err := s.Outbox().Create(ctx, run.ID, stepID, store.OutboxReasonStepReady)
		if err != nil {
			t.Fatalf("create outbox task %s: %v", stepID, err)
		}
		ids = append(ids, task.ID)
	}

	tasks, err := s.Outbox().List(ctx, 10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("list outbox: got %d tasks, want 3", len(tasks))
	}
	for i := 1; i < len(tasks); i++ {
		if tasks[i].ID <= tasks[i-1].ID {
			t.Fatal("outbox list not in id (drain) order")
		}
	}

	// Drain = delete: row exists ⇔ dispatch pending.
	deleted, err := s.Outbox().Delete(ctx, ids[:2])
	if err != nil {
		t.Fatalf("delete outbox tasks: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d tasks, want 2", deleted)
	}
	remaining, err := s.Outbox().List(ctx, 10)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != ids[2] {
		t.Fatalf("remaining outbox: %+v", remaining)
	}
}
