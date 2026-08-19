// The bounds the client validator enforces, mirroring the Go constants in
// internal/dag (validate.go, retry.go, validation.go, context.go,
// cachepolicy.go, expansion.go, blackboard). Each constant names its Go source
// so a bound change on the backend is a one-line change here; the parity test
// (test/parity.test.ts) fails on drift because a mis-set bound rejects (or
// accepts) a corpus fixture the backend does not.

// Document limits (dag.MaxSteps / MaxEdges / MaxNameLen / MaxExprLen).
export const MAX_STEPS = 10000;
export const MAX_EDGES = 20000;
export const MAX_NAME_LEN = 128;
export const MAX_EXPR_LEN = 1024;
export const MAX_LOOP_ITERATIONS = 100;

// Retry / timeout / wall-clock bounds (dag.retry.go), expressed in seconds so
// they compose with parseGoDuration.
export const MAX_RETRY_ATTEMPTS = 100;
export const MAX_BACKOFF_INITIAL_SECONDS = 3600; // 1h
export const MAX_BACKOFF_CAP_SECONDS = 24 * 3600; // 24h
export const MAX_BACKOFF_MULTIPLIER = 100;
export const MAX_STEP_TIMEOUT_SECONDS = 24 * 3600; // 24h
export const MAX_RUN_WALL_CLOCK_SECONDS = 30 * 24 * 3600; // 30d
export const MAX_APPROVAL_TIMEOUT_SECONDS = 30 * 24 * 3600; // 30d (dag.MaxApprovalTimeout)
export const MAX_CACHE_TTL_SECONDS = 30 * 24 * 3600; // 30d (dag.MaxCacheTTL)
export const MAX_SUMMARY_TIMEOUT_SECONDS = 10 * 60; // 10m (dag.MaxSummaryTimeout)

// Validation / context bounds.
export const MAX_VALIDATORS = 16; // dag.MaxValidators
export const MAX_SEMANTIC_ATTEMPTS = 10; // dag.MaxSemanticAttempts
export const MAX_CONTEXT_SOURCES = 32; // dag.MaxContextSources
export const MAX_CONTEXT_TOP_K = 100; // dag.MaxContextTopK
export const MAX_COMPACTION_STRATEGIES = 8; // dag.MaxCompactionStrategies

// Blackboard grammar bounds (internal/blackboard).
export const MAX_BLACKBOARD_WRITES = 16; // dag.MaxBlackboardWrites
export const MAX_BLACKBOARD_TAGS = 32; // blackboard.MaxTags
export const MAX_BLACKBOARD_KEY_LEN = 128; // blackboard.MaxKeyLen
export const MAX_BLACKBOARD_TAG_LEN = 64; // blackboard.MaxTagLen

// Expansion defaults (dag.expansion.go). max_total_steps defaults to MAX_STEPS.
export const DEFAULT_MAX_ADDED_STEPS_PER_EXPANSION = 32;
export const DEFAULT_MAX_TOTAL_STEPS = MAX_STEPS;

// The supported definition schema version (dag.SchemaVersion).
export const SCHEMA_VERSION = 1;

// stepIDRe / pluginNameRe / instanceStepIDRe — the id and name grammars
// (validate.go, validation.go). Kept verbatim so a syntactically-invalid id
// names the same problem client- and server-side.
export const STEP_ID_RE = /^[a-z][a-z0-9_-]{0,63}$/;
export const PLUGIN_NAME_RE = /^[a-z][a-z0-9_]*$/;
export const INSTANCE_STEP_ID_RE = /^[a-z][a-z0-9_-]{0,63}(#[a-z0-9_]+)+$/;

// The stable string forms the Go error messages use for the id/name regexes,
// so a client message reads like the backend's ("does not match ...").
export const STEP_ID_RE_TEXT = "^[a-z][a-z0-9_-]{0,63}$";
export const PLUGIN_NAME_RE_TEXT = "^[a-z][a-z0-9_]*$";
