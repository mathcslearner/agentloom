package api

import (
	"encoding/json"
	"time"
)

// The wire contract (v1). cmd/ctl imports these types; api/openapi.yaml
// (ticket 6.6) is the formal contract mirroring them schema-for-schema.
// Route drift is caught by TestOpenAPIRouteCoverage; field-level changes
// here must update the spec's components in the same change.

// Error codes. Part of the contract: renaming one is a breaking change.
const (
	// ErrCodeInvalidRequest: the request itself is malformed — bad JSON,
	// unknown body fields, a bad UUID, or a violated body rule.
	ErrCodeInvalidRequest = "invalid_request"
	// ErrCodeInvalidDefinition: the submitted definition failed decoding or
	// M1 validation; Issues carries the path-qualified findings.
	ErrCodeInvalidDefinition = "invalid_definition"
	// ErrCodeDefinitionNotFound: the referenced stored definition does not
	// exist.
	ErrCodeDefinitionNotFound = "definition_not_found"
	// ErrCodeRunNotFound: no run with the requested id.
	ErrCodeRunNotFound = "run_not_found"
	// ErrCodeKeyNotFound: no API key with the requested id.
	ErrCodeKeyNotFound = "key_not_found"
	// ErrCodeUnauthorized: missing or invalid credential (401). Every
	// credential failure — absent header, bad shape, unknown key, revoked,
	// expired — collapses to this one code (ADR-007).
	ErrCodeUnauthorized = "unauthorized"
	// ErrCodeForbidden: valid credential, missing scope (403).
	ErrCodeForbidden = "forbidden"
	// ErrCodeRateLimited: over a rate limit (429); the Retry-After header
	// says when to try again (ticket 6.4, ADR-007).
	ErrCodeRateLimited = "rate_limited"
	// ErrCodeStepNotFound: the run exists but has no step with the
	// requested id (ticket 6.5).
	ErrCodeStepNotFound = "step_not_found"
	// ErrCodeConflict: the request is well-formed but the entity's current
	// state refuses it (409) — cancelling a finished run, unparking a
	// running run, requeueing a step that is not dead-lettered, creating a
	// definition name that already exists (ticket 6.5).
	ErrCodeConflict = "conflict"
	// ErrCodeIdempotencyMismatch: the Idempotency-Key was seen before but
	// with a different payload (definition, params, or definition ref);
	// the replay is refused instead of returning the original run (409,
	// ticket 6.5).
	ErrCodeIdempotencyMismatch = "idempotency_key_conflict"
	// ErrCodeNotFound / ErrCodeMethodNotAllowed: routing misses.
	ErrCodeNotFound         = "not_found"
	ErrCodeMethodNotAllowed = "method_not_allowed"
	// ErrCodeInternal: the request was fine, the server was not.
	ErrCodeInternal = "internal"
)

// ErrorBody is the envelope every non-2xx response carries.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the error payload inside the envelope.
type ErrorDetail struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Issues  []Issue `json:"issues,omitempty"`
}

// Issue is one path-qualified definition problem — a decode error or an
// M1 validation issue, in the dag package's vocabulary.
type Issue struct {
	// Code is the dag validation code (empty for codec-level errors).
	Code string `json:"code,omitempty"`
	// Severity is "error" or "warning"; decode errors are always "error".
	Severity string `json:"severity"`
	// Path is the JSON path of the offender; empty means the document.
	Path string `json:"path,omitempty"`
	Msg  string `json:"msg"`
}

// SubmitRunRequest is the POST /v1/runs body. Exactly one of Definition
// (inline document) and DefinitionID (stored definition ref) must be set.
// Idempotent submission is requested via the Idempotency-Key header, not
// the body (ticket 6.5; the pre-6.5 idempotency_token body field is gone):
// resubmitting the same key with the same payload returns the original run
// (200, reused=true), while the same key with a different payload is a 409.
type SubmitRunRequest struct {
	Definition   json.RawMessage `json:"definition,omitempty"`
	DefinitionID string          `json:"definition_id,omitempty"`
	// Params are the run parameters, stored opaquely (value validation
	// against the definition's ParamSpecs is M6).
	Params json.RawMessage `json:"params,omitempty"`
}

// SubmitRunResponse answers POST /v1/runs: 201 on creation, 200 with
// Reused set when an idempotency token matched an existing run.
type SubmitRunResponse struct {
	RunID      string   `json:"run_id"`
	Status     string   `json:"status"`
	EntrySteps []string `json:"entry_steps"`
	Reused     bool     `json:"reused,omitempty"`
}

// RunResponse answers GET /v1/runs/{id}.
type RunResponse struct {
	Run   RunView    `json:"run"`
	Steps []StepView `json:"steps"`
	Edges []EdgeView `json:"edges"`
	// DeadLetters lists the run's DLQ records (ticket 6.5) — how a client
	// discovers which steps are requeueable. Empty for healthy runs.
	DeadLetters []DeadLetterView `json:"dead_letters,omitempty"`
}

// RunView is the run row's client-facing projection.
type RunView struct {
	ID             string     `json:"id"`
	DefinitionID   string     `json:"definition_id,omitempty"`
	Status         string     `json:"status"`
	OnFailure      string     `json:"on_failure"`
	StepsTotal     int        `json:"steps_total"`
	StepsSucceeded int        `json:"steps_succeeded"`
	StepsFailed    int        `json:"steps_failed"`
	StepsSkipped   int        `json:"steps_skipped"`
	StepsCancelled int        `json:"steps_cancelled"`
	ParkReason     string     `json:"park_reason,omitempty"`
	CancelReason   string     `json:"cancel_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	DeadlineAt     *time.Time `json:"deadline_at,omitempty"`
}

// StepView is one run step with its attempt history.
type StepView struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Status        string          `json:"status"`
	RemainingDeps int             `json:"remaining_deps"`
	FiredDeps     int             `json:"fired_deps"`
	AttemptCount  int             `json:"attempt_count"`
	Output        json.RawMessage `json:"output,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
	StartedAt     *time.Time      `json:"started_at,omitempty"`
	FinishedAt    *time.Time      `json:"finished_at,omitempty"`
	Attempts      []AttemptView   `json:"attempts,omitempty"`
}

// AttemptView is one execution try of a step.
type AttemptView struct {
	Attempt    int             `json:"attempt"`
	ClaimID    string          `json:"claim_id"`
	Outcome    string          `json:"outcome,omitempty"`
	Error      json.RawMessage `json:"error,omitempty"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// EdgeView is one run edge with its resolution — enough for a client to
// render the graph and see which paths fired.
type EdgeView struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Type       string `json:"type"`
	Resolution string `json:"resolution"`
}

// DeadLetterView is one DLQ record (ticket 6.5). Seq counts the step's
// deaths; AttemptsAtDeath is the requeue budget baseline.
type DeadLetterView struct {
	StepID          string          `json:"step_id"`
	Seq             int             `json:"seq"`
	Source          string          `json:"source"`
	Class           string          `json:"class,omitempty"`
	Error           json.RawMessage `json:"error,omitempty"`
	AttemptsAtDeath int             `json:"attempts_at_death"`
	CreatedAt       time.Time       `json:"created_at"`
}

// StepLogLineView is one captured executor log line (ticket 7.4).
type StepLogLineView struct {
	// Seq is the line's position in the attempt's log stream, 1-based and
	// monotonic; gaps mark lines lost to the capture buffer or ring cap.
	Seq     int64  `json:"seq"`
	Level   string `json:"level"`
	Message string `json:"message"`
	// Fields carries the executor call-site attributes as one JSON object;
	// absent when the line had none.
	Fields json.RawMessage `json:"fields,omitempty"`
	// TraceID joins the line to its attempt's trace (hex); absent when
	// tracing was off at capture.
	TraceID  string    `json:"trace_id,omitempty"`
	LoggedAt time.Time `json:"logged_at"`
}

// StepLogsResponse answers GET /v1/runs/{id}/steps/{sid}/logs (ticket
// 7.4): one ascending-seq keyset page of one attempt's captured lines.
// Truncated reports that the attempt lost lines — DroppedLines many —
// to the size-capped ring or the capture buffer; the retained window is
// the newest lines. NextCursor, when set, fetches the next page via
// ?cursor=; absent means this was the last page.
type StepLogsResponse struct {
	RunID        string            `json:"run_id"`
	StepID       string            `json:"step_id"`
	Attempt      int               `json:"attempt"`
	Lines        []StepLogLineView `json:"lines"`
	Truncated    bool              `json:"truncated"`
	DroppedLines int64             `json:"dropped_lines,omitempty"`
	NextCursor   string            `json:"next_cursor,omitempty"`
}

// ListRunsResponse answers GET /v1/runs (ticket 6.5): one keyset page,
// newest-first. NextCursor, when set, fetches the next page verbatim via
// ?cursor=; absent means this was the last page.
type ListRunsResponse struct {
	Runs       []RunView `json:"runs"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// CancelRunResponse answers POST /v1/runs/{id}/cancel: the run is
// cancelling (or already cancelled when Finalized — nothing was in
// flight); CancelledSteps lists the claimless steps the request swept.
type CancelRunResponse struct {
	Run            RunView  `json:"run"`
	CancelledSteps []string `json:"cancelled_steps"`
	Finalized      bool     `json:"finalized"`
}

// ParkRunResponse answers POST /v1/runs/{id}/park.
type ParkRunResponse struct {
	Run RunView `json:"run"`
}

// UnparkRunResponse answers POST /v1/runs/{id}/unpark; Dispatched lists
// the ready steps re-dispatched because their deliveries were consumed
// while the run was parked.
type UnparkRunResponse struct {
	Run        RunView  `json:"run"`
	Dispatched []string `json:"dispatched"`
}

// RequeueStepResponse answers POST /v1/runs/{id}/steps/{sid}/requeue: the
// dead-lettered step is back in ready with its retry budget re-armed.
type RequeueStepResponse struct {
	RunID      string   `json:"run_id"`
	StepID     string   `json:"step_id"`
	Status     string   `json:"status"`
	RunResumed bool     `json:"run_resumed"`
	Revived    []string `json:"revived,omitempty"`
	Dispatched []string `json:"dispatched"`
}

// CreateDefinitionRequest is the body of POST /v1/definitions and
// POST /v1/definitions/{name}/versions (ticket 6.5).
type CreateDefinitionRequest struct {
	Definition json.RawMessage `json:"definition"`
}

// DefinitionView is one registry row's summary — everything but the spec.
type DefinitionView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// DefinitionResponse answers POST /v1/definitions, POST
// /v1/definitions/{name}/versions, and GET /v1/definitions/{id}: the
// summary plus the stored canonical spec.
type DefinitionResponse struct {
	DefinitionView
	Spec json.RawMessage `json:"spec"`
}

// ListDefinitionsResponse answers GET /v1/definitions (ticket 6.5): one
// keyset page of each name's newest version, in name order. NextCursor,
// when set, fetches the next page via ?cursor=.
type ListDefinitionsResponse struct {
	Definitions []DefinitionView `json:"definitions"`
	NextCursor  string           `json:"next_cursor,omitempty"`
}

// DefinitionVersionsResponse answers GET /v1/definitions/{name}/versions:
// every version of one name, oldest first.
type DefinitionVersionsResponse struct {
	Versions []DefinitionView `json:"versions"`
}

// CreateKeyRequest is the POST /v1/keys body (ticket 6.1, ADR-007).
type CreateKeyRequest struct {
	// Name is the human label shown in listings, at most 200 characters.
	Name string `json:"name"`
	// Scopes is the granted scope set: submit, read, approve, admin.
	Scopes []string `json:"scopes"`
	// TTL is an optional positive Go duration (e.g. "720h"); the server
	// resolves it to an absolute expiry against its own clock. Empty
	// means the key never expires.
	TTL string `json:"ttl,omitempty"`
}

// KeyView is one API key's client-facing projection: the lookup prefix
// is the only key material it ever carries.
type KeyView struct {
	ID        string     `json:"id"`
	Prefix    string     `json:"prefix"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// CreateKeyResponse answers POST /v1/keys. Key is the plaintext
// credential, shown in this response once and recoverable nowhere else.
type CreateKeyResponse struct {
	KeyView
	Key string `json:"key"`
}

// ListKeysResponse answers GET /v1/keys.
type ListKeysResponse struct {
	Keys []KeyView `json:"keys"`
}
