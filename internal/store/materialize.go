package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// stepPlacement is where a run_steps row sits in the graph — everything about
// a materialized step that is *not* a function of its dag.Step alone. Run
// instantiation (2.5) and expansion (13.2) compute it differently (an entry
// step vs a spliced one), then hand it to stepRowParams so the envelope-policy
// materialization is shared and cannot drift between the two paths.
type stepPlacement struct {
	Status        string
	RemainingDeps int32
	GraphVersion  int32
	// Depth is the expansion nesting depth (ADR-015): 0 at instantiation,
	// origin.depth+1 for an injected step.
	Depth int32
	// OriginStep / OriginKind name the expansion that introduced the row; both
	// nil for a definition-authored step.
	OriginStep *string
	OriginKind *string
}

// stepRowParams materializes one dag.Step into a run_steps insert row — the
// shared materialization CreateRun (2.5, entry steps) and ExpandRun (13.2,
// ADR-015, injected steps) both use, so an injected step's every envelope
// policy (retry, timeout, cache, budget, validation, blackboard, context) is
// materialized byte-identically to an authored one. The step is assumed
// validated (CreateRun validates the definition; ExpandRun validates the
// plan) — a marshal failure here is corrupt in-memory state, surfaced as an
// error rather than swallowed. now becomes the row's updated_at (the
// reconciler's staleness scan reads it — ADR-004 timestamp policy).
func stepRowParams(runID uuid.UUID, step dag.Step, p stepPlacement, now time.Time) (gen.CreateRunStepsParams, error) {
	var config json.RawMessage
	if step.Config != nil {
		b, err := json.Marshal(step.Config)
		if err != nil {
			return gen.CreateRunStepsParams{}, fmt.Errorf("marshaling config of step %q: %w", step.ID, err)
		}
		config = b
	}
	// The *effective* retry policy — authored fields merged over engine
	// defaults — is materialized per step (ticket 5.2, ADR-006): the failure
	// path never reparses the snapshot, and a worker upgrade cannot change an
	// in-flight run's retry behavior.
	retryPolicy, err := json.Marshal(dag.ResolveRetryPolicy(step.Retry))
	if err != nil {
		return gen.CreateRunStepsParams{}, fmt.Errorf("marshaling retry policy of step %q: %w", step.ID, err)
	}
	// The per-attempt execution timeout (ticket 5.3); nil means no timeout.
	var timeout *string
	if step.Timeout != "" {
		timeout = &step.Timeout
	}
	// The remaining authored envelope blocks are each materialized as a
	// nullable JSONB column — NULL when the block was absent (tickets 9.5 cache,
	// 10.3 budget, 11.1 validation, 12.2 blackboard, 12.3 context). Only the
	// nil check differs per concrete type, so they stay explicit here.
	var cachePolicy json.RawMessage
	if step.Cache != nil {
		if cachePolicy, err = json.Marshal(step.Cache); err != nil {
			return gen.CreateRunStepsParams{}, fmt.Errorf("marshaling cache policy of step %q: %w", step.ID, err)
		}
	}
	var budgetPolicy json.RawMessage
	if step.Budget != nil {
		if budgetPolicy, err = json.Marshal(step.Budget); err != nil {
			return gen.CreateRunStepsParams{}, fmt.Errorf("marshaling budget policy of step %q: %w", step.ID, err)
		}
	}
	var validationPolicy json.RawMessage
	if step.Validation != nil {
		if validationPolicy, err = json.Marshal(step.Validation); err != nil {
			return gen.CreateRunStepsParams{}, fmt.Errorf("marshaling validation policy of step %q: %w", step.ID, err)
		}
	}
	var blackboardPolicy json.RawMessage
	if step.Blackboard != nil {
		if blackboardPolicy, err = json.Marshal(step.Blackboard); err != nil {
			return gen.CreateRunStepsParams{}, fmt.Errorf("marshaling blackboard policy of step %q: %w", step.ID, err)
		}
	}
	var contextPolicy json.RawMessage
	if step.Context != nil {
		if contextPolicy, err = json.Marshal(step.Context); err != nil {
			return gen.CreateRunStepsParams{}, fmt.Errorf("marshaling context policy of step %q: %w", step.ID, err)
		}
	}
	return gen.CreateRunStepsParams{
		RunID:            runID,
		StepID:           step.ID,
		StepType:         string(step.Type),
		Config:           config,
		RetryPolicy:      retryPolicy,
		Timeout:          timeout,
		CachePolicy:      cachePolicy,
		BudgetPolicy:     budgetPolicy,
		ValidationPolicy: validationPolicy,
		BlackboardPolicy: blackboardPolicy,
		ContextPolicy:    contextPolicy,
		Status:           p.Status,
		RemainingDeps:    p.RemainingDeps,
		GraphVersion:     p.GraphVersion,
		Depth:            p.Depth,
		OriginStep:       p.OriginStep,
		OriginKind:       p.OriginKind,
		UpdatedAt:        now,
	}, nil
}

// edgeRowParams materializes one dag.Edge into a run_edges insert row at the
// given ordinal — shared by CreateRun (2.5) and ExpandRun (13.2). origin names
// the expansion that spliced it in (both nil for an authored edge).
func edgeRowParams(runID uuid.UUID, e dag.Edge, ordinal, graphVersion int32, originStep, originKind *string) gen.CreateRunEdgesParams {
	edge := gen.CreateRunEdgesParams{
		RunID:        runID,
		Ordinal:      ordinal,
		FromStep:     e.From,
		ToStep:       e.To,
		EdgeType:     string(e.Type),
		GraphVersion: graphVersion,
		OriginStep:   originStep,
		OriginKind:   originKind,
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
	// The decision marker (ticket 15.3, ADR-017): a normal edge leaving a
	// human_approval step under on_reject: route. NULL for an unmarked edge,
	// so 15.3's reject-routing gate is a column comparison. Carried on both
	// authored edges and engine-injected instances (a gate inside a loop keeps
	// its reject edge across `#k`).
	if e.Decision != "" {
		dec := string(e.Decision)
		edge.Decision = &dec
	}
	return edge
}
