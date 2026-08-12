package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
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
	// ListLatest returns the newest version of each name, in name order,
	// keyset-paginated: names strictly after cursorName (nil = from the
	// start), up to limit rows (the definitions list API, ticket 6.5).
	ListLatest(ctx context.Context, cursorName *string, limit int32) ([]gen.WorkflowDefinition, error)
	// NextVersion returns the version a new row of this name should take:
	// 1 for an unseen name. Callers serialize via LockName first —
	// Store.CreateDefinitionVersion is the composed op.
	NextVersion(ctx context.Context, name string) (int32, error)
	// LockName takes the name's transaction-scoped advisory lock,
	// serializing version allocation. Must run inside WithTx (the lock is
	// released at commit/rollback); any other Querier fails with ErrNoTx.
	LockName(ctx context.Context, name string) error
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

func (r definitionRepo) ListLatest(ctx context.Context, cursorName *string, limit int32) ([]gen.WorkflowDefinition, error) {
	defs, err := r.q.ListDefinitionsLatest(ctx, gen.ListDefinitionsLatestParams{CursorName: cursorName, RowLimit: limit})
	return defs, wrapErr("list latest definitions", err)
}

func (r definitionRepo) NextVersion(ctx context.Context, name string) (int32, error) {
	v, err := r.q.NextDefinitionVersion(ctx, name)
	return v, wrapErr("next definition version", err)
}

func (r definitionRepo) LockName(ctx context.Context, name string) error {
	if ctx.Value(txMarker{}) == nil {
		return fmt.Errorf("store: lock definition name: %w", ErrNoTx)
	}
	return wrapErr("lock definition name", r.q.AcquireDefinitionNameLock(ctx, definitionNameLockKey(name)))
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
	// GetStatus is the lightweight unlocked status read behind the
	// engine's in-flight cancellation poller (ticket 5.6): a hint only —
	// authoritative checks run under the run lock inside transactions.
	GetStatus(ctx context.Context, id uuid.UUID) (string, error)
	GetByIdempotencyToken(ctx context.Context, token string) (gen.Run, error)
	// List returns runs newest-first.
	List(ctx context.Context, limit int32) ([]gen.Run, error)
	ListByStatus(ctx context.Context, status string, limit int32) ([]gen.Run, error)
	// ListPage is the run-list API's keyset page read (ticket 6.5): order
	// (created_at DESC, id DESC), optional status/definition/time-range
	// filters, cursor = the previous page's last row. Nil filter/cursor
	// fields are disabled.
	ListPage(ctx context.Context, arg gen.ListRunsPageParams) ([]gen.Run, error)
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
	if arg.OnFailure == "" {
		// Absent on_failure means fail_fast (ADR-006); run instantiation
		// always materializes explicitly, like retry_policy.
		arg.OnFailure = string(dag.FailFast)
	}
	run, err := r.q.CreateRun(ctx, arg)
	return run, wrapErr("create run", err)
}

func (r runRepo) Get(ctx context.Context, id uuid.UUID) (gen.Run, error) {
	run, err := r.q.GetRun(ctx, id)
	return run, wrapErr("get run", err)
}

func (r runRepo) GetStatus(ctx context.Context, id uuid.UUID) (string, error) {
	status, err := r.q.GetRunStatus(ctx, id)
	return status, wrapErr("get run status", err)
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

func (r runRepo) ListPage(ctx context.Context, arg gen.ListRunsPageParams) ([]gen.Run, error) {
	runs, err := r.q.ListRunsPage(ctx, arg)
	return runs, wrapErr("list runs page", err)
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
	// zero arg.GraphVersion becomes 1; a nil arg.RetryPolicy becomes the
	// serialized engine-default policy (ticket 5.2 — semantically what an
	// absent authored policy means; run instantiation always materializes
	// explicitly). arg.UpdatedAt is required (injected clock — the
	// reconciler's staleness scan reads it): zero is an error.
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
	// ListEdgesFromStep returns one step's outgoing edges in ordinal
	// (declaration) order — the completion transaction's fan-out read
	// (M4.3).
	ListEdgesFromStep(ctx context.Context, runID uuid.UUID, fromStep string) ([]gen.RunEdge, error)
	// ListReadyWithoutOutbox returns the run's ready steps with no pending
	// task_outbox row, in step_id order — the requeue op's re-dispatch set
	// (ticket 5.4): the requeued step itself plus fail_fast siblings whose
	// deliveries were consumed while the run was failed.
	ListReadyWithoutOutbox(ctx context.Context, runID uuid.UUID) ([]string, error)
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
	if arg.RetryPolicy == nil {
		arg.RetryPolicy = defaultRetryPolicyJSON()
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
		if args[i].RetryPolicy == nil {
			args[i].RetryPolicy = defaultRetryPolicyJSON()
		}
	}
	rows, err := r.q.CreateRunSteps(ctx, args)
	return rows, wrapErr("create run steps", err)
}

// defaultRetryPolicyJSON serializes the all-defaults effective policy —
// what a nil RetryPolicy on a directly-created step row means. The merge
// is dag's; marshaling a plain struct cannot fail.
func defaultRetryPolicyJSON() json.RawMessage {
	b, _ := json.Marshal(dag.ResolveRetryPolicy(nil)) //nolint:errcheck // plain struct
	return b
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

func (r stepRepo) ListEdgesFromStep(ctx context.Context, runID uuid.UUID, fromStep string) ([]gen.RunEdge, error) {
	edges, err := r.q.ListRunEdgesFromStep(ctx, gen.ListRunEdgesFromStepParams{RunID: runID, FromStep: fromStep})
	return edges, wrapErr("list run edges from step", err)
}

func (r stepRepo) ListReadyWithoutOutbox(ctx context.Context, runID uuid.UUID) ([]string, error) {
	ids, err := r.q.ListReadyStepsWithoutOutbox(ctx, runID)
	return ids, wrapErr("list ready steps without outbox", err)
}

// AttemptRepo stores step_attempts rows. Outcome/error/finished_at are
// written by the completion transitions (2.6).
type AttemptRepo interface {
	Create(ctx context.Context, arg gen.CreateStepAttemptParams) (gen.StepAttempt, error)
	// ListByStep returns a step's attempts in attempt_no order.
	ListByStep(ctx context.Context, runID uuid.UUID, stepID string) ([]gen.StepAttempt, error)
	// ListByRun returns every attempt of the run in (step_id, attempt_no)
	// order — one query for the run-detail API instead of one per step.
	ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.StepAttempt, error)
	// CountCountedFailures returns how many of the step's attempts closed
	// with a counted outcome — transient or timeout — the durable retry
	// budget (ticket 5.2, ADR-006: `lost` closures are excluded).
	CountCountedFailures(ctx context.Context, runID uuid.UUID, stepID string) (int64, error)
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

func (r attemptRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.StepAttempt, error) {
	atts, err := r.q.ListRunStepAttempts(ctx, runID)
	return atts, wrapErr("list run step attempts", err)
}

func (r attemptRepo) CountCountedFailures(ctx context.Context, runID uuid.UUID, stepID string) (int64, error) {
	n, err := r.q.CountCountedFailures(ctx, gen.CountCountedFailuresParams{RunID: runID, StepID: stepID})
	return n, wrapErr("count counted failures", err)
}

// DeadLetterRepo reads dead_letters rows (ticket 5.4). Read-only by
// design: rows are written exclusively inside the dead-lettering
// transitions (transitions.go), in the same transaction as the step's
// terminal CAS, and are never updated or deleted — like attempt history,
// the death record is immutable audit state (requeue counts from its
// baseline instead of erasing it).
type DeadLetterRepo interface {
	// ListByStep returns a step's dead-letter records in seq (death)
	// order.
	ListByStep(ctx context.Context, runID uuid.UUID, stepID string) ([]gen.DeadLetter, error)
	// ListByRun returns every dead-letter record of the run in
	// (step_id, seq) order.
	ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.DeadLetter, error)
}

type deadLetterRepo struct{ q *gen.Queries }

func (r deadLetterRepo) ListByStep(ctx context.Context, runID uuid.UUID, stepID string) ([]gen.DeadLetter, error) {
	rows, err := r.q.ListDeadLettersByStep(ctx, gen.ListDeadLettersByStepParams{RunID: runID, StepID: stepID})
	return rows, wrapErr("list dead letters by step", err)
}

func (r deadLetterRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]gen.DeadLetter, error) {
	rows, err := r.q.ListDeadLettersByRun(ctx, runID)
	return rows, wrapErr("list dead letters by run", err)
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
// dispatch pending: drained rows are deleted.
type OutboxRepo interface {
	Create(ctx context.Context, runID uuid.UUID, stepID, reason string) (gen.TaskOutbox, error)
	// List returns up to limit pending tasks in id (drain) order.
	List(ctx context.Context, limit int32) ([]gen.TaskOutbox, error)
	// ListForDrain is List with FOR UPDATE SKIP LOCKED — the dispatcher's
	// batch read (ticket 4.4). Concurrent drainers lock disjoint sets, so
	// no row is dispatched twice unless a drain transaction rolls back
	// after its XADD (ADR-005 P1 — the claim CAS absorbs the duplicate).
	// Must run inside WithTx: row locks outside a transaction are
	// meaningless, so any other Querier fails with ErrNoTx.
	ListForDrain(ctx context.Context, limit int32) ([]gen.TaskOutbox, error)
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

func (r outboxRepo) ListForDrain(ctx context.Context, limit int32) ([]gen.TaskOutbox, error) {
	if ctx.Value(txMarker{}) == nil {
		return nil, fmt.Errorf("store: list outbox tasks for drain: %w", ErrNoTx)
	}
	tasks, err := r.q.ListOutboxTasksForDrain(ctx, limit)
	return tasks, wrapErr("list outbox tasks for drain", err)
}

func (r outboxRepo) Delete(ctx context.Context, ids []int64) (int64, error) {
	rows, err := r.q.DeleteOutboxTasks(ctx, ids)
	return rows, wrapErr("delete outbox tasks", err)
}

// APIKeyRepo stores api_keys rows (ticket 6.1, ADR-007). Plain CRUD by
// design — keys are not run state machines. The plaintext credential never
// reaches this layer: callers pass the precomputed hash and lookup prefix.
type APIKeyRepo interface {
	// Create inserts the key row. A zero arg.ID gets a random UUID. A
	// prefix or hash collision surfaces as a *ConflictError (the create
	// endpoint regenerates and retries on prefix collisions).
	Create(ctx context.Context, arg gen.CreateAPIKeyParams) (gen.ApiKey, error)
	Get(ctx context.Context, id uuid.UUID) (gen.ApiKey, error)
	// GetByPrefix is the verification path's single indexed read.
	GetByPrefix(ctx context.Context, prefix string) (gen.ApiKey, error)
	// List returns all keys, newest first, revoked and expired included.
	List(ctx context.Context) ([]gen.ApiKey, error)
	// Revoke soft-revokes the key at the given instant. First-wins: an
	// already-revoked key keeps its original revoked_at and Revoke
	// returns (false, nil); an unknown id returns ErrNotFound.
	Revoke(ctx context.Context, id uuid.UUID, at time.Time) (revoked bool, err error)
}

type apiKeyRepo struct{ q *gen.Queries }

func (r apiKeyRepo) Create(ctx context.Context, arg gen.CreateAPIKeyParams) (gen.ApiKey, error) {
	if arg.ID == uuid.Nil {
		arg.ID = uuid.New()
	}
	key, err := r.q.CreateAPIKey(ctx, arg)
	return key, wrapErr("create api key", err)
}

func (r apiKeyRepo) Get(ctx context.Context, id uuid.UUID) (gen.ApiKey, error) {
	key, err := r.q.GetAPIKey(ctx, id)
	return key, wrapErr("get api key", err)
}

func (r apiKeyRepo) GetByPrefix(ctx context.Context, prefix string) (gen.ApiKey, error) {
	key, err := r.q.GetAPIKeyByPrefix(ctx, prefix)
	return key, wrapErr("get api key by prefix", err)
}

func (r apiKeyRepo) List(ctx context.Context) ([]gen.ApiKey, error) {
	keys, err := r.q.ListAPIKeys(ctx)
	return keys, wrapErr("list api keys", err)
}

func (r apiKeyRepo) Revoke(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	rows, err := r.q.RevokeAPIKey(ctx, gen.RevokeAPIKeyParams{ID: id, RevokedAt: &at})
	if err != nil {
		return false, wrapErr("revoke api key", err)
	}
	if rows > 0 {
		return true, nil
	}
	// Zero rows is either "already revoked" (fine, idempotent) or "no such
	// key" (the caller's error) — one more read tells them apart.
	if _, err := r.q.GetAPIKey(ctx, id); err != nil {
		return false, wrapErr("revoke api key", err)
	}
	return false, nil
}
