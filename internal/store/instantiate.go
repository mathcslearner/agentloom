package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// constraintIdempotencyToken is the unique partial index behind idempotent
// submission (schema v1). A unique violation naming it means another
// submission with the same token already created the run.
const constraintIdempotencyToken = "runs_idempotency_token_key"

// Failpoint stages, in transaction order. Test-only: the instantiation
// transaction consults instantiateFailpoint after each phase so the
// all-or-nothing test can abort it anywhere.
const (
	stageAfterRunInsert = "after_run_insert"
	stageAfterSteps     = "after_steps"
	stageAfterEdges     = "after_edges"
	stageAfterEvents    = "after_events"
)

// instantiateFailpoint, when non-nil, is called with each stage name inside
// the instantiation transaction; a non-nil return aborts it. Installed only
// by tests (export_test.go), nil in production.
var instantiateFailpoint func(stage string) error

func failpoint(stage string) error {
	if instantiateFailpoint != nil {
		return instantiateFailpoint(stage)
	}
	return nil
}

// CreateRunArgs are the inputs to CreateRun.
type CreateRunArgs struct {
	// RunID optionally fixes the new run's id; zero gets a random UUID.
	RunID uuid.UUID
	// Definition is the decoded workflow definition to instantiate.
	// CreateRun validates it and rejects definitions with error-severity
	// issues. Required.
	Definition *dag.Definition
	// DefinitionID is the registry row the definition came from; nil for
	// inline (submit-by-value) submissions.
	DefinitionID *uuid.UUID
	// Params are the submitted run parameters; nil becomes the empty JSON
	// object. Values are stored opaquely — validation against the
	// definition's ParamSpecs is the submission API's (M6).
	Params json.RawMessage
	// IdempotencyToken makes submission idempotent: a second CreateRun with
	// the same token returns the original run instead of creating another.
	// Empty means not idempotent.
	IdempotencyToken string
	// Now is the injected current time (project invariant: no bare
	// time.Now in logic under test); it becomes the run's started_at.
	// Required.
	Now time.Time
}

// CreateRunResult is what CreateRun returns.
type CreateRunResult struct {
	Run gen.Run
	// EntrySteps are the step ids instantiated ready (no incoming normal
	// edges), in declaration order — derived from the run's definition
	// snapshot, so it is the same on the created and reused paths.
	EntrySteps []string
	// Reused is true when an existing run was returned for
	// IdempotencyToken and nothing was written.
	Reused bool
}

// Event payloads (v1 minimal shapes; ADR-018 owns the formal envelope).
type runCreatedPayload struct {
	Name       string `json:"name"`
	StepsTotal int    `json:"steps_total"`
}

type stepReadyPayload struct {
	StepID string `json:"step_id"`
}

// CreateRun instantiates one run of def atomically (ticket 2.5): in a
// single transaction it writes the run row (status running, definition
// snapshot, params), the per-run graph copy with ADR-004's dependency
// bookkeeping precomputed (remaining_deps = incoming normal edges; loop
// edges excluded), entry steps as ready, a task_outbox row per entry step,
// and the run_created / step_ready events. Any failure leaves zero rows.
//
// With an IdempotencyToken, a duplicate submission — sequential or racing —
// returns the original run with Reused set; the unique partial index on
// runs.idempotency_token is the authority, the pre-check merely avoids the
// round trip.
func (s *Store) CreateRun(ctx context.Context, args CreateRunArgs) (CreateRunResult, error) {
	if args.Definition == nil {
		return CreateRunResult{}, errors.New("store: CreateRun: nil definition")
	}
	if args.Now.IsZero() {
		return CreateRunResult{}, errors.New("store: CreateRun: zero Now — pass the injected current time")
	}
	if _, err := dag.Validate(args.Definition); err != nil {
		return CreateRunResult{}, fmt.Errorf("store: CreateRun: %w", err)
	}
	plan, err := planInstantiation(args.Definition)
	if err != nil {
		return CreateRunResult{}, fmt.Errorf("store: CreateRun: %w", err)
	}

	if args.IdempotencyToken != "" {
		run, err := s.Runs().GetByIdempotencyToken(ctx, args.IdempotencyToken)
		switch {
		case err == nil:
			return reusedResult(run)
		case !errors.Is(err, ErrNotFound):
			return CreateRunResult{}, err
		}
	}

	var run gen.Run
	txErr := s.WithTx(ctx, func(ctx context.Context, q Querier) error {
		var err error
		run, err = plan.insert(ctx, q, args)
		return err
	})
	if txErr != nil {
		// Lost a same-token race: the index rejected our insert, so the
		// winner's run exists — return it, as the sequential path would.
		var conflict *ConflictError
		if args.IdempotencyToken != "" && errors.As(txErr, &conflict) &&
			conflict.Constraint == constraintIdempotencyToken {
			if run, err := s.Runs().GetByIdempotencyToken(ctx, args.IdempotencyToken); err == nil {
				return reusedResult(run)
			}
		}
		return CreateRunResult{}, txErr
	}

	log.From(ctx).InfoContext(ctx, "run instantiated",
		log.RunID(run.ID.String()),
		slog.Int("steps_total", len(plan.def.Steps)),
		slog.Int("entry_steps", len(plan.entry)))
	return CreateRunResult{Run: run, EntrySteps: plan.entry}, nil
}

// instantiationPlan is everything CreateRun computes about a definition
// before opening the transaction: the canonical snapshot, the entry steps,
// and the per-step dependency counters.
type instantiationPlan struct {
	def       *dag.Definition
	snapshot  json.RawMessage
	entry     []string // declaration order
	entrySet  map[string]bool
	remaining map[string]int32 // incoming normal edges per step id
}

// planInstantiation derives the plan from a validated definition. Entry
// steps come from dag.ReadySteps with empty inputs — the reference
// implementation ADR-004's counters must mirror — not a re-derivation.
func planInstantiation(def *dag.Definition) (*instantiationPlan, error) {
	snapshot, err := dag.Encode(def)
	if err != nil {
		return nil, fmt.Errorf("encoding definition snapshot: %w", err)
	}
	g, err := dag.NewGraph(def)
	if err != nil {
		return nil, fmt.Errorf("building graph: %w", err)
	}
	entry, newlySkipped, err := g.ReadySteps(nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("computing entry steps: %w", err)
	}
	if len(newlySkipped) > 0 {
		// Impossible with empty inputs; a non-empty result means ReadySteps
		// and this plan disagree about instantiation-time semantics.
		return nil, fmt.Errorf("instantiation skipped steps %q", newlySkipped)
	}
	plan := &instantiationPlan{
		def:       def,
		snapshot:  snapshot,
		entry:     entry,
		entrySet:  make(map[string]bool, len(entry)),
		remaining: make(map[string]int32, len(def.Steps)),
	}
	for _, id := range entry {
		plan.entrySet[id] = true
	}
	for _, e := range def.Edges {
		if !e.IsLoop() {
			plan.remaining[e.To]++
		}
	}
	return plan, nil
}

// insert writes the whole run inside the caller's transaction.
func (p *instantiationPlan) insert(ctx context.Context, q Querier, args CreateRunArgs) (gen.Run, error) {
	var token *string
	if args.IdempotencyToken != "" {
		token = &args.IdempotencyToken
	}
	startedAt := args.Now
	run, err := q.Runs().Create(ctx, gen.CreateRunParams{
		ID:               args.RunID,
		DefinitionID:     args.DefinitionID,
		Definition:       p.snapshot,
		Status:           RunStatusRunning,
		Params:           args.Params,
		IdempotencyToken: token,
		StepsTotal:       int32(len(p.def.Steps)), //nolint:gosec // step count is validation-bounded
		StartedAt:        &startedAt,
	})
	if err != nil {
		return gen.Run{}, err
	}
	if err := failpoint(stageAfterRunInsert); err != nil {
		return gen.Run{}, err
	}

	steps := make([]gen.CreateRunStepsParams, len(p.def.Steps))
	for i, step := range p.def.Steps {
		status := StepStatusPending
		if p.entrySet[step.ID] {
			status = StepStatusReady
		}
		var config json.RawMessage
		if step.Config != nil {
			config, err = json.Marshal(step.Config)
			if err != nil {
				return gen.Run{}, fmt.Errorf("marshaling config of step %q: %w", step.ID, err)
			}
		}
		steps[i] = gen.CreateRunStepsParams{
			RunID:         run.ID,
			StepID:        step.ID,
			StepType:      string(step.Type),
			Config:        config,
			Status:        status,
			RemainingDeps: p.remaining[step.ID],
			GraphVersion:  1,
		}
	}
	if _, err := q.Steps().CreateBatch(ctx, steps); err != nil {
		return gen.Run{}, err
	}
	if err := failpoint(stageAfterSteps); err != nil {
		return gen.Run{}, err
	}

	edges := make([]gen.CreateRunEdgesParams, len(p.def.Edges))
	for i, e := range p.def.Edges {
		edge := gen.CreateRunEdgesParams{
			RunID:        run.ID,
			Ordinal:      int32(i), //nolint:gosec // edge count is validation-bounded
			FromStep:     e.From,
			ToStep:       e.To,
			EdgeType:     string(e.Type),
			GraphVersion: 1,
		}
		if e.When != "" {
			edge.WhenExpr = &e.When
		}
		if e.Condition != "" {
			edge.Condition = &e.Condition
		}
		if e.MaxIterations != 0 {
			maxIter := int32(e.MaxIterations) //nolint:gosec // validation-bounded
			edge.MaxIterations = &maxIter
		}
		edges[i] = edge
	}
	if _, err := q.Steps().CreateEdgeBatch(ctx, edges); err != nil {
		return gen.Run{}, err
	}
	if err := failpoint(stageAfterEdges); err != nil {
		return gen.Run{}, err
	}

	if err := p.appendEvent(ctx, q, run.ID, EventRunCreated,
		runCreatedPayload{Name: p.def.Name, StepsTotal: len(p.def.Steps)}); err != nil {
		return gen.Run{}, err
	}
	for _, id := range p.entry {
		if err := p.appendEvent(ctx, q, run.ID, EventStepReady, stepReadyPayload{StepID: id}); err != nil {
			return gen.Run{}, err
		}
	}
	if err := failpoint(stageAfterEvents); err != nil {
		return gen.Run{}, err
	}

	for _, id := range p.entry {
		if _, err := q.Outbox().Create(ctx, run.ID, id, OutboxReasonStepReady); err != nil {
			return gen.Run{}, err
		}
	}
	return run, nil
}

// appendEvent allocates the next per-run seq and appends one event, both on
// the transaction q runs on (ADR-004 event sequencing).
func (p *instantiationPlan) appendEvent(ctx context.Context, q Querier, runID uuid.UUID, typ string, payload any) error {
	seq, err := q.Runs().AllocateEventSeq(ctx, runID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling %s payload: %w", typ, err)
	}
	_, err = q.Events().Append(ctx, gen.AppendEventParams{RunID: runID, Seq: seq, Type: typ, Payload: body})
	return err
}

// reusedResult builds the duplicate-submission result from the original
// run, re-deriving EntrySteps from its stored snapshot so the result shape
// does not depend on which caller won the race.
func reusedResult(run gen.Run) (CreateRunResult, error) {
	def, err := dag.Decode(run.Definition)
	if err != nil {
		return CreateRunResult{}, fmt.Errorf("store: CreateRun: decoding stored snapshot of run %s: %w", run.ID, err)
	}
	plan, err := planInstantiation(def)
	if err != nil {
		return CreateRunResult{}, fmt.Errorf("store: CreateRun: replanning stored snapshot of run %s: %w", run.ID, err)
	}
	return CreateRunResult{Run: run, EntrySteps: plan.entry, Reused: true}, nil
}
