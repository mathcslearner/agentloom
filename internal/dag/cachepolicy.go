package dag

import "time"

// This file is the definition-contract half of ADR-011: the optional
// per-step `cache` policy an author may carry on the step envelope, and
// the bound Validate enforces on it. The runtime half — the key builder,
// the eligibility/default policy, the Redis read-through/write-through
// middleware — lives in internal/cache and the engine (M9.4–9.6); this
// package only defines and validates the authored field.

// CacheMode selects how a step interacts with the response cache when the
// step's plugin is cache-eligible (ADR-011). An absent `cache` block means
// the engine's default policy decides (cache deterministic eligible steps,
// bypass the rest); an explicit block overrides that decision.
type CacheMode string

// The cache modes (ADR-011). A mode has no effect on a plugin whose
// capability flags make it cache-ineligible (side-effectful or
// non-cacheable): eligibility is a hard gate the mode cannot open.
const (
	// CacheOff never reads and never writes the cache for this step —
	// the way to opt a default-cached (deterministic, eligible) step out.
	CacheOff CacheMode = "off"

	// CacheReadWrite reads a cached result if present and writes the
	// step's result on a miss — the way to opt an eligible-but-not-
	// default-cached step in (a `temperature > 0` llm call, a retrieve).
	CacheReadWrite CacheMode = "read_write"

	// CacheReadOnly serves a cached result if present but never writes —
	// a canary/migration mode for validating a cache against live traffic
	// without populating it.
	CacheReadOnly CacheMode = "read_only"
)

// cacheModes enumerates the valid CacheMode values in documentation order.
var cacheModes = []CacheMode{CacheOff, CacheReadWrite, CacheReadOnly}

// CacheScope bounds which runs may share a cached entry (ADR-011). It is a
// component of the cache key: a wider scope shares more, a narrower scope
// isolates.
type CacheScope string

// The cache scopes (ADR-011). An absent scope means CacheGlobal — the
// canonical spelling of the default is no key.
const (
	// CacheGlobal shares a cached entry across every run of every
	// definition: the key is a pure function of the request. The default.
	CacheGlobal CacheScope = "global"

	// CacheRun isolates the entry to a single run — the run ID mixes into
	// the key — so a retried or resumed step of the same run reuses its
	// own result but no other run does. For steps whose inputs are
	// run-identical yet must not leak across runs.
	CacheRun CacheScope = "run"
)

// cacheScopes enumerates the valid CacheScope values in documentation order.
var cacheScopes = []CacheScope{CacheGlobal, CacheRun}

// MaxCacheTTL caps the step-envelope `cache.ttl` field (ADR-011). Same
// ceiling as the run wall-clock deadline: a cached AI output kept longer
// than a month is a staleness hazard, not a workload. The fleet default
// TTL (applied when a step opts in without a `ttl`) is 9.5 configuration,
// not a definition-contract bound.
const MaxCacheTTL = 30 * 24 * time.Hour

// CachePolicy is a step's authored response-cache policy (ADR-011); nil on
// the step envelope means the `cache` key was absent and the engine's
// default policy applies. Uniform across step types, so it lives on the
// step envelope, not in the per-type config — like retry and timeout.
type CachePolicy struct {
	// Mode selects read/write behavior; required when a `cache` block is
	// present (an empty block is meaningless — Validate rejects it).
	Mode CacheMode `json:"mode,omitempty"`

	// TTL is the cached entry's time-to-live, a Go duration string
	// ("1h", "30m"); empty means the key was absent (the engine's
	// configured default TTL applies at runtime, 9.5). Bounded by
	// MaxCacheTTL.
	TTL string `json:"ttl,omitempty"`

	// Scope bounds sharing; empty means the key was absent (engine
	// default: global). Encode omits the empty value, so the canonical
	// spelling of the default is no key.
	Scope CacheScope `json:"scope,omitempty"`
}
