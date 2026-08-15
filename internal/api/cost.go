package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// nanoUSD is the nano-USD in one US dollar (ADR-012; mirrors cost.NanoPerUSD,
// duplicated so the api package need not import internal/cost just to format).
const nanoUSD = int64(1_000_000_000)

// nanoUSDString renders integer nano-USD as a full-precision USD decimal
// string, exactly (no float): "10500000" → "0.0105", "0" → "0". The integer
// on the wire is the source of truth; this is the human-readable rendering.
func nanoUSDString(nano int64) string {
	neg := nano < 0
	if neg {
		nano = -nano
	}
	s := fmt.Sprintf("%d.%09d", nano/nanoUSD, nano%nanoUSD)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if neg {
		s = "-" + s
	}
	return s
}

// costSummary builds the wire cost summary from the materialized run
// aggregates.
func costSummary(spentNano, savedNano int64) CostSummaryView {
	return CostSummaryView{
		SpentNanoUSD: spentNano,
		SavedNanoUSD: savedNano,
		SpentUSD:     nanoUSDString(spentNano),
		SavedUSD:     nanoUSDString(savedNano),
	}
}

// handleRunCost is GET /v1/runs/{id}/cost (ticket 10.2, ADR-012): the run's
// cumulative cost, its per-step and per-resource (per-model / per-tool)
// breakdowns, and the full per-attempt ledger. The summary is the run row's
// materialized aggregate (equal by construction to the exact sum of the
// ledger rows). A run with no cost-bearing attempts returns a zero summary
// and empty breakdowns.
func (h *Handler) handleRunCost(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "run id is not a valid UUID",
		})
		return
	}
	ctx := r.Context()
	run, err := h.st.Runs().Get(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrorDetail{
			Code: ErrCodeRunNotFound, Message: "no run with id " + id.String(),
		})
		return
	case err != nil:
		internalError(w, r, "reading run", err)
		return
	}
	byStep, err := h.st.Ledger().AggregateByStep(ctx, id)
	if err != nil {
		internalError(w, r, "aggregating cost by step", err)
		return
	}
	byResource, err := h.st.Ledger().AggregateByResource(ctx, id)
	if err != nil {
		internalError(w, r, "aggregating cost by resource", err)
		return
	}
	entries, err := h.st.Ledger().ListByRun(ctx, id)
	if err != nil {
		internalError(w, r, "listing cost ledger", err)
		return
	}
	writeJSON(w, http.StatusOK, buildRunCostResponse(run, byStep, byResource, entries))
}

// buildRunCostResponse assembles the cost breakdown wire projection.
func buildRunCostResponse(
	run gen.Run,
	byStep []gen.AggregateCostByStepRow,
	byResource []gen.AggregateCostByResourceRow,
	entries []gen.CostLedger,
) RunCostResponse {
	resp := RunCostResponse{
		RunID:      run.ID.String(),
		Summary:    costSummary(run.SpentNanoUsd, run.SavedNanoUsd),
		ByStep:     make([]CostByStepView, 0, len(byStep)),
		ByResource: make([]CostByResourceView, 0, len(byResource)),
		Entries:    make([]CostEntryView, 0, len(entries)),
	}
	for _, s := range byStep {
		resp.ByStep = append(resp.ByStep, CostByStepView{
			StepID:       s.StepID,
			Entries:      s.Entries,
			SpentNanoUSD: s.SpentNanoUsd,
			SavedNanoUSD: s.SavedNanoUsd,
			SpentUSD:     nanoUSDString(s.SpentNanoUsd),
			SavedUSD:     nanoUSDString(s.SavedNanoUsd),
		})
	}
	for _, m := range byResource {
		resp.ByResource = append(resp.ByResource, CostByResourceView{
			Resource:     m.Resource,
			Entries:      m.Entries,
			InputTokens:  m.InputTokens,
			OutputTokens: m.OutputTokens,
			SpentNanoUSD: m.SpentNanoUsd,
			SavedNanoUSD: m.SavedNanoUsd,
			SpentUSD:     nanoUSDString(m.SpentNanoUsd),
			SavedUSD:     nanoUSDString(m.SavedNanoUsd),
		})
	}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, CostEntryView{
			StepID:       e.StepID,
			Attempt:      int(e.Attempt),
			Entry:        e.Entry,
			Resource:     e.Resource,
			Usage:        e.Usage,
			Rate:         e.Rate,
			RateSource:   e.RateSource,
			CacheHit:     e.CacheHit,
			Overhead:     e.Overhead,
			SpentNanoUSD: e.CostNanoUsd,
			SavedNanoUSD: e.SavedNanoUsd,
			CreatedAt:    e.CreatedAt,
		})
	}
	return resp
}
