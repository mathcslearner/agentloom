package api

import (
	"encoding/json"
	"time"
)

// The wire contract (v1, dev mode). cmd/ctl imports these types; the
// OpenAPI spec formalizing them is ticket 6.6.

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
type SubmitRunRequest struct {
	Definition   json.RawMessage `json:"definition,omitempty"`
	DefinitionID string          `json:"definition_id,omitempty"`
	// Params are the run parameters, stored opaquely (value validation
	// against the definition's ParamSpecs is M6).
	Params json.RawMessage `json:"params,omitempty"`
	// IdempotencyToken makes submission idempotent: resubmitting with the
	// same token returns the original run (200, reused=true).
	IdempotencyToken string `json:"idempotency_token,omitempty"`
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
}

// RunView is the run row's client-facing projection.
type RunView struct {
	ID             string     `json:"id"`
	Status         string     `json:"status"`
	StepsTotal     int        `json:"steps_total"`
	StepsSucceeded int        `json:"steps_succeeded"`
	StepsFailed    int        `json:"steps_failed"`
	StepsSkipped   int        `json:"steps_skipped"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
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
