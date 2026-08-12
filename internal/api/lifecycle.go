package api

// Run-lifecycle endpoints (ticket 6.5): cancel, park, unpark, and
// dead-lettered-step requeue, each a thin HTTP shell over the engine's
// Control ops (tickets 5.4/5.6 — the ops own the semantics; ADR-006 the
// state machines). Wrong-state refusals surface as 409 with the conflict
// envelope code; the ops' outbox rows are drained by the worker fleet's
// dispatchers (the API holds no queue client, ADR-002).

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/store"
)

// handleCancelRun is POST /v1/runs/{id}/cancel: request the cooperative
// cancel with reason manual (deadline_exceeded is the reconciler's). The
// response reports what settled in the request transaction; in-flight
// steps converge afterwards (workers' completion checks, the reconciler).
func (h *Handler) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := h.runIDParam(w, r)
	if !ok {
		return
	}
	res, err := h.ctl.Cancel(r.Context(), id, store.RunCancelReasonManual)
	if err != nil {
		h.writeRunOpError(w, r, "cancelling run", id, err)
		return
	}
	writeJSON(w, http.StatusOK, CancelRunResponse{
		Run:            buildRunView(res.Run),
		CancelledSteps: orEmpty(res.Cancelled),
		Finalized:      res.Finalized,
	})
}

// handleParkRun is POST /v1/runs/{id}/park: pause dispatch with reason
// manual (budget_exceeded / awaiting_human are reserved for M10/M15's
// engine-internal parks).
func (h *Handler) handleParkRun(w http.ResponseWriter, r *http.Request) {
	id, ok := h.runIDParam(w, r)
	if !ok {
		return
	}
	run, err := h.ctl.Park(r.Context(), id, store.ParkReasonManual)
	if err != nil {
		h.writeRunOpError(w, r, "parking run", id, err)
		return
	}
	writeJSON(w, http.StatusOK, ParkRunResponse{Run: buildRunView(run)})
}

// handleUnparkRun is POST /v1/runs/{id}/unpark: resume dispatch,
// re-outboxing every ready step whose delivery was consumed while parked.
func (h *Handler) handleUnparkRun(w http.ResponseWriter, r *http.Request) {
	id, ok := h.runIDParam(w, r)
	if !ok {
		return
	}
	res, err := h.ctl.Unpark(r.Context(), id)
	if err != nil {
		h.writeRunOpError(w, r, "unparking run", id, err)
		return
	}
	writeJSON(w, http.StatusOK, UnparkRunResponse{
		Run:        buildRunView(res.Run),
		Dispatched: orEmpty(res.Dispatched),
	})
}

// handleRequeueStep is POST /v1/runs/{id}/steps/{sid}/requeue: reset a
// dead-lettered step to ready with its retry budget re-armed, re-opening a
// failed run and reviving written-off descendants (ticket 5.4).
func (h *Handler) handleRequeueStep(w http.ResponseWriter, r *http.Request) {
	id, ok := h.runIDParam(w, r)
	if !ok {
		return
	}
	stepID := chi.URLParam(r, "stepID")
	res, err := h.ctl.Requeue(r.Context(), id, stepID)
	if errors.Is(err, store.ErrNotFound) {
		// The op reports both a missing run and a missing step as not
		// found; one read tells the client which.
		if _, runErr := h.st.Runs().Get(r.Context(), id); errors.Is(runErr, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrorDetail{
				Code: ErrCodeRunNotFound, Message: "no run with id " + id.String(),
			})
		} else {
			writeError(w, http.StatusNotFound, ErrorDetail{
				Code: ErrCodeStepNotFound, Message: "run has no step " + stepID,
			})
		}
		return
	}
	if err != nil {
		h.writeRunOpError(w, r, "requeueing step", id, err)
		return
	}
	writeJSON(w, http.StatusOK, RequeueStepResponse{
		RunID:      id.String(),
		StepID:     res.Step.StepID,
		Status:     res.Step.Status,
		RunResumed: res.RunResumed,
		Revived:    res.Revived,
		Dispatched: orEmpty(res.Dispatched),
	})
}

// runIDParam parses the {runID} path parameter, answering the 400 itself.
func (h *Handler) runIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "run id is not a valid UUID",
		})
		return uuid.Nil, false
	}
	return id, true
}

// writeRunOpError maps a lifecycle op's typed errors onto the wire: a
// wrong-state refusal is a 409 naming the actual status, a missing run a
// 404, anything else the logged 500.
func (h *Handler) writeRunOpError(w http.ResponseWriter, r *http.Request, what string, runID uuid.UUID, err error) {
	var te *store.TransitionError
	switch {
	case errors.Is(err, engine.ErrRunNotRequeueable):
		writeError(w, http.StatusConflict, ErrorDetail{
			Code:    ErrCodeConflict,
			Message: "run is cancelled — dead-lettered steps of a cancelled run cannot be requeued",
		})
	case errors.As(err, &te):
		// The transition error's own message names the entity, the actual
		// status, and the refused target — exactly what the client needs.
		writeError(w, http.StatusConflict, ErrorDetail{Code: ErrCodeConflict, Message: te.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrorDetail{
			Code: ErrCodeRunNotFound, Message: "no run with id " + runID.String(),
		})
	default:
		internalError(w, r, what, err)
	}
}

// orEmpty keeps empty lists as [] on the wire instead of null.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
