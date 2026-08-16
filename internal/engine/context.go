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

	augmented, err := injector.WithContext(sc, asm.Preamble)
	if err != nil {
		return nil, &contextError{cause: fmt.Errorf("injecting assembled context: %w", err)}
	}

	// Pre-flight token total over the AUGMENTED request (context already
	// prepended), for the audit event and the 12.6 window guardrail. A build
	// failure is non-fatal here — Execute lands any real config failure — so
	// the count is recorded as unavailable (0) rather than failing the step.
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

	if err := e.recordContextAssembled(ctx, step, asm, preflight); err != nil {
		// A fenced caller (taken over) or a transport failure of the audit
		// write decides nothing — redeliver, exactly like recordDowngrade.
		return nil, err
	}

	logger := log.From(ctx)
	included, skipped, truncated := tallyDispositions(asm.Sources)
	logger.InfoContext(ctx, "assembled step context",
		slog.Int("sources", len(asm.Sources)),
		slog.Int("included", included),
		slog.Int("skipped", skipped),
		slog.Int("truncated", truncated),
		slog.Int("context_tokens", asm.ContextTokens),
		slog.Int("preflight_tokens", preflight))
	return augmented, nil
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

// recordContextAssembled appends the context_assembled event for a claim whose
// context spec the assembly built into the request, in its own short fenced
// transaction (the recordDowngrade precedent). It is not a state transition,
// so it only fences on the caller's live claim and appends the event; a fenced
// caller surfaces a *store.TransitionError so assembleContext abandons like any
// other fenced write, and a transport error redelivers.
func (e *Engine) recordContextAssembled(ctx context.Context, step gen.RunStep, asm contextmgr.Assembly, preflight int) error {
	claimID := uuid.Nil
	if step.ClaimID != nil {
		claimID = *step.ClaimID
	}
	sources := make([]store.ContextSourceRecord, len(asm.Sources))
	for i, r := range asm.Sources {
		sources[i] = store.ContextSourceRecord{
			Index: r.Index, Kind: string(r.Kind), Name: r.Name, Ref: r.Ref,
			Status: string(r.Status), Reason: r.Reason, Tokens: r.Tokens, Pinned: r.Pinned,
		}
	}
	event := store.ContextAssembledEvent{
		CounterID:       asm.CounterID,
		Sources:         sources,
		ContextTokens:   asm.ContextTokens,
		PreflightTokens: preflight,
	}
	now := e.now()
	txCtx, span := e.tracer.Start(ctx, "step.context.record")
	err := e.store.WithTx(txCtx, func(ctx context.Context, q store.Querier) error {
		return store.RecordContextAssembled(ctx, q, store.RecordContextAssembledArgs{
			RunID: step.RunID, StepID: step.StepID, ClaimID: claimID, Event: event, Now: now,
		})
	})
	endTxSpan(span, err)
	return err
}

// tallyDispositions counts included/skipped/truncated sources for the log line.
func tallyDispositions(reports []contextmgr.SourceReport) (included, skipped, truncated int) {
	for _, r := range reports {
		switch r.Status {
		case contextmgr.Included:
			included++
		case contextmgr.Skipped:
			skipped++
		case contextmgr.Truncated:
			truncated++
		}
	}
	return included, skipped, truncated
}
