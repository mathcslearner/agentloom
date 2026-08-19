package event

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mathcslearner/agentloom/internal/cost"
	"github.com/mathcslearner/agentloom/internal/dag"
)

// This file defines one payload struct per event type (ADR-018). Each is a
// plain JSON-serializable value with an EventType method (so the store's append
// helper derives the type from the payload) and, for step-scoped events, an
// EventStepID method (so the envelope can lift the step id). Structurally
// similar events get distinct named types on purpose — a payload names exactly
// one event type, which is what makes the writer fence total.
//
// Wire-tag note (ticket 16.1): the step-lifecycle payloads normalize the
// attempt field to the JSON key `attempt`, matching the cost/approval/context
// payloads and the run-status API's `attempt` (pre-16.1 rows used `attempt_no`
// for these five types; those are history on an append-only feed).

// ---- Run instantiation (ticket 2.5) ---------------------------------------

// RunCreated (run_created) is written by run instantiation: the run's name, its
// stored-definition id (empty for an inline definition), and its total step
// count. DefinitionID lets a firehose subscriber filter by definition without a
// per-run DB lookup at the moment a new run is first seen (ticket 16.4).
type RunCreated struct {
	Name string `json:"name"`
	// DefinitionID is the registry definition the run came from; empty for an
	// inline (unregistered) definition. Added additively in 16.4 (ADR-018
	// payload evolution under one envelope version).
	DefinitionID string `json:"definition_id,omitempty"`
	StepsTotal   int    `json:"steps_total"`
}

func (RunCreated) EventType() Type { return TypeRunCreated }

// ---- Step lifecycle (tickets 2.5/2.6, 4.5, 5.2/5.4, 9.2, 11.4, 13.4b) -----

// StepReady (step_ready) marks a step becoming dispatchable.
type StepReady struct {
	StepID string `json:"step_id"`
}

func (StepReady) EventType() Type       { return TypeStepReady }
func (p StepReady) EventStepID() string { return p.StepID }

// StepSkipped (step_skipped) marks a step whose incoming edge did not fire, so
// it is skipped and propagates the skip.
type StepSkipped struct {
	StepID string `json:"step_id"`
}

func (StepSkipped) EventType() Type       { return TypeStepSkipped }
func (p StepSkipped) EventStepID() string { return p.StepID }

// StepRequeued (step_requeued) marks a dead-lettered step reset to ready by the
// requeue op, re-arming its full retry policy (ticket 5.4).
type StepRequeued struct {
	StepID string `json:"step_id"`
}

func (StepRequeued) EventType() Type       { return TypeStepRequeued }
func (p StepRequeued) EventStepID() string { return p.StepID }

// StepClaimed (step_claimed) marks a worker claiming a step; ClaimID is the
// fresh fence.
type StepClaimed struct {
	StepID  string `json:"step_id"`
	ClaimID string `json:"claim_id"`
	Attempt int32  `json:"attempt"`
}

func (StepClaimed) EventType() Type       { return TypeStepClaimed }
func (p StepClaimed) EventStepID() string { return p.StepID }

// StepReclaimed (step_reclaimed) is the lease-expiry takeover (ticket 4.5):
// ClaimID is the displaced holder's cleared fence and Attempt the attempt it
// strands.
type StepReclaimed struct {
	StepID  string `json:"step_id"`
	ClaimID string `json:"claim_id"`
	Attempt int32  `json:"attempt"`
}

func (StepReclaimed) EventType() Type       { return TypeStepReclaimed }
func (p StepReclaimed) EventStepID() string { return p.StepID }

// StepSucceeded (step_succeeded) marks a step's attempt completing successfully.
type StepSucceeded struct {
	StepID  string `json:"step_id"`
	Attempt int32  `json:"attempt"`
}

func (StepSucceeded) EventType() Type       { return TypeStepSucceeded }
func (p StepSucceeded) EventStepID() string { return p.StepID }

// StepFailed (step_failed) is the retired resting-failure event (pre-5.4).
// Retained in the vocabulary because pre-5.4 rows carry it; no writer emits it
// today (steps dead-letter instead).
type StepFailed struct {
	StepID  string `json:"step_id"`
	Attempt int32  `json:"attempt"`
}

func (StepFailed) EventType() Type       { return TypeStepFailed }
func (p StepFailed) EventStepID() string { return p.StepID }

// StepRetryScheduled (step_retry_scheduled) records a classified-retryable
// attempt failure and the routing to retrying (ticket 5.2): which attempt
// failed, its class, and when the next attempt is due.
type StepRetryScheduled struct {
	StepID        string    `json:"step_id"`
	Attempt       int32     `json:"attempt"`
	Class         string    `json:"class"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

func (StepRetryScheduled) EventType() Type       { return TypeStepRetryScheduled }
func (p StepRetryScheduled) EventStepID() string { return p.StepID }

// StepThrottled (step_throttled) records a fleet-wide rate-limit denial that
// deferred a step without executing it (ticket 9.2): the resource, which bucket
// denied, the limiter's retry_after, and when the re-dispatch is due.
type StepThrottled struct {
	StepID        string    `json:"step_id"`
	Attempt       int32     `json:"attempt"`
	Resource      string    `json:"resource"`
	Bucket        string    `json:"bucket"`
	RetryAfter    string    `json:"retry_after"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

func (StepThrottled) EventType() Type       { return TypeStepThrottled }
func (p StepThrottled) EventStepID() string { return p.StepID }

// StepSemanticRetry (step_semantic_retry_scheduled) records an output-validation
// failure routed to a feedback-augmented re-attempt (ticket 11.4): the semantic
// depth of max_attempts, the failing verdict's issue count, and when the
// re-attempt is due. Distinct from step_retry_scheduled so the transport and
// semantic budgets read apart on the feed.
type StepSemanticRetry struct {
	StepID          string    `json:"step_id"`
	Attempt         int32     `json:"attempt"`
	SemanticAttempt int       `json:"semantic_attempt"`
	MaxAttempts     int       `json:"max_attempts"`
	IssueCount      int       `json:"issue_count"`
	NextAttemptAt   time.Time `json:"next_attempt_at"`
}

func (StepSemanticRetry) EventType() Type       { return TypeStepSemanticRetry }
func (p StepSemanticRetry) EventStepID() string { return p.StepID }

// StepDeadLettered (step_dead_lettered) marks a step reaching terminal failure
// (ticket 5.4): the source, the judged class (empty for poison), the attempt
// count at death, and the dead_letters seq the full record lives under.
type StepDeadLettered struct {
	StepID   string `json:"step_id"`
	Source   string `json:"source"`
	Class    string `json:"class,omitempty"`
	Attempts int32  `json:"attempts"`
	Seq      int32  `json:"seq"`
}

func (StepDeadLettered) EventType() Type       { return TypeStepDeadLettered }
func (p StepDeadLettered) EventStepID() string { return p.StepID }

// StepCancelled (step_cancelled) marks a step written off — it can never become
// ready (ticket 5.4/5.6): the reason.
type StepCancelled struct {
	StepID string `json:"step_id"`
	Reason string `json:"reason"`
}

func (StepCancelled) EventType() Type       { return TypeStepCancelled }
func (p StepCancelled) EventStepID() string { return p.StepID }

// StepRevived (step_revived) marks a requeue making a written-off step's
// readiness possible again (ticket 5.4): back to pending.
type StepRevived struct {
	StepID string `json:"step_id"`
	Reason string `json:"reason"`
}

func (StepRevived) EventType() Type       { return TypeStepRevived }
func (p StepRevived) EventStepID() string { return p.StepID }

// StepCollected (step_collected) marks a map instance that failed terminally
// under on_item_failure=collect_errors and was tolerated (ticket 13.4b): the
// judged class and the attempt count at the tolerated failure.
type StepCollected struct {
	StepID   string `json:"step_id"`
	Class    string `json:"class,omitempty"`
	Attempts int32  `json:"attempts"`
}

func (StepCollected) EventType() Type       { return TypeStepCollected }
func (p StepCollected) EventStepID() string { return p.StepID }

// ---- Run lifecycle (tickets 2.5/2.6, 5.4, 5.6) ----------------------------

// RunSucceeded (run_succeeded) marks the run reaching terminal success.
type RunSucceeded struct{}

func (RunSucceeded) EventType() Type { return TypeRunSucceeded }

// RunFailed (run_failed) marks the run reaching terminal failure.
type RunFailed struct{}

func (RunFailed) EventType() Type { return TypeRunFailed }

// RunResumed (run_resumed) marks a requeue re-opening a failed run (ticket 5.4).
type RunResumed struct{}

func (RunResumed) EventType() Type { return TypeRunResumed }

// RunParked (run_parked) marks dispatch paused with a typed reason (ticket 5.6):
// manual, budget_exceeded, or awaiting_human.
type RunParked struct {
	Reason string `json:"reason"`
}

func (RunParked) EventType() Type { return TypeRunParked }

// RunUnparked (run_unparked) marks dispatch resumed by the unpark op (ticket 5.6).
type RunUnparked struct{}

func (RunUnparked) EventType() Type { return TypeRunUnparked }

// RunCancelling (run_cancelling) marks a cancel requested with a typed reason
// (ticket 5.6) — the quiescing state.
type RunCancelling struct {
	Reason string `json:"reason"`
}

func (RunCancelling) EventType() Type { return TypeRunCancelling }

// RunCancelled (run_cancelled) marks the cancel finalized — every step terminal.
type RunCancelled struct{}

func (RunCancelled) EventType() Type { return TypeRunCancelled }

// ---- Cost & budget (tickets 10.2–10.5, ADR-012) ---------------------------

// CostUpdated (cost_updated) is written once per cost-bearing attempt (ticket
// 10.5): the attempt's charge plus the run's running spend/saved totals after
// the bump. The totals are non-decreasing in seq order (shared run lock + seq
// with the aggregate bump). Feeds the M18 live meter.
type CostUpdated struct {
	StepID  string `json:"step_id"`
	Attempt int32  `json:"attempt"`
	// Entry is the charge kind (attempt now; judge/compaction overhead in M11/M12).
	Entry string `json:"entry"`
	// Resource is the ADR-010/ADR-012 resource billed ("mock:sim-1", "tool:paid_search").
	Resource string `json:"resource"`
	// CacheHit marks a $0 cache-served charge (CostNanoUSD 0, SavedNanoUSD set).
	CacheHit bool `json:"cache_hit,omitempty"`
	// Overhead flags a judge/summarization charge (ADR-012 rule 4).
	Overhead bool `json:"overhead,omitempty"`
	// CostNanoUSD / SavedNanoUSD are this attempt's charge and cache savings.
	CostNanoUSD  int64 `json:"cost_nano_usd,omitempty"`
	SavedNanoUSD int64 `json:"saved_nano_usd,omitempty"`
	// RunSpentNanoUSD / RunSavedNanoUSD are the run's totals after this charge.
	RunSpentNanoUSD int64 `json:"run_spent_nano_usd"`
	RunSavedNanoUSD int64 `json:"run_saved_nano_usd"`
	// BudgetNanoUSD is the run's budget (nil = unbudgeted).
	BudgetNanoUSD *int64 `json:"budget_nano_usd,omitempty"`
}

func (CostUpdated) EventType() Type       { return TypeCostUpdated }
func (p CostUpdated) EventStepID() string { return p.StepID }

// CostUnknownModel (cost_unknown_model) is written when a cost-bearing attempt
// named a model with no catalog entry and was priced at the fallback rate
// (ticket 10.2): the unpriced model and the fallback rate. It mirrors
// cost.UnknownModelWarning's shape and follows the cost_updated event.
type CostUnknownModel struct {
	Model    string    `json:"model"`
	Fallback cost.Rate `json:"fallback"`
}

func (CostUnknownModel) EventType() Type { return TypeCostUnknownModel }

// CostUnknownModelFrom projects the cost package's warning value onto the event
// payload — the store/engine path stays typed end to end.
func CostUnknownModelFrom(w cost.UnknownModelWarning) CostUnknownModel {
	return CostUnknownModel{Model: w.Model, Fallback: w.Fallback}
}

// BudgetExceeded (budget_exceeded) records a claim's projected spend crossing a
// budget limit (ticket 10.3): the resource, the run spend and estimate that made
// the projection, the limit crossed, and the action (park / fail).
type BudgetExceeded struct {
	StepID  string `json:"step_id"`
	Attempt int32  `json:"attempt"`
	// Resource is the resource the claim would bill to; empty when unknown pre-flight.
	Resource string `json:"resource,omitempty"`
	// Limit is which cap was crossed: run | step_usd | step_tokens.
	Limit string `json:"limit"`
	// Action is what the engine did: park | fail.
	Action string `json:"action"`
	// SpentNanoUSD / EstimateNanoUSD / ProjectedNanoUSD / BudgetNanoUSD describe
	// a USD projection (run or step_usd limits).
	SpentNanoUSD     int64 `json:"spent_nano_usd,omitempty"`
	EstimateNanoUSD  int64 `json:"estimate_nano_usd,omitempty"`
	ProjectedNanoUSD int64 `json:"projected_nano_usd,omitempty"`
	BudgetNanoUSD    int64 `json:"budget_nano_usd,omitempty"`
	// ProjectedTokens / MaxTokens describe a step_tokens projection.
	ProjectedTokens int64 `json:"projected_tokens,omitempty"`
	MaxTokens       int64 `json:"max_tokens,omitempty"`
}

func (BudgetExceeded) EventType() Type       { return TypeBudgetExceeded }
func (p BudgetExceeded) EventStepID() string { return p.StepID }

// RunBudgetUpdated (run_budget_updated) records a PATCH …/budget raise (ticket
// 10.3): the previous and new budget in nano-USD.
type RunBudgetUpdated struct {
	PreviousNanoUSD int64 `json:"previous_nano_usd"`
	BudgetNanoUSD   int64 `json:"budget_nano_usd"`
}

func (RunBudgetUpdated) EventType() Type { return TypeRunBudgetUpdated }

// ModelDowngraded (model_downgraded) records the claim-time budget check routing
// an llm step to a cheaper model in its fallback chain (ticket 10.4): the
// from/to models and resources, the trigger, and the spend/budget projection.
type ModelDowngraded struct {
	StepID  string `json:"step_id"`
	Attempt int32  `json:"attempt"`
	// FromModel/ToModel are the authored model ids.
	FromModel string `json:"from_model"`
	ToModel   string `json:"to_model"`
	// FromResource/ToResource are the resolved ADR-010 pricing keys.
	FromResource string `json:"from_resource"`
	ToResource   string `json:"to_resource"`
	// Trigger is why the downgrade fired: budget_threshold | budget_projection.
	Trigger string `json:"trigger"`
	// Limit is which budget the projection trigger was measured against
	// (run | step_usd); empty for a pure threshold trigger.
	Limit string `json:"limit,omitempty"`
	// ThresholdFraction is the fallback's at_budget_fraction (threshold trigger only).
	ThresholdFraction float64 `json:"threshold_fraction,omitempty"`
	// SpentNanoUSD / BudgetNanoUSD describe the run's spend and budget at the
	// decision; From/ToEstimateNanoUSD are the priced pre-flight estimates.
	SpentNanoUSD        int64 `json:"spent_nano_usd,omitempty"`
	BudgetNanoUSD       int64 `json:"budget_nano_usd,omitempty"`
	FromEstimateNanoUSD int64 `json:"from_estimate_nano_usd,omitempty"`
	ToEstimateNanoUSD   int64 `json:"to_estimate_nano_usd,omitempty"`
}

func (ModelDowngraded) EventType() Type       { return TypeModelDowngraded }
func (p ModelDowngraded) EventStepID() string { return p.StepID }

// ---- Context & memory (tickets 12.2–12.4, ADR-014) ------------------------

// BlackboardUpdated (blackboard_updated) records a run-scoped blackboard key
// gaining a new version (ticket 12.2): the key, version, tags, token count, and
// the author step/attempt.
type BlackboardUpdated struct {
	Key           string   `json:"key"`
	Version       int32    `json:"version"`
	Tags          []string `json:"tags"`
	TokenCount    int32    `json:"token_count"`
	AuthorStepID  string   `json:"author_step_id,omitempty"`
	AuthorAttempt int32    `json:"author_attempt,omitempty"`
}

func (BlackboardUpdated) EventType() Type { return TypeBlackboardUpdated }

// EventStepID is the author step (the step that wrote the version), so a
// blackboard update filters onto the writer's timeline.
func (p BlackboardUpdated) EventStepID() string { return p.AuthorStepID }

// ContextSourceRecord is one source's disposition in a context_assembled event.
type ContextSourceRecord struct {
	Index  int    `json:"index"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Ref    string `json:"ref,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Tokens int    `json:"tokens"`
	Pinned bool   `json:"pinned,omitempty"`
}

// ContextAssembled (context_assembled) is the pre-execution context-assembly
// manifest for an llm step (ticket 12.3): per-source dispositions, the counter
// fingerprint, and the token totals (post-compaction, with raw figures for the
// audit).
type ContextAssembled struct {
	StepID             string                `json:"step_id"`
	Attempt            int32                 `json:"attempt"`
	CounterID          string                `json:"counter_id"`
	Sources            []ContextSourceRecord `json:"sources"`
	ContextTokens      int                   `json:"context_tokens"`
	PreflightTokens    int                   `json:"preflight_tokens"`
	BudgetTokens       int                   `json:"budget_tokens,omitempty"`
	BudgetSource       string                `json:"budget_source,omitempty"`
	ContextWindow      int                   `json:"context_window,omitempty"`
	RawContextTokens   int                   `json:"raw_context_tokens,omitempty"`
	RawPreflightTokens int                   `json:"raw_preflight_tokens,omitempty"`
	Revisions          int                   `json:"revisions,omitempty"`
	Summaries          int                   `json:"summaries,omitempty"`
}

func (ContextAssembled) EventType() Type       { return TypeContextAssembled }
func (p ContextAssembled) EventStepID() string { return p.StepID }

// ContextRevisionActionRecord is one entry's drop/truncate/summarize action.
type ContextRevisionActionRecord struct {
	SourceIndex  int    `json:"source_index"`
	Name         string `json:"name"`
	Action       string `json:"action"`
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
}

// ContextRevisionSummaryRecord is one summarization's provenance.
type ContextRevisionSummaryRecord struct {
	Key           string   `json:"key"`
	Version       int      `json:"version"`
	ParentVersion int      `json:"parent_version,omitempty"`
	Model         string   `json:"model"`
	Resource      string   `json:"resource"`
	SpanNames     []string `json:"span_names,omitempty"`
	SpanTokens    int      `json:"span_tokens"`
	SummaryTokens int      `json:"summary_tokens"`
	CacheHit      bool     `json:"cache_hit,omitempty"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
}

// ContextRevision (context_revision) is one deterministic compaction strategy's
// application to shrink an over-budget assembly (ticket 12.4): what ran, its
// parameters, the framed-request tokens before/after, and the per-entry actions.
type ContextRevision struct {
	StepID       string                         `json:"step_id"`
	Attempt      int32                          `json:"attempt"`
	Index        int                            `json:"index"`
	Strategy     string                         `json:"strategy"`
	N            *int                           `json:"n,omitempty"`
	MinTokens    *int                           `json:"min_tokens,omitempty"`
	Budget       int                            `json:"budget"`
	TokensBefore int                            `json:"tokens_before"`
	TokensAfter  int                            `json:"tokens_after"`
	Changed      bool                           `json:"changed"`
	Actions      []ContextRevisionActionRecord  `json:"actions,omitempty"`
	Summaries    []ContextRevisionSummaryRecord `json:"summaries,omitempty"`
	Error        string                         `json:"error,omitempty"`
	Kept         []string                       `json:"kept,omitempty"`
}

func (ContextRevision) EventType() Type       { return TypeContextRevision }
func (p ContextRevision) EventStepID() string { return p.StepID }

// ---- Dynamic graph, loops, guards (tickets 13.2, 14.3/14.4) ---------------

// GraphExpanded (graph_expanded) records a planner/map/loop expansion splicing
// steps and edges into the running graph (ticket 13.2): the origin, the
// graph_version transition, the injected depth, the verbatim delta, and the
// readied/widened step ids.
type GraphExpanded struct {
	OriginStep  string `json:"origin_step"`
	OriginKind  string `json:"origin_kind"`
	FromVersion int32  `json:"from_version"`
	ToVersion   int32  `json:"to_version"`
	Depth       int32  `json:"depth"`
	// Delta is the verbatim validated plan (new steps + edges).
	Delta dag.PlanOutput `json:"delta"`
	// Readied / Widened mirror ExpandRunResult for the audit feed.
	Readied []string `json:"readied,omitempty"`
	Widened []string `json:"widened,omitempty"`
}

func (GraphExpanded) EventType() Type { return TypeGraphExpanded }

// EventStepID is the origin step whose completion drove the expansion.
func (p GraphExpanded) EventStepID() string { return p.OriginStep }

// UnmarshalJSON decodes a graph_expanded payload. The delta reuses the
// definition's dag.Step, whose Config is the dag.StepConfig *interface*, which
// a plain json.Unmarshal cannot populate — so any consumer that re-decodes a
// stored/published envelope (the run WS live Tailer, the multi-run firehose)
// would otherwise fail with "cannot unmarshal object into … dag.StepConfig".
// We route the delta through the canonical plan decoder, which knows the
// per-type config shapes. A delta with no steps (e.g. the zero-value catalog
// sample) decodes plainly — there is no interface field to populate.
func (p *GraphExpanded) UnmarshalJSON(data []byte) error {
	var raw struct {
		OriginStep  string          `json:"origin_step"`
		OriginKind  string          `json:"origin_kind"`
		FromVersion int32           `json:"from_version"`
		ToVersion   int32           `json:"to_version"`
		Depth       int32           `json:"depth"`
		Delta       json.RawMessage `json:"delta"`
		Readied     []string        `json:"readied"`
		Widened     []string        `json:"widened"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.OriginStep = raw.OriginStep
	p.OriginKind = raw.OriginKind
	p.FromVersion = raw.FromVersion
	p.ToVersion = raw.ToVersion
	p.Depth = raw.Depth
	p.Readied = raw.Readied
	p.Widened = raw.Widened

	if len(raw.Delta) == 0 {
		p.Delta = dag.PlanOutput{}
		return nil
	}
	// Peek at the step count: with no steps, plain decode is safe (no interface
	// field is populated) and avoids the plan decoder's shape validation, which
	// the zero-value sample would fail.
	var peek struct {
		Steps []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(raw.Delta, &peek); err != nil {
		return err
	}
	if len(peek.Steps) == 0 {
		var d dag.PlanOutput
		if err := json.Unmarshal(raw.Delta, &d); err != nil {
			return err
		}
		p.Delta = d
		return nil
	}
	plan, err := dag.DecodePlanOutput(raw.Delta)
	if err != nil {
		return fmt.Errorf("decoding graph_expanded delta: %w", err)
	}
	p.Delta = *plan
	return nil
}

// LoopExhausted (loop_exhausted) records a marked loop edge reaching its
// max_iterations bound while its condition still signaled "iterate again"
// (ticket 14.3): which loop, the iteration reached vs the cap, the condition,
// and the termination policy with the action taken.
type LoopExhausted struct {
	LoopSourceStep     string `json:"loop_source_step"`
	LoopSourceInstance string `json:"loop_source_instance"`
	BodyEntry          string `json:"body_entry"`
	Iteration          int    `json:"iteration"`
	MaxIterations      int    `json:"max_iterations"`
	Condition          string `json:"condition"`
	Policy             string `json:"policy"`
	Action             string `json:"action"`
}

func (LoopExhausted) EventType() Type { return TypeLoopExhausted }

// EventStepID is the concrete completing loop instance.
func (p LoopExhausted) EventStepID() string { return p.LoopSourceInstance }

// LoopNoProgress (loop_no_progress) records a loop's opt-in no-progress guard
// firing on two consecutive identical output hashes (ticket 14.4).
type LoopNoProgress struct {
	LoopSourceStep     string `json:"loop_source_step"`
	LoopSourceInstance string `json:"loop_source_instance"`
	ComparedStep       string `json:"compared_step"`
	Path               string `json:"path,omitempty"`
	Iteration          int    `json:"iteration"`
	PrevInstance       string `json:"prev_instance"`
	CurInstance        string `json:"cur_instance"`
	Hash               string `json:"hash"`
	Policy             string `json:"policy"`
	Action             string `json:"action"`
}

func (LoopNoProgress) EventType() Type { return TypeLoopNoProgress }

// EventStepID is the concrete completing loop instance.
func (p LoopNoProgress) EventStepID() string { return p.LoopSourceInstance }

// GuardTripped (guard_tripped) records a run-level guard (an expansion cap, or
// the wall-clock deadline) halting the run (ticket 14.4): which limit, the
// current value, the cap, the unit, and the action (fail / cancel).
type GuardTripped struct {
	Guard   string `json:"guard"`
	StepID  string `json:"step_id,omitempty"`
	Current int64  `json:"current"`
	Cap     int64  `json:"cap"`
	Unit    string `json:"unit"`
	Action  string `json:"action"`
}

func (GuardTripped) EventType() Type { return TypeGuardTripped }

// EventStepID is the step whose completion tripped an expansion cap, or empty
// for a run-wide guard (the wall-clock deadline).
func (p GuardTripped) EventStepID() string { return p.StepID }

// ---- Human-in-the-loop (tickets 15.2–15.5, ADR-017) -----------------------

// ApprovalRequested (approval_requested) records a human_approval step parking
// without a lease and writing a pending approval (ticket 15.2): the approval id,
// title, allowed decisions, whether edits are permitted, and the timeout
// deadline (nil = wait indefinitely).
type ApprovalRequested struct {
	ApprovalID       string     `json:"approval_id"`
	StepID           string     `json:"step_id"`
	Attempt          int32      `json:"attempt"`
	Title            string     `json:"title"`
	AllowedDecisions []string   `json:"allowed_decisions"`
	AllowEdit        bool       `json:"allow_edit,omitempty"`
	TimeoutAt        *time.Time `json:"timeout_at,omitempty"`
}

func (ApprovalRequested) EventType() Type       { return TypeApprovalRequested }
func (p ApprovalRequested) EventStepID() string { return p.StepID }

// ApprovalCancelled (approval_cancelled) records a pending approval cancelled
// because its run was cancelled while the step was parked (ticket 15.2).
type ApprovalCancelled struct {
	ApprovalID string `json:"approval_id"`
	StepID     string `json:"step_id"`
	Reason     string `json:"reason"`
}

func (ApprovalCancelled) EventType() Type       { return TypeApprovalCancelled }
func (p ApprovalCancelled) EventStepID() string { return p.StepID }

// ApprovalDecided (approval_decided) records a pending approval decided through
// the single arbiter CAS (ticket 15.3): the decision, whether the payload was
// edited, the actor, the comment, and the source (human / timeout).
type ApprovalDecided struct {
	ApprovalID string `json:"approval_id"`
	StepID     string `json:"step_id"`
	Attempt    int32  `json:"attempt"`
	Decision   string `json:"decision"`
	Edited     bool   `json:"edited,omitempty"`
	Comment    string `json:"comment,omitempty"`
	DecidedBy  string `json:"decided_by"`
	Source     string `json:"source"`
}

func (ApprovalDecided) EventType() Type       { return TypeApprovalDecided }
func (p ApprovalDecided) EventStepID() string { return p.StepID }

// ApprovalExpired (approval_expired) records a pending approval's timeout passing
// and its on_timeout policy being applied (ticket 15.4). Distinct from
// approval_decided so the audit separates an operator decision from an automatic
// expiry.
type ApprovalExpired struct {
	ApprovalID string     `json:"approval_id"`
	StepID     string     `json:"step_id"`
	Attempt    int32      `json:"attempt"`
	Policy     string     `json:"policy"`
	Decision   string     `json:"decision,omitempty"`
	Action     string     `json:"action"`
	TimeoutAt  *time.Time `json:"timeout_at,omitempty"`
}

func (ApprovalExpired) EventType() Type       { return TypeApprovalExpired }
func (p ApprovalExpired) EventStepID() string { return p.StepID }

// ApprovalNotified (approval_notified) records a pending-approval notification
// delivered to the configured webhook (ticket 15.5): best-effort telemetry, the
// target host only (never the full URL, which may carry a token).
type ApprovalNotified struct {
	ApprovalID string `json:"approval_id"`
	StepID     string `json:"step_id"`
	TargetHost string `json:"target_host"`
	Attempts   int    `json:"attempts"`
	StatusCode int    `json:"status_code"`
}

func (ApprovalNotified) EventType() Type       { return TypeApprovalNotified }
func (p ApprovalNotified) EventStepID() string { return p.StepID }

// ApprovalNotificationFailed (approval_notification_failed) records a
// notification that could not be delivered (ticket 15.5): a warning, not a
// failure — the run stays parked and decidable. Reason never includes the URL,
// headers, or response body.
type ApprovalNotificationFailed struct {
	ApprovalID string `json:"approval_id"`
	StepID     string `json:"step_id"`
	TargetHost string `json:"target_host"`
	Attempts   int    `json:"attempts"`
	Reason     string `json:"reason"`
}

func (ApprovalNotificationFailed) EventType() Type       { return TypeApprovalNotificationFailed }
func (p ApprovalNotificationFailed) EventStepID() string { return p.StepID }
