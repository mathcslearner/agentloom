package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
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

// MaxIdempotencyTokenLength bounds the idempotency token (ticket 6.5,
// post-M4 audit): the token sits under a btree unique index, whose row
// limit an unbounded token would hit as a raw Postgres error. The API
// rejects longer tokens with a 400 before calling; CreateRun enforces the
// same bound defensively.
const MaxIdempotencyTokenLength = 200

// Failpoint stages, in transaction order. Test-only: the instantiation
// transaction consults instantiateFailpoint after each phase so the
// all-or-nothing test can abort it anywhere.
const (
	stageAfterRunInsert = "after_run_insert"
	stageAfterSteps     = "after_steps"
	stageAfterEdges     = "after_edges"
	stageAfterEvents    = "after_events"
	stageAfterOutbox    = "after_outbox"
)

// instantiateFailpoint, when non-nil, is called with each stage name inside
// the instantiation transaction; a non-nil return aborts it. Installed only
// by tests (export_test.go), nil in production. An atomic.Pointer so an
// arming test racing another CreateRun caller is a logic hazard the test
// must avoid, not a data race.
var instantiateFailpoint atomic.Pointer[func(stage string) error]

func failpoint(stage string) error {
	if fn := instantiateFailpoint.Load(); fn != nil {
		return (*fn)(stage)
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
	// the same token and the same payload returns the original run instead
	// of creating another. Empty means not idempotent. The token is bound
	// to its payload by a stored fingerprint (ticket 6.5): replaying it
	// with a different definition, params, or definition ref fails with
	// *IdempotencyMismatchError instead of silently returning the old run.
	// Longer than MaxIdempotencyTokenLength is rejected. Tokens are global,
	// not scoped per API key (recorded in ADR-007).
	IdempotencyToken string
	// Now is the injected current time (project invariant: no bare
	// time.Now in logic under test); it becomes the run's started_at.
	// Required.
	Now time.Time
	// Trace is the submission's W3C trace context (ticket 7.3, ADR-008):
	// the run's root, persisted on the run row so healed re-dispatches
	// restore linkage, and stamped onto the entry-step outbox rows as the
	// enqueuing span's context. Zero means no context (tracing off).
	Trace TraceContext
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

// stepIDPayload serves the events whose payload is just the step:
// step_ready and step_skipped.
type stepIDPayload struct {
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
	if len(args.IdempotencyToken) > MaxIdempotencyTokenLength {
		return CreateRunResult{}, fmt.Errorf("store: CreateRun: idempotency token exceeds %d bytes", MaxIdempotencyTokenLength)
	}
	if _, err := dag.Validate(args.Definition); err != nil {
		return CreateRunResult{}, fmt.Errorf("store: CreateRun: %w", err)
	}
	plan, err := planInstantiation(args.Definition)
	if err != nil {
		return CreateRunResult{}, fmt.Errorf("store: CreateRun: %w", err)
	}

	if args.IdempotencyToken != "" {
		plan.fingerprint, err = idempotencyFingerprint(plan.snapshot, args.Params, args.DefinitionID)
		if err != nil {
			return CreateRunResult{}, fmt.Errorf("store: CreateRun: fingerprinting payload: %w", err)
		}
		run, err := s.Runs().GetByIdempotencyToken(ctx, args.IdempotencyToken)
		switch {
		case err == nil:
			return checkedReuse(run, args.IdempotencyToken, plan.fingerprint)
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
				return checkedReuse(run, args.IdempotencyToken, plan.fingerprint)
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
	// fingerprint is the payload fingerprint stored alongside a non-empty
	// idempotency token (ticket 6.5); empty when the submission carries no
	// token.
	fingerprint string
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
	var token, fingerprint *string
	if args.IdempotencyToken != "" {
		token = &args.IdempotencyToken
		fingerprint = &p.fingerprint
	}
	startedAt := args.Now
	// The effective failure policy is materialized like the steps'
	// retry_policy (ticket 5.4, ADR-006): absent means fail_fast, and the
	// dead-lettering path never reparses the snapshot.
	onFailure := p.def.OnFailure
	if onFailure == "" {
		onFailure = dag.FailFast
	}
	// The optional wall-clock deadline is materialized as an absolute time
	// (ticket 5.6): the reconciler's deadline scan never reparses the
	// snapshot. Validation guaranteed parseability; a hand-built definition
	// that skipped Validate cannot reach here (CreateRun validates).
	var deadlineAt *time.Time
	if p.def.MaxWallClock != "" {
		d, err := time.ParseDuration(p.def.MaxWallClock)
		if err != nil {
			return gen.Run{}, fmt.Errorf("store: CreateRun: parsing max_wall_clock: %w", err)
		}
		t := args.Now.Add(d)
		deadlineAt = &t
	}
	// The optional spend budget is materialized as integer nano-USD (ticket
	// 10.3, ADR-012) — the same unit as spent_nano_usd, so the claim-time
	// projection compare is exact. NULL means unbudgeted (distinct from 0).
	// on_budget_exceeded is the materialized disposition (like on_failure);
	// absent means park.
	var budgetNanoUSD *int64
	if p.def.BudgetUSD != nil {
		n := usdToNanoUSD(*p.def.BudgetUSD)
		budgetNanoUSD = &n
	}
	onBudgetExceeded := p.def.OnBudgetExceeded
	if onBudgetExceeded == "" {
		onBudgetExceeded = dag.BudgetPark
	}
	// The run's resolved dynamic-expansion caps are materialized like the
	// failure/budget policies (ticket 13.2, ADR-015): ExpandRun and the
	// claim-time guards read them off the row, never reparsing the snapshot,
	// and a worker upgrade cannot change an in-flight run's caps. Always
	// present (defaults resolved) so a pre-13.2 NULL is the only "use compiled
	// defaults" signal, reserved for rows that predate this column.
	expansionCaps, err := json.Marshal(p.def.Expansion.Resolve())
	if err != nil {
		return gen.Run{}, fmt.Errorf("store: CreateRun: marshaling expansion caps: %w", err)
	}
	run, err := q.Runs().Create(ctx, gen.CreateRunParams{
		ID:                     args.RunID,
		DefinitionID:           args.DefinitionID,
		Definition:             p.snapshot,
		Status:                 RunStatusRunning,
		Params:                 args.Params,
		IdempotencyToken:       token,
		IdempotencyFingerprint: fingerprint,
		OnFailure:              string(onFailure),
		StepsTotal:             int32(len(p.def.Steps)), //nolint:gosec // step count is validation-bounded
		StartedAt:              &startedAt,
		DeadlineAt:             deadlineAt,
		TraceParent:            nullableText(args.Trace.Parent),
		TraceState:             nullableText(args.Trace.State),
		BudgetNanoUsd:          budgetNanoUSD,
		OnBudgetExceeded:       string(onBudgetExceeded),
		ExpansionCaps:          expansionCaps,
	})
	if err != nil {
		return gen.Run{}, err
	}
	if err := failpoint(stageAfterRunInsert); err != nil {
		return gen.Run{}, err
	}

	// Steps and edges are materialized through the shared helpers (ticket
	// 13.2): run instantiation and ExpandRun compute placement differently (an
	// entry step vs a spliced one) but materialize each step's envelope
	// policies identically. Instantiation-time rows are all depth 0 with no
	// origin (definition-authored), at graph_version 1.
	steps := make([]gen.CreateRunStepsParams, len(p.def.Steps))
	for i, step := range p.def.Steps {
		status := StepStatusPending
		if p.entrySet[step.ID] {
			status = StepStatusReady
		}
		steps[i], err = stepRowParams(run.ID, step, stepPlacement{
			Status:        status,
			RemainingDeps: p.remaining[step.ID],
			GraphVersion:  1,
		}, args.Now)
		if err != nil {
			return gen.Run{}, err
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
		edges[i] = edgeRowParams(run.ID, e, int32(i), 1, nil, nil) //nolint:gosec // edge count is validation-bounded
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
		if err := p.appendEvent(ctx, q, run.ID, EventStepReady, stepIDPayload{StepID: id}); err != nil {
			return gen.Run{}, err
		}
	}
	if err := failpoint(stageAfterEvents); err != nil {
		return gen.Run{}, err
	}

	for _, id := range p.entry {
		if _, err := q.Outbox().CreateTraced(ctx, run.ID, id, OutboxReasonStepReady, args.Trace); err != nil {
			return gen.Run{}, err
		}
	}
	if err := failpoint(stageAfterOutbox); err != nil {
		return gen.Run{}, err
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

// idempotencyFingerprint binds an idempotency token to its payload: the hex
// SHA-256 over the canonical definition snapshot, the canonicalized params,
// and the definition ref (or an inline marker). Both sides of a comparison
// go through the same canonicalization, so JSON formatting differences —
// key order, whitespace — never produce a spurious mismatch.
func idempotencyFingerprint(snapshot, params json.RawMessage, defID *uuid.UUID) (string, error) {
	canonParams, err := canonicalJSON(params)
	if err != nil {
		return "", fmt.Errorf("canonicalizing params: %w", err)
	}
	h := sha256.New()
	h.Write(snapshot)
	h.Write([]byte{0})
	h.Write(canonParams)
	h.Write([]byte{0})
	if defID != nil {
		h.Write([]byte(defID.String()))
	} else {
		h.Write([]byte("inline"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalJSON round-trips raw JSON through Go's decoder so semantically
// equal documents serialize identically (object keys sorted, formatting
// normalized). nil/empty means the empty object, mirroring what the run
// row stores for absent params.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return emptyJSON, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// checkedReuse guards the duplicate-submission path (ticket 6.5): the
// original run's stored fingerprint must match the replay's, or the token
// is being reused for a different payload and the caller gets
// *IdempotencyMismatchError. A NULL stored fingerprint (pre-0009 row) is
// grandfathered — reuse is allowed unchecked.
func checkedReuse(run gen.Run, token, fingerprint string) (CreateRunResult, error) {
	if run.IdempotencyFingerprint != nil && *run.IdempotencyFingerprint != fingerprint {
		return CreateRunResult{}, fmt.Errorf("store: CreateRun: %w",
			&IdempotencyMismatchError{Token: token, RunID: run.ID})
	}
	return reusedResult(run)
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

// usdToNanoUSD converts a validated (positive) USD budget to integer
// nano-USD, rounding half away from zero. Kept identical to
// cost.USDToNano — the store deliberately does not import internal/cost (it
// holds computed nano values, never prices), so the trivial unit conversion
// is duplicated here rather than pull the pricing leaf into the store.
func usdToNanoUSD(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(math.Round(usd * float64(nanoPerUSD)))
}

// nanoPerUSD mirrors cost.NanoPerUSD (1e9) for usdToNanoUSD.
const nanoPerUSD = int64(1_000_000_000)
