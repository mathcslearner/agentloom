package engine

// Context assembly in the execution pipeline (ticket 12.3, ADR-014): before a
// context-bearing llm step runs, the engine assembles its declarative
// `context` spec — upstream step outputs, blackboard entries, retrieval
// results, literals — into a preamble it prepends to the request, records the
// per-source disposition on a context_assembled event, and hands the executor
// the augmented config. The assembly (internal/contextmgr) is a pure function
// of store state; this file owns the data fetch, the failure routing, and the
// audit record.
//
// It sits after feedback injection and before the cache read (claim.go), so the
// assembled context is a cache-key input by construction (ADR-011/014): the
// preamble is prepended into the request the cache binding keys on. A missing
// source (on_missing: error, the default) or a config error (no board /
// retriever wired, an unknown retriever, an executor that cannot inject
// context) is a permanent step failure before any provider call; a transport
// failure of a source read decides nothing and redelivers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// contextError marks a deterministic context-assembly failure — a corrupt
// materialized spec, a missing source under the default (error) policy, an
// unwired/unknown backend, an executor that cannot inject context, or an
// injector rewrite failure. All are deterministic (the same store state
// resolves the same way, ADR-006 row 15), so the engine records the failure
// with ClassPermanent before any provider call. A transport failure of a
// source read is returned bare instead — nothing was decided, the delivery
// redelivers.
type contextError struct {
	cause error
}

func (e *contextError) Error() string { return "assembling step context: " + e.cause.Error() }
func (e *contextError) Unwrap() error { return e.cause }

// assembleContext resolves a claimed step's materialized `context` spec and
// returns the config with the assembled preamble prepended. A step with no
// context block returns its config untouched with zero reads (the common fast
// path). A deterministic failure is a *contextError (permanent completion); a
// transport failure is returned bare (redeliver). The assembly manifest is
// appended as a context_assembled event before the request runs, so the
// pre-execution context decision survives a crash mid-call.
func (e *Engine) assembleContext(ctx context.Context, step gen.RunStep, executor exec.Executor, sc exec.StepContext) (json.RawMessage, error) {
	if len(step.ContextPolicy) == 0 {
		return sc.Config, nil
	}
	ctx, span := e.tracer.Start(ctx, "step.context")
	defer span.End()

	var spec dag.ContextSpec
	if err := json.Unmarshal(step.ContextPolicy, &spec); err != nil {
		// Corrupt materialized spec — deterministic, so permanent.
		return nil, &contextError{cause: fmt.Errorf("decoding context policy: %w", err)}
	}
	if len(spec.Sources) == 0 {
		return sc.Config, nil
	}
	injector, ok := executor.(exec.ContextInjector)
	if !ok {
		// A step type whose executor cannot prepend context (validated at submit
		// as llm-family, so this is version skew or corrupt state) — permanent.
		return nil, &contextError{cause: fmt.Errorf("step type %q does not support context assembly", step.StepType)}
	}

	sources, srcErr := e.contextSources(ctx, step, spec, sc)
	if srcErr != nil {
		return nil, srcErr // transport (the referenced-output read failed): redeliver
	}
	counter := e.stepCounter(executor, sc)
	asm, err := contextmgr.Assemble(ctx, spec, counter, sources)
	if err != nil {
		var mse *contextmgr.MissingSourceError
		var ce *contextmgr.ConfigError
		if errors.As(err, &mse) || errors.As(err, &ce) {
			return nil, &contextError{cause: err}
		}
		return nil, fmt.Errorf("engine: assembling context: %w", err) // transport
	}

	// The assembled context becomes the request preamble; the compaction stage
	// (tickets 12.4/12.5) may shrink it below the step budget first.
	preamble := asm.Preamble
	reports := asm.Sources
	contextTokens := asm.ContextTokens
	rawContextTokens := asm.ContextTokens
	rawPreflight := 0
	budgetTokens := 0
	var revisions []store.ContextRevisionEvent
	var costRows []store.AttemptCostArgs
	var summaries []contextmgr.SummaryAction

	if spec.BudgetTokens != nil {
		// Compaction (ticket 12.4): the budget is over the whole framed request,
		// so a PreflightCounter is required — a step type that cannot count its
		// request cannot enforce a budget (deterministic → permanent). The
		// measure closure injects a candidate preamble and counts the request,
		// the same arithmetic 12.6's window guardrail uses.
		pc, pcok := executor.(exec.PreflightCounter)
		if !pcok {
			return nil, &contextError{cause: fmt.Errorf("step type %q cannot enforce a context budget (no preflight counter)", step.StepType)}
		}
		budgetTokens = *spec.BudgetTokens
		measure := func(candidate string) (int, error) {
			scM := sc
			aug, ierr := injector.WithContext(scM, candidate)
			if ierr != nil {
				return 0, ierr
			}
			scM.Config = aug
			return pc.PreflightTokens(scM, counter)
		}
		if n, merr := measure(asm.Preamble); merr == nil {
			rawPreflight = n
		}
		comp, cerr := contextmgr.Compact(ctx, asm, contextmgr.Policy{
			Budget: budgetTokens, Pipeline: spec.Compaction,
			Summarizer: e.stepSummarizer(), Board: sc.Blackboard,
		}, counter, measure)
		if cerr != nil {
			// A caller-context cancellation (surfaced by the summarizer, ticket
			// 12.5) decides nothing — return it bare so the engine keeps its
			// timeout/cancelled judgment and the delivery redelivers.
			if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
				return nil, cerr
			}
			// An OverBudgetError (pipeline could not fit, or pins alone exceed the
			// budget) and a measure-build failure are both deterministic — the
			// same request assembles the same way every attempt — so the step
			// fails permanently before any provider call (ADR-014's hard
			// guarantee: overflow is never sent). Before failing, record the
			// revisions that DID run and ledger any summarizer overhead that
			// billed (ticket 12.5) — a failed compaction is auditable and its
			// spend metered, not silent.
			var obe *contextmgr.OverBudgetError
			if errors.As(cerr, &obe) {
				if rerr := e.recordFailedCompaction(ctx, step, obe); rerr != nil {
					return nil, rerr // fenced/transport: redeliver
				}
			}
			return nil, &contextError{cause: cerr}
		}
		preamble = comp.Preamble
		reports = comp.Sources
		contextTokens = comp.ContextTokens
		revisions = toRevisionEvents(comp.Revisions)
		summaries = comp.Summaries
		costRows = e.priceSummaryOverheads(ctx, step, summaries, e.now())
	}

	augmented, err := injector.WithContext(sc, preamble)
	if err != nil {
		return nil, &contextError{cause: fmt.Errorf("injecting assembled context: %w", err)}
	}

	// Pre-flight token total over the AUGMENTED (final) request. In the budgeted
	// path the compaction already measured it (comp.FinalTokens == this count);
	// otherwise it is best-effort — a build failure is non-fatal (Execute lands
	// any real config failure), recorded as 0 rather than failing the step.
	preflight := 0
	if pc, pcok := executor.(exec.PreflightCounter); pcok {
		scAug := sc
		scAug.Config = augmented
		if n, perr := pc.PreflightTokens(scAug, counter); perr == nil {
			preflight = n
		} else {
			log.From(ctx).WarnContext(ctx, "context preflight token count unavailable", slog.Any("error", perr))
		}
	}
	if rawPreflight == 0 {
		rawPreflight = preflight
	}

	asmEvent := store.ContextAssembledEvent{
		CounterID:          asm.CounterID,
		Sources:            toSourceRecords(reports),
		ContextTokens:      contextTokens,
		PreflightTokens:    preflight,
		BudgetTokens:       budgetTokens,
		RawContextTokens:   rawContextTokens,
		RawPreflightTokens: rawPreflight,
		Revisions:          len(revisions),
		Summaries:          len(summaries),
	}
	if err := e.recordContextAssembled(ctx, step, asmEvent, revisions, costRows); err != nil {
		// A fenced caller (taken over) or a transport failure of the audit
		// write decides nothing — redeliver, exactly like recordDowngrade.
		return nil, err
	}
	// Meter the summarizer overhead post-commit (ticket 12.5): the rows are
	// durable now, so record the cost metrics off the transaction (the 10.5
	// discipline — a fenced record returned above and metered nothing).
	e.recordCostRows(nil, costRows)

	logger := log.From(ctx)
	included, skipped, truncated, dropped := tallyDispositions(reports)
	logger.InfoContext(ctx, "assembled step context",
		slog.Int("sources", len(reports)),
		slog.Int("included", included),
		slog.Int("skipped", skipped),
		slog.Int("truncated", truncated),
		slog.Int("dropped", dropped),
		slog.Int("revisions", len(revisions)),
		slog.Int("summaries", len(summaries)),
		slog.Int("context_tokens", contextTokens),
		slog.Int("budget_tokens", budgetTokens),
		slog.Int("preflight_tokens", preflight))
	return augmented, nil
}

// recordFailedCompaction records the compaction that ran and ledgers any
// summarizer overhead that billed before the pipeline gave up (ticket 12.5),
// then leaves the caller to fail the step permanently. A summarizer may have
// billed a real provider call before a later strategy could not fit the budget;
// the spend must be metered and the decision auditable even though no assembled
// context is sent. Returns a fenced/transport error to redeliver.
func (e *Engine) recordFailedCompaction(ctx context.Context, step gen.RunStep, obe *contextmgr.OverBudgetError) error {
	revisions := toRevisionEvents(obe.Revisions)
	costRows := e.priceSummaryOverheads(ctx, step, obe.Summaries, e.now())
	if len(revisions) == 0 && len(costRows) == 0 {
		return nil // nothing ran and nothing billed — the plain guardrail path
	}
	if err := e.recordContextRevisions(ctx, step, revisions, costRows); err != nil {
		return err
	}
	e.recordCostRows(nil, costRows)
	return nil
}

// contextSources builds the store-backed readers contextmgr.Assemble needs:
// a batched upstream-output map, the step's already-bound blackboard handle
// (claim.go binds it onto the StepContext), and the retriever registry. A
// batched read failure is transport (returned to redeliver).
func (e *Engine) contextSources(ctx context.Context, step gen.RunStep, spec dag.ContextSpec, sc exec.StepContext) (contextmgr.Sources, error) {
	// Gather the distinct step_output source step ids for one batched read
	// (referenced outputs are immutable once recorded — the render.go read).
	var ids []string
	seen := map[string]bool{}
	for _, s := range spec.Sources {
		if s.Kind == dag.SourceStepOutput && s.Step != "" && !seen[s.Step] {
			seen[s.Step] = true
			ids = append(ids, s.Step)
		}
	}
	outputs := make(map[string]json.RawMessage, len(ids))
	if len(ids) > 0 {
		rows, err := e.store.Steps().ListByIDs(ctx, step.RunID, ids)
		if err != nil {
			return contextmgr.Sources{}, fmt.Errorf("engine: reading context step outputs: %w", err)
		}
		for _, r := range rows {
			// Only a succeeded step has a recorded output; anything else a
			// source names surfaces as a missing source (subject to on_missing).
			if r.Status == store.StepStatusSucceeded {
				outputs[r.StepID] = r.Output
			}
		}
	}
	src := contextmgr.Sources{
		StepOutput: func(_ context.Context, id string) (json.RawMessage, bool, error) {
			out, ok := outputs[id]
			return out, ok, nil
		},
		// sc.Blackboard is the step-scoped board claim.go bound (nil when no
		// board is wired — a blackboard source then fails as a config error).
		Board: sc.Blackboard,
	}
	if e.retrievers != nil {
		src.Retriever = func(name string) (retrieval.Retriever, error) { return e.retrievers.Get(name) }
	}
	return src, nil
}

// recordContextAssembled appends the compaction revision events (ticket 12.4),
// the summarize overhead ledger rows (ticket 12.5), and the context_assembled
// event for a claim whose context spec the assembly built into the request, in
// one short fenced transaction (the recordDowngrade precedent). It is not a
// state transition, so it only fences on the caller's live claim and appends
// the events; a fenced caller surfaces a *store.TransitionError so
// assembleContext abandons like any other fenced write, and a transport error
// redelivers.
func (e *Engine) recordContextAssembled(ctx context.Context, step gen.RunStep, event store.ContextAssembledEvent, revisions []store.ContextRevisionEvent, costRows []store.AttemptCostArgs) error {
	claimID := uuid.Nil
	if step.ClaimID != nil {
		claimID = *step.ClaimID
	}
	now := e.now()
	txCtx, span := e.tracer.Start(ctx, "step.context.record")
	err := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		return store.RecordContextAssembled(ctx, q, store.RecordContextAssembledArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: claimID,
			Event: event, Revisions: revisions, CostRows: costRows, Now: now,
		})
	})
	endTxSpan(span, err)
	return err
}

// recordContextRevisions appends the compaction revision events and ledgers the
// summarize overhead rows for a claim whose compaction pipeline could NOT fit
// the budget (ticket 12.5) — no context_assembled event, since no assembly is
// sent, but a summarizer that billed before the pipeline gave up must still be
// audited and metered. Fenced on the caller's live claim, like
// recordContextAssembled.
func (e *Engine) recordContextRevisions(ctx context.Context, step gen.RunStep, revisions []store.ContextRevisionEvent, costRows []store.AttemptCostArgs) error {
	claimID := uuid.Nil
	if step.ClaimID != nil {
		claimID = *step.ClaimID
	}
	now := e.now()
	txCtx, span := e.tracer.Start(ctx, "step.context.record")
	err := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		return store.RecordContextRevisions(ctx, q, store.RecordContextAssembledArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: claimID,
			Revisions: revisions, CostRows: costRows, Now: now,
		})
	})
	endTxSpan(span, err)
	return err
}

// toSourceRecords maps the contextmgr per-source dispositions onto the store's
// audit record shape.
func toSourceRecords(reports []contextmgr.SourceReport) []store.ContextSourceRecord {
	out := make([]store.ContextSourceRecord, len(reports))
	for i, r := range reports {
		out[i] = store.ContextSourceRecord{
			Index: r.Index, Kind: string(r.Kind), Name: r.Name, Ref: r.Ref,
			Status: string(r.Status), Reason: r.Reason, Tokens: r.Tokens, Pinned: r.Pinned,
		}
	}
	return out
}

// toRevisionEvents maps compaction revisions onto the store's context_revision
// event payloads (tickets 12.4/12.5).
func toRevisionEvents(revs []contextmgr.Revision) []store.ContextRevisionEvent {
	if len(revs) == 0 {
		return nil
	}
	out := make([]store.ContextRevisionEvent, len(revs))
	for i, r := range revs {
		actions := make([]store.ContextRevisionActionRecord, len(r.Actions))
		for j, a := range r.Actions {
			actions[j] = store.ContextRevisionActionRecord{
				SourceIndex: a.SourceIndex, Name: a.Name, Action: a.Action,
				TokensBefore: a.TokensBefore, TokensAfter: a.TokensAfter,
			}
		}
		summaries := make([]store.ContextRevisionSummaryRecord, len(r.Summaries))
		for j, s := range r.Summaries {
			summaries[j] = store.ContextRevisionSummaryRecord{
				Key: s.Key, Version: s.Version, ParentVersion: s.ParentVersion,
				Model: s.Model, Resource: s.Resource, SpanNames: s.SpanNames,
				SpanTokens: s.SpanTokens, SummaryTokens: s.SummaryTokens, CacheHit: s.CacheHit,
				InputTokens: s.InputTokens, OutputTokens: s.OutputTokens,
			}
		}
		out[i] = store.ContextRevisionEvent{
			Index: r.Index, Strategy: r.Strategy, N: r.N, MinTokens: r.MinTokens,
			Budget: r.Budget, TokensBefore: r.TokensBefore, TokensAfter: r.TokensAfter,
			Changed: r.Changed, Actions: actions, Summaries: summaries, Error: r.Error, Kept: r.Kept,
		}
	}
	return out
}

// tallyDispositions counts included/skipped/truncated/dropped sources for the
// log line.
func tallyDispositions(reports []contextmgr.SourceReport) (included, skipped, truncated, dropped int) {
	for _, r := range reports {
		switch r.Status {
		case contextmgr.Included:
			included++
		case contextmgr.Skipped:
			skipped++
		case contextmgr.Truncated:
			truncated++
		case contextmgr.Dropped:
			dropped++
		case contextmgr.Summarized:
			// A summarized source folded into a summary — counted as dropped
			// (its content left the assembly) for this coarse log tally.
			dropped++
		}
	}
	return included, skipped, truncated, dropped
}
