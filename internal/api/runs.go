package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// handleSubmitRun is POST /v1/runs: decode + validate the definition
// (inline or by stored ref), then hand it to store.CreateRun — one
// transaction writing the run, its graph copy, entry-ready steps, their
// outbox rows, and the creation events. Dispatch happens when a worker's
// drain loop picks the outbox rows up.
func (h *Handler) handleSubmitRun(w http.ResponseWriter, r *http.Request) {
	var req SubmitRunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "decoding request body: " + err.Error(),
		})
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "request body holds more than one JSON document",
		})
		return
	}

	var (
		def   *dag.Definition
		defID *uuid.UUID
	)
	switch {
	case len(req.Definition) > 0 && req.DefinitionID != "":
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "definition and definition_id are mutually exclusive",
		})
		return
	case len(req.Definition) > 0:
		var ok bool
		def, ok = h.decodeAndValidate(w, req.Definition)
		if !ok {
			return
		}
	case req.DefinitionID != "":
		id, err := uuid.Parse(req.DefinitionID)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "definition_id is not a valid UUID",
			})
			return
		}
		row, err := h.st.Definitions().Get(r.Context(), id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeDefinitionNotFound, Message: "no stored definition with id " + req.DefinitionID,
			})
			return
		case err != nil:
			internalError(w, r, "reading stored definition", err)
			return
		}
		// A stored spec was validated when it was stored; a decode failure
		// here means corruption or version skew, which is the server's
		// problem, not the client's.
		def, err = dag.Decode(row.Spec)
		if err != nil {
			internalError(w, r, "decoding stored definition", err)
			return
		}
		defID = &id
	default:
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "one of definition or definition_id is required",
		})
		return
	}

	res, err := h.st.CreateRun(r.Context(), store.CreateRunArgs{
		Definition:       def,
		DefinitionID:     defID,
		Params:           req.Params,
		IdempotencyToken: req.IdempotencyToken,
		Now:              h.now(),
	})
	if err != nil {
		internalError(w, r, "creating run", err)
		return
	}
	log.From(r.Context()).InfoContext(r.Context(), "run submitted",
		log.RunID(res.Run.ID.String()),
		slog.String("name", def.Name),
		slog.Bool("reused", res.Reused))

	status := http.StatusCreated
	if res.Reused {
		status = http.StatusOK
	}
	writeJSON(w, status, SubmitRunResponse{
		RunID:      res.Run.ID.String(),
		Status:     res.Run.Status,
		EntrySteps: res.EntrySteps,
		Reused:     res.Reused,
	})
}

// decodeAndValidate turns an inline definition document into a decoded,
// validated *dag.Definition, answering the 400 (with every path-qualified
// issue) itself when the document is bad. The bool reports success.
func (h *Handler) decodeAndValidate(w http.ResponseWriter, raw json.RawMessage) (*dag.Definition, bool) {
	def, err := dag.Decode(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidDefinition,
			Message: "definition failed to decode",
			Issues:  DefinitionIssues(err),
		})
		return nil, false
	}
	issues, err := dag.Validate(def)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidDefinition,
			Message: "definition failed validation",
			Issues:  ValidationIssues(issues), // warnings included — the client sees the full report
		})
		return nil, false
	}
	return def, true
}

// handleGetRun is GET /v1/runs/{id}: the run row, every step with its
// attempt history, and every edge with its resolution. Three pool reads,
// deliberately not one transaction: a run mid-flight is allowed to show a
// step newer than its rollup counters; the watch loop's next poll heals it.
func (h *Handler) handleGetRun(w http.ResponseWriter, r *http.Request) {
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
	steps, err := h.st.Steps().ListByRun(ctx, id)
	if err != nil {
		internalError(w, r, "listing run steps", err)
		return
	}
	edges, err := h.st.Steps().ListEdgesByRun(ctx, id)
	if err != nil {
		internalError(w, r, "listing run edges", err)
		return
	}
	attempts, err := h.st.Attempts().ListByRun(ctx, id)
	if err != nil {
		internalError(w, r, "listing run attempts", err)
		return
	}
	writeJSON(w, http.StatusOK, buildRunResponse(run, steps, edges, attempts))
}

// buildRunResponse assembles the wire projection from the store rows.
func buildRunResponse(run gen.Run, steps []gen.RunStep, edges []gen.RunEdge, attempts []gen.StepAttempt) RunResponse {
	byStep := make(map[string][]AttemptView, len(steps))
	for _, a := range attempts {
		v := AttemptView{
			Attempt:    int(a.AttemptNo),
			ClaimID:    a.ClaimID.String(),
			Error:      a.Error,
			StartedAt:  a.StartedAt,
			FinishedAt: a.FinishedAt,
		}
		if a.Outcome != nil {
			v.Outcome = *a.Outcome
		}
		byStep[a.StepID] = append(byStep[a.StepID], v)
	}

	resp := RunResponse{
		Run: RunView{
			ID:             run.ID.String(),
			Status:         run.Status,
			StepsTotal:     int(run.StepsTotal),
			StepsSucceeded: int(run.StepsSucceeded),
			StepsFailed:    int(run.StepsFailed),
			StepsSkipped:   int(run.StepsSkipped),
			CreatedAt:      run.CreatedAt,
			StartedAt:      run.StartedAt,
			FinishedAt:     run.FinishedAt,
		},
		Steps: make([]StepView, 0, len(steps)),
		Edges: make([]EdgeView, 0, len(edges)),
	}
	for _, s := range steps {
		resp.Steps = append(resp.Steps, StepView{
			ID:            s.StepID,
			Type:          s.StepType,
			Status:        s.Status,
			RemainingDeps: int(s.RemainingDeps),
			FiredDeps:     int(s.FiredDeps),
			AttemptCount:  int(s.AttemptCount),
			Output:        s.Output,
			Error:         s.Error,
			StartedAt:     s.StartedAt,
			FinishedAt:    s.FinishedAt,
			Attempts:      byStep[s.StepID],
		})
	}
	for _, e := range edges {
		resp.Edges = append(resp.Edges, EdgeView{
			From:       e.FromStep,
			To:         e.ToStep,
			Type:       e.EdgeType,
			Resolution: e.Resolution,
		})
	}
	return resp
}
