package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// emptyJSON is the value repositories substitute for nil on NOT NULL JSONB
// columns whose schema default the explicit INSERT would otherwise bypass.
var emptyJSON = json.RawMessage(`{}`)

// DefinitionRepo stores workflow_definitions rows. Rows are immutable —
// there is no update method by design; a new version is a new row.
type DefinitionRepo interface {
	Create(ctx context.Context, arg gen.CreateDefinitionParams) (gen.WorkflowDefinition, error)
	Get(ctx context.Context, id uuid.UUID) (gen.WorkflowDefinition, error)
	GetByNameVersion(ctx context.Context, name string, version int32) (gen.WorkflowDefinition, error)
	// ListVersions returns all versions of one definition, oldest first.
	ListVersions(ctx context.Context, name string) ([]gen.WorkflowDefinition, error)
	List(ctx context.Context) ([]gen.WorkflowDefinition, error)
	// Delete fails with a ConflictError while runs reference the
	// definition (ON DELETE RESTRICT).
	Delete(ctx context.Context, id uuid.UUID) error
}

type definitionRepo struct{ q *gen.Queries }

func (r definitionRepo) Create(ctx context.Context, arg gen.CreateDefinitionParams) (gen.WorkflowDefinition, error) {
	def, err := r.q.CreateDefinition(ctx, arg)
	return def, wrapErr("create definition", err)
}

func (r definitionRepo) Get(ctx context.Context, id uuid.UUID) (gen.WorkflowDefinition, error) {
	def, err := r.q.GetDefinition(ctx, id)
	return def, wrapErr("get definition", err)
}

func (r definitionRepo) GetByNameVersion(ctx context.Context, name string, version int32) (gen.WorkflowDefinition, error) {
	def, err := r.q.GetDefinitionByNameVersion(ctx, gen.GetDefinitionByNameVersionParams{Name: name, Version: version})
	return def, wrapErr("get definition by name+version", err)
}

func (r definitionRepo) ListVersions(ctx context.Context, name string) ([]gen.WorkflowDefinition, error) {
	defs, err := r.q.ListDefinitionVersions(ctx, name)
	return defs, wrapErr("list definition versions", err)
}

func (r definitionRepo) List(ctx context.Context) ([]gen.WorkflowDefinition, error) {
	defs, err := r.q.ListDefinitions(ctx)
	return defs, wrapErr("list definitions", err)
}

func (r definitionRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rows, err := r.q.DeleteDefinition(ctx, id)
	if err == nil && rows == 0 {
		return wrapErr("delete definition", errNoRowsDeleted)
	}
	return wrapErr("delete definition", err)
}

// RunRepo stores runs rows. Create/read/delete only: status and aggregate
// mutations are guarded CAS transitions (ticket 2.6), never plain updates.
type RunRepo interface {
	// Create inserts the run row. A zero arg.ID gets a random UUID; a nil
	// arg.Params becomes the empty JSON object.
	Create(ctx context.Context, arg gen.CreateRunParams) (gen.Run, error)
	Get(ctx context.Context, id uuid.UUID) (gen.Run, error)
	GetByIdempotencyToken(ctx context.Context, token string) (gen.Run, error)
	// List returns runs newest-first.
	List(ctx context.Context, limit int32) ([]gen.Run, error)
	ListByStatus(ctx context.Context, status string, limit int32) ([]gen.Run, error)
	// Delete cascades to the run's steps, edges, attempts, and events.
	Delete(ctx context.Context, id uuid.UUID) error
	// AllocateEventSeq returns the next per-run event sequence number
	// (ADR-004): the runs-row lock it takes serializes appends per run,
	// so callers allocate and append in the same transaction.
	AllocateEventSeq(ctx context.Context, runID uuid.UUID) (int64, error)
}

type runRepo struct{ q *gen.Queries }

func (r runRepo) Create(ctx context.Context, arg gen.CreateRunParams) (gen.Run, error) {
	if arg.ID == uuid.Nil {
		arg.ID = uuid.New()
	}
	if arg.Params == nil {
		arg.Params = emptyJSON
	}
	run, err := r.q.CreateRun(ctx, arg)
	return run, wrapErr("create run", err)
}

func (r runRepo) Get(ctx context.Context, id uuid.UUID) (gen.Run, error) {
	run, err := r.q.GetRun(ctx, id)
	return run, wrapErr("get run", err)
}

func (r runRepo) GetByIdempotencyToken(ctx context.Context, token string) (gen.Run, error) {
	run, err := r.q.GetRunByIdempotencyToken(ctx, token)
	return run, wrapErr("get run by idempotency token", err)
}

func (r runRepo) List(ctx context.Context, limit int32) ([]gen.Run, error) {
	runs, err := r.q.ListRuns(ctx, limit)
	return runs, wrapErr("list runs", err)
}

func (r runRepo) ListByStatus(ctx context.Context, status string, limit int32) ([]gen.Run, error) {
	runs, err := r.q.ListRunsByStatus(ctx, gen.ListRunsByStatusParams{Status: status, Limit: limit})
	return runs, wrapErr("list runs by status", err)
}

func (r runRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rows, err := r.q.DeleteRun(ctx, id)
	if err == nil && rows == 0 {
		return wrapErr("delete run", errNoRowsDeleted)
	}
	return wrapErr("delete run", err)
}

func (r runRepo) AllocateEventSeq(ctx context.Context, runID uuid.UUID) (int64, error) {
	seq, err := r.q.AllocateEventSeq(ctx, runID)
	return seq, wrapErr("allocate event seq", err)
}

// StepRepo stores the per-run graph copy: run_steps and run_edges together,
// because instantiation (2.5) and expansion (M13) always write them as one
// graph.
type StepRepo interface {
	// Create inserts a step row. An empty arg.Status becomes pending; a
	// zero arg.GraphVersion becomes 1. arg.UpdatedAt is required (injected
	// clock — the reconciler's staleness scan reads it): zero is an error.
	Create(ctx context.Context, arg gen.CreateRunStepParams) (gen.RunStep, error)
	// CreateBatch inserts step rows in one COPY, with the same per-row
	// defaulting and UpdatedAt requirement as Create.
	CreateBatch(ctx context.Context, args []gen.CreateRunStepsParams) (int64, error)
	Get(ctx context.Context, runID uuid.UUID, stepID string) (gen.RunStep, error)
	ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.RunStep, error)
	// CreateEdge inserts an edge row. An empty arg.EdgeType becomes
	// normal; a zero arg.GraphVersion becomes 1.
	CreateEdge(ctx context.Context, arg gen.CreateRunEdgeParams) (gen.RunEdge, error)
	// CreateEdgeBatch inserts edge rows in one COPY, with the same per-row
	// defaulting as CreateEdge.
	CreateEdgeBatch(ctx context.Context, args []gen.CreateRunEdgesParams) (int64, error)
	// ListEdgesByRun returns edges in ordinal (declaration) order — the
	// order the branch first-match rule evaluates them in.
	ListEdgesByRun(ctx context.Context, runID uuid.UUID) ([]gen.RunEdge, error)
}

type stepRepo struct{ q *gen.Queries }

func (r stepRepo) Create(ctx context.Context, arg gen.CreateRunStepParams) (gen.RunStep, error) {
	if arg.UpdatedAt.IsZero() {
		return gen.RunStep{}, errors.New("store: create run step: zero UpdatedAt — pass the injected current time")
	}
	if arg.Status == "" {
		arg.Status = StepStatusPending
	}
	if arg.GraphVersion == 0 {
		arg.GraphVersion = 1
	}
	step, err := r.q.CreateRunStep(ctx, arg)
	return step, wrapErr("create run step", err)
}

func (r stepRepo) CreateBatch(ctx context.Context, args []gen.CreateRunStepsParams) (int64, error) {
	for i := range args {
		if args[i].UpdatedAt.IsZero() {
			return 0, fmt.Errorf("store: create run steps: step %q: zero UpdatedAt — pass the injected current time", args[i].StepID)
		}
		if args[i].Status == "" {
			args[i].Status = StepStatusPending
		}
		if args[i].GraphVersion == 0 {
			args[i].GraphVersion = 1
		}
	}
	rows, err := r.q.CreateRunSteps(ctx, args)
	return rows, wrapErr("create run steps", err)
}

func (r stepRepo) Get(ctx context.Context, runID uuid.UUID, stepID string) (gen.RunStep, error) {
	step, err := r.q.GetRunStep(ctx, gen.GetRunStepParams{RunID: runID, StepID: stepID})
	return step, wrapErr("get run step", err)
}

func (r stepRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.RunStep, error) {
	steps, err := r.q.ListRunSteps(ctx, runID)
	return steps, wrapErr("list run steps", err)
}

func (r stepRepo) CreateEdge(ctx context.Context, arg gen.CreateRunEdgeParams) (gen.RunEdge, error) {
	if arg.EdgeType == "" {
		arg.EdgeType = EdgeTypeNormal
	}
	if arg.GraphVersion == 0 {
		arg.GraphVersion = 1
	}
	edge, err := r.q.CreateRunEdge(ctx, arg)
	return edge, wrapErr("create run edge", err)
}

func (r stepRepo) CreateEdgeBatch(ctx context.Context, args []gen.CreateRunEdgesParams) (int64, error) {
	for i := range args {
		if args[i].EdgeType == "" {
			args[i].EdgeType = EdgeTypeNormal
		}
		if args[i].GraphVersion == 0 {
			args[i].GraphVersion = 1
		}
	}
	rows, err := r.q.CreateRunEdges(ctx, args)
	return rows, wrapErr("create run edges", err)
}

func (r stepRepo) ListEdgesByRun(ctx context.Context, runID uuid.UUID) ([]gen.RunEdge, error) {
	edges, err := r.q.ListRunEdges(ctx, runID)
	return edges, wrapErr("list run edges", err)
}

// AttemptRepo stores step_attempts rows. Outcome/error/finished_at are
// written by the completion transitions (2.6).
type AttemptRepo interface {
	Create(ctx context.Context, arg gen.CreateStepAttemptParams) (gen.StepAttempt, error)
	// ListByStep returns a step's attempts in attempt_no order.
	ListByStep(ctx context.Context, runID uuid.UUID, stepID string) ([]gen.StepAttempt, error)
}

type attemptRepo struct{ q *gen.Queries }

func (r attemptRepo) Create(ctx context.Context, arg gen.CreateStepAttemptParams) (gen.StepAttempt, error) {
	att, err := r.q.CreateStepAttempt(ctx, arg)
	return att, wrapErr("create step attempt", err)
}

func (r attemptRepo) ListByStep(ctx context.Context, runID uuid.UUID, stepID string) ([]gen.StepAttempt, error) {
	atts, err := r.q.ListStepAttempts(ctx, gen.ListStepAttemptsParams{RunID: runID, StepID: stepID})
	return atts, wrapErr("list step attempts", err)
}

// EventRepo stores the append-only per-run event log. Append-only is
// enforced by this surface: no update or delete methods exist, ever
// (ADR-004). Sequence numbers come from RunRepo.AllocateEventSeq in the
// same transaction as the append.
type EventRepo interface {
	// Append inserts an event. A nil arg.Payload becomes the empty JSON
	// object.
	Append(ctx context.Context, arg gen.AppendEventParams) (gen.Event, error)
	// List returns up to limit events with seq > afterSeq, in seq order —
	// the M16 backfill shape.
	List(ctx context.Context, runID uuid.UUID, afterSeq int64, limit int32) ([]gen.Event, error)
}

type eventRepo struct{ q *gen.Queries }

func (r eventRepo) Append(ctx context.Context, arg gen.AppendEventParams) (gen.Event, error) {
	if arg.Payload == nil {
		arg.Payload = emptyJSON
	}
	ev, err := r.q.AppendEvent(ctx, arg)
	return ev, wrapErr("append event", err)
}

func (r eventRepo) List(ctx context.Context, runID uuid.UUID, afterSeq int64, limit int32) ([]gen.Event, error) {
	evs, err := r.q.ListEvents(ctx, gen.ListEventsParams{RunID: runID, Seq: afterSeq, Limit: limit})
	return evs, wrapErr("list events", err)
}

// OutboxRepo stores the transactional dispatch buffer. Row exists ⇔
// dispatch pending: drained rows are deleted, and the FOR UPDATE SKIP
// LOCKED drain query arrives with the queue layer (M4).
type OutboxRepo interface {
	Create(ctx context.Context, runID uuid.UUID, stepID, reason string) (gen.TaskOutbox, error)
	// List returns up to limit pending tasks in id (drain) order.
	List(ctx context.Context, limit int32) ([]gen.TaskOutbox, error)
	// Delete removes the given tasks, returning how many existed.
	Delete(ctx context.Context, ids []int64) (int64, error)
}

type outboxRepo struct{ q *gen.Queries }

func (r outboxRepo) Create(ctx context.Context, runID uuid.UUID, stepID, reason string) (gen.TaskOutbox, error) {
	task, err := r.q.CreateOutboxTask(ctx, gen.CreateOutboxTaskParams{RunID: runID, StepID: stepID, Reason: reason})
	return task, wrapErr("create outbox task", err)
}

func (r outboxRepo) List(ctx context.Context, limit int32) ([]gen.TaskOutbox, error) {
	tasks, err := r.q.ListOutboxTasks(ctx, limit)
	return tasks, wrapErr("list outbox tasks", err)
}

func (r outboxRepo) Delete(ctx context.Context, ids []int64) (int64, error) {
	rows, err := r.q.DeleteOutboxTasks(ctx, ids)
	return rows, wrapErr("delete outbox tasks", err)
}
