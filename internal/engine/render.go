package engine

// This file is the ticket 8.2 seam: runtime template rendering. Before an
// executor runs, the engine renders the claimed step's config — resolving
// `${{ ... }}` references against upstream step outputs and run params.
// The dag package owns the template semantics; this file owns the data
// fetch and the failure routing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// renderError marks a deterministic rendering failure — a template that
// cannot parse, a strict reference that did not resolve, a referenced
// step without a usable output. The claimed step's inputs are immutable
// (upstream outputs and params never change within a run), so retrying
// cannot help: the engine records the failure with ClassPermanent
// (ADR-006). Transport failures during the pre-render reads are returned
// bare instead — nothing was decided, the delivery redelivers.
type renderError struct {
	cause error
}

func (e *renderError) Error() string { return "rendering step config: " + e.cause.Error() }
func (e *renderError) Unwrap() error { return e.cause }

// renderConfig renders the claimed step's config templates. Configs
// without template expressions pass through untouched with zero reads —
// the common fast path. Otherwise the run's params and every referenced
// step's output are fetched (plain reads: referenced outputs are
// immutable once recorded), and the rendered config is returned. A
// *renderError is deterministic (route to a permanent failure
// completion); any other error is transport (redeliver).
func (e *Engine) renderConfig(ctx context.Context, step gen.RunStep) (json.RawMessage, error) {
	if len(step.Config) == 0 || !dag.HasTemplate(string(step.Config)) {
		return step.Config, nil
	}
	ctx, span := e.tracer.Start(ctx, "step.render")
	defer span.End()

	ct, err := dag.ParseConfigTemplates(step.Config)
	if err != nil {
		// The definition passed the submit-time lint, so an unparseable
		// stored config means corrupt state or version skew — deterministic
		// either way (same stance as a corrupt materialized timeout).
		return nil, &renderError{cause: err}
	}
	data := dag.RenderData{}
	run, err := e.store.Runs().Get(ctx, step.RunID)
	if err != nil {
		return nil, fmt.Errorf("engine: reading run for template params: %w", err)
	}
	data.Params = run.Params

	refIDs := ct.StepIDs()
	statuses := make(map[string]string, len(refIDs))
	if len(refIDs) > 0 {
		rows, err := e.store.Steps().ListByIDs(ctx, step.RunID, refIDs)
		if err != nil {
			return nil, fmt.Errorf("engine: reading referenced step outputs: %w", err)
		}
		data.Outputs = make(map[string]json.RawMessage, len(rows))
		for _, r := range rows {
			statuses[r.StepID] = r.Status
			// Only a succeeded step has a recorded output. Anything else a
			// strict reference names — a skipped branch arm, a join-any
			// loser still running — surfaces as a missing reference below;
			// lenient `get` references resolve to nil, as authored.
			if r.Status == store.StepStatusSucceeded {
				data.Outputs[r.StepID] = r.Output
			}
		}
	}

	rendered, rerr := ct.Render(data)
	if rerr != nil {
		var mre *dag.MissingRefError
		if errors.As(rerr, &mre) {
			// Attach the referenced steps' statuses: "steps.a.output missing"
			// reads very differently when step a was skipped vs misspelled.
			rerr = fmt.Errorf("%w (referenced step statuses: %s)", rerr, formatStatuses(refIDs, statuses))
		}
		return nil, &renderError{cause: rerr}
	}
	log.From(ctx).DebugContext(ctx, "step config rendered",
		slog.Int("template_refs", len(ct.Refs())),
		slog.Int("referenced_steps", len(refIDs)))
	return rendered, nil
}

// formatStatuses renders the referenced steps' statuses for the
// missing-reference diagnostic, in stable order.
func formatStatuses(refIDs []string, statuses map[string]string) string {
	if len(refIDs) == 0 {
		return "none referenced"
	}
	parts := make([]string, 0, len(refIDs))
	for _, id := range refIDs {
		st, ok := statuses[id]
		if !ok {
			st = "not in run"
		}
		parts = append(parts, id+"="+st)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
