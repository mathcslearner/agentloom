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
	// ErrCodeCacheUnavailable: the response-cache ops surface (bust/stats,
	// ticket 9.6) is not wired on this API — either caching is disabled or
	// the API was built without the cache store (503). ADR-002's Redis
	// independence means the cache ops are an opt-in extra, never a boot
	// dependency; an operator who wants them enables the cache.
	ErrCodeCacheUnavailable = "cache_unavailable"
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
	ID             string `json:"id"`
	DefinitionID   string `json:"definition_id,omitempty"`
	Status         string `json:"status"`
	OnFailure      string `json:"on_failure"`
	StepsTotal     int    `json:"steps_total"`
	StepsSucceeded int    `json:"steps_succeeded"`
	StepsFailed    int    `json:"steps_failed"`
	StepsSkipped   int    `json:"steps_skipped"`
	StepsCancelled int    `json:"steps_cancelled"`
	// StepsCollected counts map instances tolerated under collect_errors
	// (ticket 13.4b): terminal failures the run did not fail on.
	StepsCollected int        `json:"steps_collected"`
	ParkReason     string     `json:"park_reason,omitempty"`
	CancelReason   string     `json:"cancel_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	DeadlineAt     *time.Time `json:"deadline_at,omitempty"`
	// Cost is the run's materialized cost summary (ticket 10.2, ADR-012):
	// cumulative spend and cache savings. Always present; zero for a run with
	// no cost-bearing attempts.
	Cost CostSummaryView `json:"cost"`
}

// CostSummaryView is a run's cumulative cost and budget (ticket 10.2/10.3,
// ADR-012). Money is integer nano-USD on the wire (the exact source of truth);
// the *_usd strings are the human-readable USD rendering derived from the
// integers. Budget is nullable (nil = unbudgeted); on_budget_exceeded is the
// materialized disposition (park | fail), always present.
type CostSummaryView struct {
	SpentNanoUSD     int64   `json:"spent_nano_usd"`
	SavedNanoUSD     int64   `json:"saved_nano_usd"`
	SpentUSD         string  `json:"spent_usd"`
	SavedUSD         string  `json:"saved_usd"`
	BudgetNanoUSD    *int64  `json:"budget_nano_usd,omitempty"`
	BudgetUSD        *string `json:"budget_usd,omitempty"`
	OnBudgetExceeded string  `json:"on_budget_exceeded"`
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
	// TransportFailures counts this step's transport-retry failures (attempt
	// outcomes transient/timeout — ADR-006), and ValidationFailures its
	// semantic failures (attempt outcome validation_failed — ADR-013, ticket
	// 11.4). The two budgets are disjoint, so the counters are reported
	// separately: a step can exhaust one while the other is untouched.
	TransportFailures  int           `json:"transport_failures"`
	ValidationFailures int           `json:"validation_failures"`
	StartedAt          *time.Time    `json:"started_at,omitempty"`
	FinishedAt         *time.Time    `json:"finished_at,omitempty"`
	Attempts           []AttemptView `json:"attempts,omitempty"`
	// Validation is the per-step output-validation summary (ticket 11.6,
	// ADR-013): a compact roll-up of the step's attempt verdicts (pass/fail
	// counts, the latest verdict, and a per-validator breakdown), so a client
	// reads a step's quality health without parsing every attempt's raw
	// verdict. Present only when at least one attempt carried a verdict;
	// absent for every unvalidated step.
	Validation *ValidationSummaryView `json:"validation,omitempty"`
}

// ValidationSummaryView is a step's per-attempt verdict roll-up (ticket 11.6,
// ADR-013). It is derived from the step's attempt verdicts at read time — a
// pure projection of attempts[].verdict, no stored state — and is present only
// when the step carried a validation chain.
type ValidationSummaryView struct {
	// Attempts is how many of the step's attempts carried a verdict.
	Attempts int `json:"attempts"`
	// Passed and Failed count those verdicts by overall status.
	Passed int `json:"passed"`
	Failed int `json:"failed"`
	// LastAttempt is the attempt number of the most recent verdict, and
	// LastStatus its overall status (pass/fail).
	LastAttempt int    `json:"last_attempt"`
	LastStatus  string `json:"last_status"`
	// LastScore is the most recent verdict's overall score (the chain minimum),
	// when one was reported; nil otherwise.
	LastScore *float64 `json:"last_score,omitempty"`
	// LastIssueCount is the number of issues on the most recent verdict.
	LastIssueCount int `json:"last_issue_count"`
	// Validators is the per-validator roll-up across the step's attempts, in
	// the latest verdict's chain order.
	Validators []ValidatorSummaryView `json:"validators,omitempty"`
}

// ValidatorSummaryView is one validator's roll-up across a step's attempts
// (ticket 11.6): how it judged the output over the semantic-retry loop.
type ValidatorSummaryView struct {
	// Name is the validator's plugin name.
	Name string `json:"name"`
	// Passed, Failed, Skipped, and Errored count this validator's per-attempt
	// results by status (skipped: a cheaper validator already failed; errored:
	// a cost-bearing validator under on_error:skip).
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	Errored int `json:"errored"`
	// LastStatus is this validator's status on the step's most recent verdict.
	LastStatus string `json:"last_status"`
	// LastScore is this validator's most recent reported score (nil when it
	// reports none — every deterministic validator).
	LastScore *float64 `json:"last_score,omitempty"`
}

// AttemptView is one execution try of a step.
type AttemptView struct {
	Attempt int             `json:"attempt"`
	ClaimID string          `json:"claim_id"`
	Outcome string          `json:"outcome,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
	// Usage is the attempt's token accounting, present only on a
	// successful llm attempt (ticket 8.6): {input_tokens, output_tokens}.
	// Absent for every other step type and outcome.
	Usage json.RawMessage `json:"usage,omitempty"`
	// Verdict is the output-validation chain verdict (ticket 11.1, ADR-013),
	// present on a succeeded or validation_failed attempt of a step carrying
	// a validation chain: {schema_version, status, score?, issues[],
	// results[]}. Absent for every unvalidated step.
	Verdict json.RawMessage `json:"verdict,omitempty"`
	// Repair is the structured-output provenance (ticket 11.3, ADR-013),
	// present on an attempt of an llm step that declared an output_format:
	// {schema_version, status, steps?, raw_text?}. Absent otherwise.
	Repair json.RawMessage `json:"repair,omitempty"`
	// Feedback is the semantic-retry critique this attempt was given (ticket
	// 11.4, ADR-013): {schema_version, semantic_attempt, max_attempts,
	// prior_attempt, text}. Present on a feedback-augmented re-attempt of a step
	// with a semantic policy; absent on a first attempt.
	Feedback   json.RawMessage `json:"feedback,omitempty"`
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

// RunGraphResponse answers GET /v1/runs/{id}/graph (ticket 13.6, ADR-015):
// the run's current versioned graph with per-row provenance, plus the ordered
// per-version expansion deltas. It is the contract the M18 dashboard uses to
// render and animate runtime graph expansion (planner / map / loop injection).
//
// GraphVersion is the current (highest) version — the definition-authored
// graph is version 1, and each expansion bumps it, so GraphVersion equals the
// run's expansion count + 1 (ADR-004). Every node and edge carries the version
// at which it was introduced, so a client reconstructs any version N by
// keeping the rows with graph_version <= N; Expansions replays the same
// history as an ordered delta feed (built from the graph_expanded events).
type RunGraphResponse struct {
	RunID        string               `json:"run_id"`
	GraphVersion int                  `json:"graph_version"`
	StepsTotal   int                  `json:"steps_total"`
	Nodes        []GraphNodeView      `json:"nodes"`
	Edges        []GraphEdgeView      `json:"edges"`
	Expansions   []GraphExpansionView `json:"expansions"`
}

// GraphOriginView is a node's or edge's provenance (ticket 13.6, ADR-015).
// Kind is "definition" for an authored row (no injecting step), or the
// expansion kind — "planner", "map", or "loop" — with Step naming the step
// whose completion injected the row.
type GraphOriginView struct {
	Kind string `json:"kind"`
	Step string `json:"step,omitempty"`
}

// GraphNodeView is one graph node: its identity, live status, expansion
// nesting depth, the version at which it was introduced, its provenance, and
// the time it was added (the run's creation time for authored nodes, the
// expansion event's time for injected ones).
type GraphNodeView struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	Depth        int             `json:"depth"`
	GraphVersion int             `json:"graph_version"`
	Origin       GraphOriginView `json:"origin"`
	AddedAt      time.Time       `json:"added_at"`
}

// GraphEdgeView is one graph edge with its provenance and the version at
// which it was spliced in. (Named distinctly from the run-detail EdgeView,
// which omits provenance.)
type GraphEdgeView struct {
	From         string          `json:"from"`
	To           string          `json:"to"`
	Type         string          `json:"type"`
	When         string          `json:"when,omitempty"`
	Resolution   string          `json:"resolution"`
	GraphVersion int             `json:"graph_version"`
	Origin       GraphOriginView `json:"origin"`
}

// GraphEdgeRef names an edge introduced by an expansion, by its endpoints and
// type — enough for a client to match it against the Edges list.
type GraphEdgeRef struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// GraphExpansionView is one expansion's delta (ticket 13.6, ADR-015),
// reconstructed from a graph_expanded event. Version is the version this
// expansion produced (from_version + 1); AddedSteps / AddedEdges are what it
// injected; Readied are the injected steps made immediately runnable; Widened
// are the pre-existing pending steps whose dependency counts this expansion
// grew (the "before"-splice targets).
type GraphExpansionView struct {
	Version     int            `json:"version"`
	FromVersion int            `json:"from_version"`
	OriginStep  string         `json:"origin_step"`
	OriginKind  string         `json:"origin_kind"`
	Depth       int            `json:"depth"`
	AddedAt     time.Time      `json:"added_at"`
	AddedSteps  []string       `json:"added_steps"`
	AddedEdges  []GraphEdgeRef `json:"added_edges"`
	Readied     []string       `json:"readied,omitempty"`
	Widened     []string       `json:"widened,omitempty"`
}

// RunCostResponse answers GET /v1/runs/{id}/cost (ticket 10.2, ADR-012): the
// run's cumulative cost plus per-step and per-resource (per-model / per-tool)
// breakdowns and the full per-attempt ledger. Money is integer nano-USD on
// the wire; the *_usd strings are the derived USD rendering.
type RunCostResponse struct {
	RunID      string               `json:"run_id"`
	Summary    CostSummaryView      `json:"summary"`
	ByStep     []CostByStepView     `json:"by_step"`
	ByResource []CostByResourceView `json:"by_resource"`
	Entries    []CostEntryView      `json:"entries"`
}

// CostByStepView is one step's spend/savings roll-up. OverheadNanoUSD is the
// slice of SpentNanoUSD attributed to validation machinery — an llm_judge's
// provider call (ADR-012 rule 4, ticket 11.5) — so a reader can separate
// productive spend from the cost of judging it.
type CostByStepView struct {
	StepID          string `json:"step_id"`
	Entries         int64  `json:"entries"`
	SpentNanoUSD    int64  `json:"spent_nano_usd"`
	SavedNanoUSD    int64  `json:"saved_nano_usd"`
	OverheadNanoUSD int64  `json:"overhead_nano_usd"`
	SpentUSD        string `json:"spent_usd"`
	SavedUSD        string `json:"saved_usd"`
	OverheadUSD     string `json:"overhead_usd"`
}

// CostByResourceView is one model's or tool's spend/savings roll-up: the
// resource is the model name ("mock:sim-1") or "tool:<name>", with summed
// input/output tokens across its attempts (zero for tool rows).
type CostByResourceView struct {
	Resource     string `json:"resource"`
	Entries      int64  `json:"entries"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	SpentNanoUSD int64  `json:"spent_nano_usd"`
	SavedNanoUSD int64  `json:"saved_nano_usd"`
	SpentUSD     string `json:"spent_usd"`
	SavedUSD     string `json:"saved_usd"`
}

// CostEntryView is one cost_ledger row: what a single attempt cost, at what
// rate, with what provenance. CacheHit rows carry cost 0 and a Saved figure.
type CostEntryView struct {
	StepID       string          `json:"step_id"`
	Attempt      int             `json:"attempt"`
	Entry        string          `json:"entry"`
	Resource     string          `json:"resource"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	Rate         json.RawMessage `json:"rate"`
	RateSource   string          `json:"rate_source"`
	CacheHit     bool            `json:"cache_hit"`
	Overhead     bool            `json:"overhead"`
	SpentNanoUSD int64           `json:"spent_nano_usd"`
	SavedNanoUSD int64           `json:"saved_nano_usd"`
	CreatedAt    time.Time       `json:"created_at"`
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

// BlackboardEntryView is one blackboard entry on the wire (ticket 12.2,
// ADR-014): one version of one run-scoped key. Value is the stored JSON
// verbatim; TokenCount is its size under TokenCounter (a counter
// fingerprint). Tags is never null. AuthorStepID/AuthorAttempt name the
// step that wrote the version (omitted for a non-step writer).
type BlackboardEntryView struct {
	Key           string          `json:"key"`
	Version       int             `json:"version"`
	Value         json.RawMessage `json:"value"`
	TokenCount    int             `json:"token_count"`
	TokenCounter  string          `json:"token_counter"`
	Tags          []string        `json:"tags"`
	AuthorStepID  string          `json:"author_step_id,omitempty"`
	AuthorAttempt int             `json:"author_attempt,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// BlackboardResponse answers GET /v1/runs/{id}/blackboard (ticket 12.2): one
// keyset page of the run's blackboard. By default it returns each key's head
// (latest version), ordered by key; ?history=true returns every version,
// ordered by (key, version). NextCursor, when set, fetches the next page via
// ?cursor=; absent means this was the last page.
type BlackboardResponse struct {
	RunID      string                `json:"run_id"`
	History    bool                  `json:"history"`
	Entries    []BlackboardEntryView `json:"entries"`
	NextCursor string                `json:"next_cursor,omitempty"`
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

// SetBudgetRequest is the body of PATCH /v1/runs/{id}/budget (ticket 10.3):
// the new run spend budget in US dollars. Required and positive.
type SetBudgetRequest struct {
	BudgetUSD *float64 `json:"budget_usd"`
}

// SetBudgetResponse answers PATCH /v1/runs/{id}/budget: the run with its
// updated cost/budget summary. Raising the budget does not resume a parked
// run — unpark does.
type SetBudgetResponse struct {
	Run RunView `json:"run"`
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

// PluginCapabilities are ADR-009's capability flags on the wire (ticket
// 8.1). All three are always present — a flag's absence and its falseness
// are the same statement.
type PluginCapabilities struct {
	SideEffectful bool `json:"side_effectful"`
	Cacheable     bool `json:"cacheable"`
	CostBearing   bool `json:"cost_bearing"`
}

// PluginInfo is one plugin's listing entry on GET /v1/plugins: the
// manifest the plugin registered at boot (ADR-009), config schema
// embedded verbatim as the JSON Schema document UI forms consume (M17.4).
type PluginInfo struct {
	// Kind is the plugin kind: executor | tool | retriever |
	// model_provider | validator.
	Kind string `json:"kind"`
	// Name identifies the plugin within its kind; for executors it is the
	// step type.
	Name string `json:"name"`
	// Version is the plugin's semver version string (feeds M9 cache keys).
	Version string `json:"version"`
	// Description is the optional one-line human description.
	Description string `json:"description,omitempty"`
	// Capabilities are the ADR-009 capability flags.
	Capabilities PluginCapabilities `json:"capabilities"`
	// ConfigSchema is the plugin's generated config JSON Schema (2020-12),
	// embedded verbatim; absent when the plugin takes no config.
	ConfigSchema json.RawMessage `json:"config_schema,omitempty"`
}

// ListPluginsResponse answers GET /v1/plugins: the catalog compiled into
// the API binary (ADR-009's in-process model — API and workers ship from
// one build), sorted by kind then name.
type ListPluginsResponse struct {
	Plugins []PluginInfo `json:"plugins"`
}

// CacheBustRequest is the body of POST /v1/cache/bust (ticket 9.6, ADR-011):
// the namespace selector for an admin cache invalidation. Both fields
// omitted busts every entry; plugin_kind alone busts one kind (all model
// providers, all tools, all retrievers); plugin_kind + plugin_name busts one
// concrete plugin. A plugin_name without a plugin_kind is a 400 (names are
// unique only within a kind). The busting granularity is the RedisKey
// namespace — a single run's entries cannot be busted (their bound is TTL).
type CacheBustRequest struct {
	// PluginKind is one of the cacheable plugin kinds: model_provider, tool,
	// retriever. Empty (with an empty name) means every kind.
	PluginKind string `json:"plugin_kind,omitempty"`
	// PluginName is the concrete plugin within the kind; requires PluginKind.
	PluginName string `json:"plugin_name,omitempty"`
}

// CacheBustResponse reports the outcome of a bust: how many Redis keys were
// removed. Point-in-time — entries written concurrently by a live worker
// after the scan passed their slot are not counted (ADR-011).
type CacheBustResponse struct {
	Deleted int64 `json:"deleted"`
}

// CacheStatsResponse answers GET /v1/cache/stats: per-plugin cumulative
// hit/miss/store counters (ticket 9.6). The numbers reconcile against the
// worker fleet's engine_cache_* Prometheus counters on the normal path
// (ADR-011); they are durable in Redis so the API can serve them without a
// worker (ADR-002).
type CacheStatsResponse struct {
	Plugins []CachePluginStat `json:"plugins"`
}

// CachePluginStat is one concrete plugin's cache counters with the derived
// hit rate (hits/(hits+misses), 0 when there were no lookups).
type CachePluginStat struct {
	Kind    string  `json:"kind"`
	Name    string  `json:"name"`
	Hits    int64   `json:"hits"`
	Misses  int64   `json:"misses"`
	Stores  int64   `json:"stores"`
	HitRate float64 `json:"hit_rate"`
}
