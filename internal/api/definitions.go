package api

// Definition-registry endpoints (ticket 6.5): create, new-version, list
// (latest per name, keyset by name), get by id, and per-name version
// listing. Rows are immutable — a new version is a new row (ADR-004); the
// stored spec is the canonical encoding, so submit-by-ref always decodes.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// handleCreateDefinition is POST /v1/definitions: register a validated
// definition under its spec name at version 1. An existing name is a 409 —
// appending versions is the /versions route's.
func (h *Handler) handleCreateDefinition(w http.ResponseWriter, r *http.Request) {
	def, ok := h.decodeDefinitionBody(w, r)
	if !ok {
		return
	}
	row, err := h.st.CreateDefinition(r.Context(), def)
	var conflict *store.ConflictError
	switch {
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, ErrorDetail{
			Code:    ErrCodeConflict,
			Message: "definition " + def.Name + " already exists; POST /v1/definitions/" + def.Name + "/versions appends a version",
		})
		return
	case err != nil:
		internalError(w, r, "creating definition", err)
		return
	}
	log.From(r.Context()).InfoContext(r.Context(), "definition created",
		slog.String("definition_id", row.ID.String()),
		slog.String("name", row.Name),
		slog.Int("version", int(row.Version)))
	writeJSON(w, http.StatusCreated, definitionResponse(row))
}

// IfMatchHeader carries the optimistic-concurrency precondition on a
// definition-version append (ticket 17.6): the version number the client
// opened the definition at. A plain integer (the version), so the builder need
// not track opaque ETags.
const IfMatchHeader = "If-Match"

// parseIfMatchVersion reads the optional If-Match precondition. An absent
// header yields (nil, true) — no precondition. A present but non-integer or
// non-positive value is a 400 (returns false after writing the error).
func parseIfMatchVersion(w http.ResponseWriter, raw string) (*int32, bool) {
	if raw == "" {
		return nil, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > (1<<31-1) {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidRequest,
			Message: IfMatchHeader + " must be a positive integer version",
		})
		return nil, false
	}
	v := int32(n) //nolint:gosec // bounded to [1, 2^31-1] above
	return &v, true
}

// handleCreateDefinitionVersion is POST /v1/definitions/{name}/versions:
// append the next version of an existing name. The body's spec name must
// match the path — a mismatch is a client error, not a rename.
func (h *Handler) handleCreateDefinitionVersion(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	def, ok := h.decodeDefinitionBody(w, r)
	if !ok {
		return
	}
	if def.Name != name {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidRequest,
			Message: "definition name " + def.Name + " does not match the path name " + name,
		})
		return
	}
	// Optional optimistic-concurrency precondition (ticket 17.6): the builder
	// sends the version it opened as `If-Match`, so a save racing another
	// append is refused (409 version_conflict) instead of silently forking.
	expected, ok := parseIfMatchVersion(w, r.Header.Get(IfMatchHeader))
	if !ok {
		return
	}
	row, err := h.st.CreateDefinitionVersion(r.Context(), def, expected)
	var conflict *store.ConflictError
	var versionConflict *store.VersionConflictError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrorDetail{
			Code:    ErrCodeDefinitionNotFound,
			Message: "no definition named " + name + "; POST /v1/definitions creates it",
		})
		return
	case errors.As(err, &versionConflict):
		writeError(w, http.StatusConflict, ErrorDetail{
			Code: ErrCodeVersionConflict,
			Message: fmt.Sprintf("stale save: %s was at version %d when opened, but its latest is now %d",
				name, versionConflict.Expected, versionConflict.Latest),
		})
		return
	case errors.As(err, &conflict):
		// The advisory lock serializes appenders, so this is a residual
		// oddity (e.g. racing a concurrent create) — retryable either way.
		writeError(w, http.StatusConflict, ErrorDetail{
			Code:    ErrCodeConflict,
			Message: "version allocation for " + name + " hit a conflict; retry",
		})
		return
	case err != nil:
		internalError(w, r, "creating definition version", err)
		return
	}
	log.From(r.Context()).InfoContext(r.Context(), "definition version created",
		slog.String("definition_id", row.ID.String()),
		slog.String("name", row.Name),
		slog.Int("version", int(row.Version)))
	writeJSON(w, http.StatusCreated, definitionResponse(row))
}

// handleListDefinitions is GET /v1/definitions: one keyset page of each
// name's newest version, in name order.
func (h *Handler) handleListDefinitions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := pageLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	var cursorName *string
	if raw := q.Get("cursor"); raw != "" {
		name, err := decodeNameCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "cursor is not a valid definition-list cursor",
			})
			return
		}
		cursorName = &name
	}
	rows, err := h.st.Definitions().ListLatest(r.Context(), cursorName, limit+1)
	if err != nil {
		internalError(w, r, "listing definitions", err)
		return
	}
	resp := ListDefinitionsResponse{Definitions: make([]DefinitionView, 0, min(len(rows), int(limit)))}
	if len(rows) > int(limit) {
		rows = rows[:limit]
		resp.NextCursor = encodeNameCursor(rows[len(rows)-1].Name)
	}
	for _, row := range rows {
		resp.Definitions = append(resp.Definitions, definitionView(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetDefinition is GET /v1/definitions/{id}: one registry row with
// its stored canonical spec.
func (h *Handler) handleGetDefinition(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "defID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "definition id is not a valid UUID",
		})
		return
	}
	row, err := h.st.Definitions().Get(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrorDetail{
			Code: ErrCodeDefinitionNotFound, Message: "no stored definition with id " + id.String(),
		})
		return
	case err != nil:
		internalError(w, r, "reading definition", err)
		return
	}
	writeJSON(w, http.StatusOK, definitionResponse(row))
}

// handleListDefinitionVersions is GET /v1/definitions/{name}/versions:
// every version of one name, oldest first. An unknown name is a 404 — an
// empty list would be indistinguishable from a typo.
func (h *Handler) handleListDefinitionVersions(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	rows, err := h.st.Definitions().ListVersions(r.Context(), name)
	if err != nil {
		internalError(w, r, "listing definition versions", err)
		return
	}
	if len(rows) == 0 {
		writeError(w, http.StatusNotFound, ErrorDetail{
			Code: ErrCodeDefinitionNotFound, Message: "no definition named " + name,
		})
		return
	}
	resp := DefinitionVersionsResponse{Versions: make([]DefinitionView, 0, len(rows))}
	for _, row := range rows {
		resp.Versions = append(resp.Versions, definitionView(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeDefinitionBody decodes the create/new-version body and runs the
// definition through decode + M1 validation, answering the 400 itself.
func (h *Handler) decodeDefinitionBody(w http.ResponseWriter, r *http.Request) (*dag.Definition, bool) {
	var req CreateDefinitionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "decoding request body: " + err.Error(),
		})
		return nil, false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "request body holds more than one JSON document",
		})
		return nil, false
	}
	if len(req.Definition) == 0 {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "definition is required",
		})
		return nil, false
	}
	return h.decodeAndValidate(w, req.Definition)
}

func definitionView(row gen.WorkflowDefinition) DefinitionView {
	return DefinitionView{
		ID:        row.ID.String(),
		Name:      row.Name,
		Version:   int(row.Version),
		CreatedAt: row.CreatedAt,
	}
}

func definitionResponse(row gen.WorkflowDefinition) DefinitionResponse {
	return DefinitionResponse{DefinitionView: definitionView(row), Spec: row.Spec}
}

// Name-cursor helpers: the definition list's cursor is the previous page's
// last name, base64url-wrapped so the wire form is opaque like the runs
// cursor.
func encodeNameCursor(name string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(name))
}

func decodeNameCursor(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", errors.New("empty cursor")
	}
	return string(b), nil
}
