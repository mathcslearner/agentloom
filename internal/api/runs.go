package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	obstrace "github.com/mathcslearner/agentloom/internal/obs/trace"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/validate"
)

// IdempotencyKeyHeader carries the submission idempotency token (ticket
// 6.5): same key + same payload replays the original run, same key +
// different payload is a 409.
const IdempotencyKeyHeader = "Idempotency-Key"

// Run-list pagination bounds.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
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

	token := r.Header.Get(IdempotencyKeyHeader)
	if len(token) > store.MaxIdempotencyTokenLength {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidRequest,
			Message: fmt.Sprintf("%s exceeds %d bytes", IdempotencyKeyHeader, store.MaxIdempotencyTokenLength),
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

	// The run's root trace context (ticket 7.3, ADR-008): the otelhttp
	// server span wrapping this request. Persisted on the run row and the
	// entry-step outbox rows; empty (NULL) when tracing is off.
	traceParent, traceState := obstrace.Inject(r.Context())
	res, err := h.st.CreateRun(r.Context(), store.CreateRunArgs{
		Definition:       def,
		DefinitionID:     defID,
		Params:           req.Params,
		IdempotencyToken: token,
		Now:              h.now(),
		Trace:            store.TraceContext{Parent: traceParent, State: traceState},
	})
	if err != nil {
		var mismatch *store.IdempotencyMismatchError
		if errors.As(err, &mismatch) {
			writeError(w, http.StatusConflict, ErrorDetail{
				Code: ErrCodeIdempotencyMismatch,
				Message: fmt.Sprintf("%s was already used by run %s with a different payload",
					IdempotencyKeyHeader, mismatch.RunID),
			})
			return
		}
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
	resp, err := h.loadRunResponse(ctx, id)
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
	writeJSON(w, http.StatusOK, resp)
}

// loadRunResponse reads a run's full client-facing projection — the same body
// GET /v1/runs/{id} returns. Three-plus pool reads, deliberately not one
// transaction: a run mid-flight may show a step newer than its rollup counters;
// the next poll (or the next event on a WS stream) heals it. Returns
// store.ErrNotFound when the run does not exist, so callers map it to their own
// 404 shape (the REST handler, and the 16.3 WS snapshot). Any other error is a
// server fault the caller reports as 500 / a stream error.
func (h *Handler) loadRunResponse(ctx context.Context, id uuid.UUID) (RunResponse, error) {
	run, err := h.st.Runs().Get(ctx, id)
	if err != nil {
		return RunResponse{}, err
	}
	steps, err := h.st.Steps().ListByRun(ctx, id)
	if err != nil {
		return RunResponse{}, err
	}
	edges, err := h.st.Steps().ListEdgesByRun(ctx, id)
	if err != nil {
		return RunResponse{}, err
	}
	attempts, err := h.st.Attempts().ListByRun(ctx, id)
	if err != nil {
		return RunResponse{}, err
	}
	deadLetters, err := h.st.DeadLetters().ListByRun(ctx, id)
	if err != nil {
		return RunResponse{}, err
	}
	approvals, err := h.st.Approvals().ListByRun(ctx, id)
	if err != nil {
		return RunResponse{}, err
	}
	return buildRunResponse(run, steps, edges, attempts, deadLetters, approvals), nil
}

// handleListRuns is GET /v1/runs (ticket 6.5): one keyset page, newest
// first — order (created_at DESC, id DESC) — with optional status,
// definition_id, and created_after/created_before (RFC 3339) filters. The
// cursor is opaque; a page's next_cursor feeds the next request verbatim.
// Keyset pagination is stable under concurrent inserts: new runs sort
// before any already-returned cursor position, so no row is skipped or
// repeated within a walk.
func (h *Handler) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	params := gen.ListRunsPageParams{}

	if status := q.Get("status"); status != "" {
		if !validRunStatus(status) {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "status is not a run status: " + status,
			})
			return
		}
		params.Status = &status
	}
	if raw := q.Get("definition_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "definition_id is not a valid UUID",
			})
			return
		}
		params.DefinitionID = &id
	}
	for _, f := range []struct {
		name string
		dst  **time.Time
	}{
		{"created_after", &params.CreatedAfter},
		{"created_before", &params.CreatedBefore},
	} {
		raw := q.Get(f.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: f.name + " is not an RFC 3339 timestamp",
			})
			return
		}
		*f.dst = &t
	}
	limit, ok := pageLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	if raw := q.Get("cursor"); raw != "" {
		cur, err := decodeRunsCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "cursor is not a valid run-list cursor",
			})
			return
		}
		params.CursorCreatedAt = &cur.T
		params.CursorID = &cur.ID
	}

	// One extra row decides whether a next page exists.
	params.RowLimit = limit + 1
	rows, err := h.st.Runs().ListPage(r.Context(), params)
	if err != nil {
		internalError(w, r, "listing runs", err)
		return
	}
	resp := ListRunsResponse{Runs: make([]RunView, 0, min(len(rows), int(limit)))}
	if len(rows) > int(limit) {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		resp.NextCursor = encodeRunsCursor(runsCursor{T: last.CreatedAt, ID: last.ID})
	}
	for _, run := range rows {
		resp.Runs = append(resp.Runs, buildRunView(run))
	}
	writeJSON(w, http.StatusOK, resp)
}

// pageLimit parses the ?limit= parameter, answering the 400 itself on a
// bad value. Absent means defaultPageLimit; the cap is maxPageLimit.
func pageLimit(w http.ResponseWriter, raw string) (int32, bool) {
	if raw == "" {
		return defaultPageLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxPageLimit {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidRequest,
			Message: fmt.Sprintf("limit must be an integer in [1, %d]", maxPageLimit),
		})
		return 0, false
	}
	return int32(n), true //nolint:gosec // bounded by maxPageLimit
}

// validRunStatus reports whether s is in the run-status vocabulary.
func validRunStatus(s string) bool {
	switch s {
	case store.RunStatusRunning, store.RunStatusSucceeded, store.RunStatusFailed,
		store.RunStatusParked, store.RunStatusCancelling, store.RunStatusCancelled:
		return true
	}
	return false
}

// runsCursor is the decoded run-list cursor: the previous page's last row
// in list order. Wire form is base64url(JSON) — opaque to clients.
type runsCursor struct {
	T  time.Time `json:"t"`
	ID uuid.UUID `json:"id"`
}

func encodeRunsCursor(c runsCursor) string {
	b, _ := json.Marshal(c) //nolint:errcheck // plain struct
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeRunsCursor(s string) (runsCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return runsCursor{}, err
	}
	var c runsCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return runsCursor{}, err
	}
	if c.T.IsZero() || c.ID == uuid.Nil {
		return runsCursor{}, errors.New("cursor missing fields")
	}
	return c, nil
}

// failureCounts tallies a step's attempts into the two disjoint failure
// budgets (ADR-006 transport vs. ADR-013 semantic): transient/timeout are
// transport failures, validation_failed a semantic one. Administrative
// outcomes (lost/throttled/budget_exceeded) count toward neither.
func failureCounts(attempts []AttemptView) (transport, validation int) {
	for _, a := range attempts {
		switch a.Outcome {
		case store.AttemptOutcomeTransient, store.AttemptOutcomeTimeout:
			transport++
		case store.AttemptOutcomeValidationFailed:
			validation++
		}
	}
	return transport, validation
}

// summarizeVerdicts rolls a step's attempt verdicts into a compact
// per-step validation summary (ticket 11.6, ADR-013). It is a pure read-time
// projection of attempts[].verdict — no stored state — returning nil when no
// attempt carried a verdict (an unvalidated step). Attempts are taken in
// ascending attempt order, so the highest-numbered verdict is "last"; a
// verdict that fails to decode is skipped (a corrupt row never 500s the status
// API), and the per-validator roll-up follows the latest decodable verdict's
// chain order so a client sees the current chain shape.
func summarizeVerdicts(attempts []AttemptView) *ValidationSummaryView {
	var summary ValidationSummaryView
	// Per-validator counts keyed by name, plus the latest verdict's results
	// (for last-status/score and stable ordering).
	counts := make(map[string]*ValidatorSummaryView)
	var lastResults []validate.ValidatorResult
	for _, a := range attempts {
		if len(a.Verdict) == 0 {
			continue
		}
		var v validate.Verdict
		if err := json.Unmarshal(a.Verdict, &v); err != nil {
			continue
		}
		summary.Attempts++
		if v.Status == validate.StatusPass {
			summary.Passed++
		} else {
			summary.Failed++
		}
		// Attempts arrive ascending, so each decodable verdict overwrites the
		// "last" fields — the final one wins.
		summary.LastAttempt = a.Attempt
		summary.LastStatus = string(v.Status)
		summary.LastScore = v.Score
		summary.LastIssueCount = len(v.Issues)
		for _, r := range v.Results {
			c := counts[r.Name]
			if c == nil {
				c = &ValidatorSummaryView{Name: r.Name}
				counts[r.Name] = c
			}
			switch r.Status {
			case validate.StatusPass:
				c.Passed++
			case validate.StatusFail:
				c.Failed++
			case validate.StatusSkipped:
				c.Skipped++
			case validate.StatusError:
				c.Errored++
			}
		}
		lastResults = v.Results
	}
	if summary.Attempts == 0 {
		return nil
	}
	// Order the per-validator roll-up by the latest verdict's chain order; any
	// validator seen only in earlier verdicts (chain changed across attempts)
	// is appended in first-seen order after those.
	seen := make(map[string]bool, len(counts))
	for _, r := range lastResults {
		c := counts[r.Name]
		if c == nil || seen[r.Name] {
			continue
		}
		c.LastStatus = string(r.Status)
		c.LastScore = r.Score
		summary.Validators = append(summary.Validators, *c)
		seen[r.Name] = true
	}
	for _, a := range attempts {
		if len(a.Verdict) == 0 {
			continue
		}
		var v validate.Verdict
		if err := json.Unmarshal(a.Verdict, &v); err != nil {
			continue
		}
		for _, r := range v.Results {
			if seen[r.Name] {
				continue
			}
			c := counts[r.Name]
			summary.Validators = append(summary.Validators, *c)
			seen[r.Name] = true
		}
	}
	return &summary
}

// buildRunView projects one run row onto the wire.
func buildRunView(run gen.Run) RunView {
	v := RunView{
		ID:             run.ID.String(),
		Status:         run.Status,
		OnFailure:      run.OnFailure,
		StepsTotal:     int(run.StepsTotal),
		StepsSucceeded: int(run.StepsSucceeded),
		StepsFailed:    int(run.StepsFailed),
		StepsSkipped:   int(run.StepsSkipped),
		StepsCancelled: int(run.StepsCancelled),
		StepsCollected: int(run.StepsCollected),
		CreatedAt:      run.CreatedAt,
		StartedAt:      run.StartedAt,
		FinishedAt:     run.FinishedAt,
		DeadlineAt:     run.DeadlineAt,
		EventSeq:       run.NextSeq,
		Cost:           costSummary(run),
	}
	if run.DefinitionID != nil {
		v.DefinitionID = run.DefinitionID.String()
	}
	if run.ParkReason != nil {
		v.ParkReason = *run.ParkReason
	}
	if run.CancelReason != nil {
		v.CancelReason = *run.CancelReason
	}
	return v
}

// buildRunResponse assembles the wire projection from the store rows.
func buildRunResponse(run gen.Run, steps []gen.RunStep, edges []gen.RunEdge, attempts []gen.StepAttempt, deadLetters []gen.DeadLetter, approvals []gen.Approval) RunResponse {
	byStep := make(map[string][]AttemptView, len(steps))
	for _, a := range attempts {
		v := AttemptView{
			Attempt:    int(a.AttemptNo),
			ClaimID:    a.ClaimID.String(),
			Error:      a.Error,
			Usage:      a.Usage,
			Verdict:    a.Verdict,
			Repair:     a.Repair,
			Feedback:   a.Feedback,
			StartedAt:  a.StartedAt,
			FinishedAt: a.FinishedAt,
		}
		if a.Outcome != nil {
			v.Outcome = *a.Outcome
		}
		if a.WorkerID != nil {
			v.WorkerID = *a.WorkerID
		}
		byStep[a.StepID] = append(byStep[a.StepID], v)
	}

	resp := RunResponse{
		Run:   buildRunView(run),
		Steps: make([]StepView, 0, len(steps)),
		Edges: make([]EdgeView, 0, len(edges)),
	}
	for _, d := range deadLetters {
		v := DeadLetterView{
			StepID:          d.StepID,
			Seq:             int(d.Seq),
			Source:          d.Source,
			Error:           d.Error,
			AttemptsAtDeath: int(d.AttemptsAtDeath),
			CreatedAt:       d.CreatedAt,
		}
		if d.Class != nil {
			v.Class = *d.Class
		}
		resp.DeadLetters = append(resp.DeadLetters, v)
	}
	for _, a := range approvals {
		resp.Approvals = append(resp.Approvals, buildApprovalView(a))
	}
	for _, s := range steps {
		transport, validation := failureCounts(byStep[s.StepID])
		resp.Steps = append(resp.Steps, StepView{
			ID:                 s.StepID,
			Type:               s.StepType,
			Status:             s.Status,
			RemainingDeps:      int(s.RemainingDeps),
			FiredDeps:          int(s.FiredDeps),
			AttemptCount:       int(s.AttemptCount),
			Output:             s.Output,
			Error:              s.Error,
			TransportFailures:  transport,
			ValidationFailures: validation,
			StartedAt:          s.StartedAt,
			FinishedAt:         s.FinishedAt,
			Attempts:           byStep[s.StepID],
			Validation:         summarizeVerdicts(byStep[s.StepID]),
			Config:             s.Config,
			IdempotencyKey:     effects.Key(run.ID, s.StepID),
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
