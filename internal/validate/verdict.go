package validate

import "encoding/json"

// VerdictSchemaVersion is the schema version of a persisted Verdict. It is
// stamped on every verdict so a reader (the status API, 11.4's feedback
// builder, the M18 UI) can evolve the shape without guessing. Bumped only
// on a breaking change to the verdict JSON.
const VerdictSchemaVersion = 1

// Status is a validator's or a chain's pass/fail judgment. A validator may
// report only pass or fail; the per-validator result adds the
// administrative StatusSkipped (a cost-bearing validator the chain never
// ran because a cheaper one already failed) and StatusError (11.5's judge
// provider error handled by on_error: skip).
type Status string

// The verdict statuses.
const (
	// StatusPass — the output satisfied the validator (or the whole chain).
	StatusPass Status = "pass"
	// StatusFail — the output was rejected. On the chain, any failing
	// validator makes the chain fail.
	StatusFail Status = "fail"
	// StatusSkipped — a per-validator result only: the chain did not run
	// this (cost-bearing) validator because a cheaper one already failed
	// (ADR-013 cheap-first ordering). Never a chain status.
	StatusSkipped Status = "skipped"
	// StatusError — a per-validator result only (11.5): the validator
	// itself errored and on_error=skip suppressed it. Never a chain status
	// in 11.1 (a validator error aborts the chain and routes as a transport
	// failure).
	StatusError Status = "error"
)

// Issue is one problem a validator found with the output, qualified by a
// machine code and (for structured outputs) the JSON pointer of the
// offending sub-value. It is the raw material 11.4's semantic retry turns
// into a feedback prompt, so it names structure — never instance values —
// keeping outputs out of the verdict (secret hygiene, and a stable prompt).
type Issue struct {
	// Validator is the name of the validator that raised the issue — so a
	// chain verdict's concatenated issues stay attributable.
	Validator string `json:"validator"`
	// Code is the machine-readable issue code (e.g. "type_mismatch",
	// "pattern_no_match", "rubric_below_threshold"). Part of the verdict
	// contract for programmatic consumers.
	Code string `json:"code"`
	// Path is the RFC 6901 JSON pointer into the validated value locating
	// the offender; empty means the whole value.
	Path string `json:"path,omitempty"`
	// Message is a human-readable description of the problem — structure
	// only (missing field, wrong type, failed predicate), never the
	// instance value that triggered it.
	Message string `json:"message"`
}

// ValidatorResult is one validator's contribution to a chain verdict: its
// name, its judgment, an optional score, how many issues it raised, and how
// long it took. The chain records one per configured validator (including
// skipped ones) so the status API and 11.6's metrics can attribute quality
// per validator.
type ValidatorResult struct {
	// Name is the validator's plugin name.
	Name string `json:"name"`
	// Status is this validator's judgment: pass, fail, skipped, or error.
	Status Status `json:"status"`
	// Score is this validator's optional [0,1] quality score; nil when the
	// validator reports none.
	Score *float64 `json:"score,omitempty"`
	// IssueCount is how many issues this validator raised.
	IssueCount int `json:"issue_count"`
	// Rationale is a cost-bearing validator's explanation of its judgment
	// (11.5's llm_judge): the model's prose reasoning, on a pass and a fail
	// alike. Empty for the deterministic validators, which have no rationale
	// beyond their issue codes. Structure/explanation only — never the
	// validated output verbatim (secret hygiene).
	Rationale string `json:"rationale,omitempty"`
	// Usage is a cost-bearing validator's token accounting (11.5's
	// llm_judge): the resolved resource and the judge call's tokens, so the
	// engine can ledger the judge's provider call as overhead on the serving
	// step (ADR-012 rule 4). Nil for a validator that made no metered call
	// (every deterministic validator, and a judge whose call did not bill).
	Usage *ValidatorUsage `json:"usage,omitempty"`
	// Error is the suppressed failure message when Status is `error` — a
	// cost-bearing validator that errored under `on_error: skip` (11.5): the
	// chain did not fail, but this result records that the validator could
	// not render a judgment. Empty otherwise.
	Error string `json:"error,omitempty"`
	// DurationMS is the wall-clock time this validator took, milliseconds.
	// Diagnostics only; excluded from equality in tests.
	DurationMS int64 `json:"duration_ms"`
}

// ValidatorUsage is a cost-bearing validator's token accounting (11.5): the
// resource its call bills to (for the cost ledger's overhead row) plus the
// tokens the judge model consumed. It carries no output or rubric text —
// only counts and identifiers — so it is safe on a verdict and on an Error.
type ValidatorUsage struct {
	// Resource is the ADR-010/ADR-012 resource the judge call bills to,
	// "<resolved-provider>:<served-model>" — the key the overhead ledger row
	// prices against.
	Resource string `json:"resource"`
	// Model is the model that served the judge call (the served model, which
	// may differ from a requested alias).
	Model string `json:"model"`
	// InputTokens / OutputTokens are the judge call's token accounting.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Verdict is a validator's or a chain's judgment on a step output — the
// contract the whole M11 feature turns on (ADR-013). A single validator
// returns one from Validate (Results empty or one-length); the chain
// aggregates the per-validator verdicts into one with a full Results slice.
// It is persisted verbatim on the attempt row (step_attempts.verdict) and
// served as attempts[].verdict.
type Verdict struct {
	// SchemaVersion is VerdictSchemaVersion.
	SchemaVersion int `json:"schema_version"`
	// Status is the overall judgment: pass or fail.
	Status Status `json:"status"`
	// Score is the overall [0,1] score; for a chain it is the minimum of
	// the per-validator reported scores (nil when none reported one).
	Score *float64 `json:"score,omitempty"`
	// Issues is every failing validator's issues, in chain order — the
	// feedback material for 11.4.
	Issues []Issue `json:"issues,omitempty"`
	// Results is the per-validator breakdown; empty for a bare
	// single-validator verdict, one entry per configured validator on a
	// chain verdict.
	Results []ValidatorResult `json:"results,omitempty"`
}

// Passed reports whether the verdict is a pass. A zero Verdict (no chain
// ran) is not a pass — callers distinguish "no chain" (nil verdict) from
// "chain passed" (Status == pass) before consulting this.
func (v Verdict) Passed() bool { return v.Status == StatusPass }

// PassVerdict builds a single-validator pass verdict with no issues.
func PassVerdict() Verdict {
	return Verdict{SchemaVersion: VerdictSchemaVersion, Status: StatusPass}
}

// FailVerdict builds a single-validator fail verdict carrying the given
// issues. The validator name is expected to already be stamped on each
// issue by the caller.
func FailVerdict(issues ...Issue) Verdict {
	return Verdict{SchemaVersion: VerdictSchemaVersion, Status: StatusFail, Issues: issues}
}

// Marshal renders the verdict as canonical JSON for persistence. It never
// fails on the fixed struct shape, but returns the error for the caller to
// route (a verdict is load-bearing evidence, unlike usage — ADR-013).
func (v Verdict) Marshal() (json.RawMessage, error) {
	return json.Marshal(v)
}
