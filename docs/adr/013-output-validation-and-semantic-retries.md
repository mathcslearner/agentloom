# ADR-013: Output validation & semantic retries

- **Status:** Accepted
- **Date:** 2026-08-15
- **Ticket:** ROADMAP.md ticket 11.1

<!--
This ADR opens M11 (output validation & semantic retries). 11.1 delivers the
validator SPI, the verdict model, the definition-contract chain config, and
the engine `validate` stage that persists a verdict per attempt and routes a
failing verdict to the `validation_failed` outcome. Later M11 tickets build on
it: 11.2 (built-in deterministic validators), 11.3 (JSON repair &
structured-output modes), 11.4 (the semantic-retry loop — feedback-augmented
re-attempts under their own counters), 11.5 (the llm_judge validator), 11.6
(quality metrics). Sections marked "(arrives in 11.x)" fix the contract now so
those tickets conform without re-litigating it.
-->

## Context

The engine's failure story through M10 is entirely *mechanical*: an attempt
either produced an output (success) or raised an error the ADR-006 taxonomy
classified (transient / permanent / timeout / cancelled / throttled /
budget_exceeded). But an AI step can *succeed at the transport level and still
produce the wrong thing* — malformed JSON where a schema was required, prose
that ignored the rubric, a plan that references nonexistent anchors. Temporal
and Airflow have no answer here: to them a 200 is a 200. This is the sharpest
contrast M11 draws, and the reason ADR-006 reserved a fifth error class,
`validation_failed`, "rejected everywhere until M11 wires the semantic-retry
loop".

The forces:

- **Validation must be a first-class, pluggable stage, not executor code.** A
  JSON-schema check, a regex, a CEL predicate, and an LLM judge are all
  "validate this output" with wildly different implementations; they must share
  one registration, one config-schema contract (ADR-009), and one verdict
  shape so the engine, the DLQ, the status API, and the M18 UI never
  re-derive "was this output acceptable?". ADR-009 already reserved kind
  `validator` for exactly this.

- **A validation failure is a genuinely different animal from a transport
  failure.** A transport failure retries the *same* request and heals on
  infrastructure luck; a validation failure must retry a *different* request
  (the semantic retry — feedback-augmented prompt) and is bounded by its own
  budget. Conflating the two counters would let provider flakiness consume the
  semantic budget and vice versa. ADR-006 reserved the outcome; this ADR fixes
  its routing and its separateness.

- **The interplay with cost (M10) and caching (M9) was pre-wired and must be
  honored.** M9 decided invalid outputs are never cached; M10 decided semantic
  attempts are cost-metered and judge overhead is attributed to the serving
  step. Those decisions constrain where the validate stage sits in the
  middleware chain and what a `validation_failed` attempt records.

- **The verdict must be durable and inspectable.** "Verdict persistence visible
  in run status API" is a 11.1 acceptance criterion: the operator (and 11.4's
  feedback-prompt builder) reads *why* an output was rejected — codes, paths,
  messages — off the attempt, not off a log line.

11.1 cannot be deferred behind the semantic-retry loop (11.4): the loop's
input *is* the verdict, so the verdict model, the SPI, and the persistence
have to exist and be conformed-to first — exactly as ADR-006 reserved the
class before ADR-013 spends it.

## Decision

### The validator SPI (kind `validator`, `internal/validate`)

We add a leaf package `internal/validate` — the fifth and final ADR-009
kind-owning package — structured exactly like `internal/tools`: it imports
`internal/plugin` (the manifest vocabulary), `internal/dag` (the ADR-006
error-class vocabulary), and stdlib/jsonschema, and **never** `internal/exec`
or the engine. A validator is:

```go
type Validator interface {
    Manifest() plugin.Manifest         // kind validator, unique name, semver, config schema
    Validate(ctx context.Context, in Input) (Verdict, error)
}
```

`Input` carries the resolved output under scrutiny (`Output` = the whole step
output; `Value` = the sub-tree the chain entry's `target` selected — see
below), the validator's own decoded `Config`, the 1-based semantic `Attempt`
(so a validator may loosen or tighten across attempts, 11.4), and a `Logger`.

A `Verdict` is the contract the whole feature turns on:

```go
type Verdict struct {
    SchemaVersion int              // 1
    Status        Status           // "pass" | "fail"
    Score         *float64         // optional [0,1]; chain score = min of reported
    Issues        []Issue          // codes/paths/messages — the feedback material
    Results       []ValidatorResult// per-validator: name, status, score, issue count, duration
}
type Issue struct {
    Validator string  // which validator raised it
    Code      string  // machine code (e.g. "type_mismatch", "rubric_below_threshold")
    Path      string  // RFC 6901 JSON pointer into the target, "" = whole value
    Message   string  // human description — structure only, never instance values
}
```

The `validate.Registry` is the typed facade over `plugin.Registry` (kind
`validator`), mirroring `tools.Registry`: it **compiles each validator's
config JSON Schema once at registration** (nil schema rejected — a validator
that takes no config declares `{}` explicitly) and exposes `ValidateConfig`,
so "bad validator config → permanent, no attempt" is a framework guarantee
enforced *before* any spend, exactly like a tool's args gate. Typed errors
mirror `tools`: `*validate.Error{Validator, Class}` (transient/permanent, the
only classes a validator may declare), `*UnknownValidatorError`,
`*ConfigValidationError`. Secret hygiene is structural — no error field can
hold an output payload, and ctx cancel/deadline pass through unwrapped so the
engine keeps the timeout/cancelled judgment.

11.1 ships the SPI with **no built-in validators** (`validate.NewBuiltins()`
returns an empty registry); 11.2 fills it with `json_schema`, `regex`,
`contains`, `cel`, `numeric_range`, and 11.5 adds `llm_judge`.

### Capability flags for validators

Validators carry the ADR-009 flags. The deterministic validators (11.2) are
`cacheable: true` (a pure function of output + config) and everything-else
false. The `llm_judge` (11.5) is `cost_bearing: true` (it calls a provider),
`cacheable: true`, and never `side_effectful`. The engine reads
`cost_bearing` to decide chain ordering (cheap validators first; see below)
and to attribute judge cost as **overhead** on the serving step (ADR-012 rule
4). No validator is ever `side_effectful` — a validator that mutated the world
would break re-validation on retry and on cache hit.

### The chain config on the step envelope

Validation is authored on the **step envelope**, uniform across step types —
like `retry`, `timeout`, `cache`, `budget`:

```jsonc
"validation": {
  "validators": [
    { "name": "json_schema", "config": { "schema": { ... } } },
    { "name": "cel",         "config": { "expr": "output.score > 0.5" }, "target": "/analysis" }
  ]
  // "max_attempts" and "feedback" (11.4) are reserved keys; strict decode
  // rejects them until 11.4 makes them admissible.
}
```

`internal/dag` defines `ValidationPolicy{Validators []ValidatorSpec}` and
`ValidatorSpec{Name, Config json.RawMessage, Target string}`, decoded
strictly (unknown keys rejected — including 11.4's future keys, the ADR-006
reservation discipline) and validated under two new codes,
`validation_field_required` / `validation_field_invalid`:

- at least one validator when a `validation` block is present (an empty block
  is meaningless — the whole field exists to declare a chain);
- each `name` matches the plugin-name shape `^[a-z][a-z0-9_]*$` (the *existence*
  of the named validator is checked at instantiation-adjacent claim time, not
  submit time — a definition may be authored against validators a given
  worker build has not registered, resolved at claim);
- each `target`, when present, is a syntactically valid RFC 6901 JSON pointer;
- the chain is at most `MaxValidators` (16) entries — a bound like every other
  envelope cap, so the worst accepted chain is finite.

Config *content* is not schema-checked at submit time (the validator's schema
is a runtime artifact, and submit-time validation cannot see a validator's
compiled schema — the tool-args precedent, ADR-009): a config that does not
match its validator's schema is caught at claim, pre-flight, as a permanent
failure.

### `target` and the default

`target` is an RFC 6901 JSON pointer selecting the sub-tree of the step output
a validator scrutinizes. Absent `target` means the whole output, **except**
for llm-family steps (`llm`, and the future `planner`/`agent`), whose output
is `{model, stop_reason, text, ...}`: there the default target is `/text`, so
`json_schema`/`regex`/`cel` validate the model's actual answer rather than the
envelope. A target that does not resolve in a given output is itself a `fail`
verdict with a structured issue (`code: "target_not_found"`), never a panic
and never a transport error — a missing target is a content problem the
semantic retry can fix.

### Chain semantics

Validators run in authored order. The engine runs **every** validator whose
`cost_bearing` flag is false (so 11.4 gets the complete critique — all issues,
not just the first — to build a feedback prompt), then, **only if every cheap
validator passed**, runs the `cost_bearing` validators (the llm_judge). The
rule is "cheap-first, expensive-only-on-otherwise-valid-output": there is no
point paying a judge to grade an output a free schema check already rejected.
The chain verdict is `fail` if any validator failed; its `Score` is the
minimum of the reported per-validator scores; its `Issues` are every failed
validator's issues concatenated in chain order. A validator that returns a
`*validate.Error` (transient/permanent) is not a `fail` verdict — it is a
transport failure of the validation stage itself (see the decision table);
11.5 adds a per-judge `on_error: skip | fail` policy for provider errors.

**Judges are terminal.** A validator's output is never itself validated or
judged — validators are not steps, they have no `validation` envelope, and the
chain never recurses. ADR-012 rule 4 already states judge overhead never
nests.

### The engine `validate` stage and where it sits

The executor middleware chain (ADR summary in CLAUDE.md) gains a `validate`
stage. Its placement is forced by the M9/M10 pre-wiring:

```
renderConfig → resolveChain → cacheRead → budget → rateLimit → execute → VALIDATE → cacheWrite → complete
```

- **`resolveChain` is pre-flight, before cacheRead and any spend.** Resolving
  the materialized `validation_policy` against the registry (unknown validator,
  config not matching its schema) is a deterministic permanent failure that
  must fire before money is spent — the tool-args gate, lifted to the chain.

- **`validate` runs after a successful execute and before cacheWrite**, because
  ADR-011 decided invalid outputs are never cached: an output that fails its
  chain never reaches `cacheWrite`.

- **A cache *hit* is re-validated too.** Validity is a property of the output
  *under the current chain*, and the cache key does not cover the validation
  envelope (a definition can tighten a validator without changing the request).
  So the cache-hit path runs the same `validate` stage before completing —
  a stored-but-now-invalid output is re-executed, not served. (A hit that
  passes still short-circuits the provider call.)

### The verdict is persisted per attempt

`step_attempts` gains a nullable `verdict JSONB` column (migration 0018, the
`usage` precedent — one JSON object, extensible without another migration).
NULL means no chain was configured (distinct from an empty pass verdict).
`GET /v1/runs/{id}` surfaces it as `attempts[].verdict`. The run's
materialized graph copy gains `run_steps.validation_policy JSONB` (NULL = no
chain), materialized at instantiation exactly like `cache_policy` — the claim
path reads it off the row and never reparses the snapshot.

### The `validation_failed` outcome and its routing

A chain `fail` verdict records the attempt outcome **`validation_failed`**
(migration 0018 widens the `step_attempts.outcome` CHECK and the
`dead_letters.class` CHECK; `validation_failed` is a *genuine* ADR-006 class,
unlike the administrative `lost`/`throttled`/`budget_exceeded`). The attempt
still records its `usage` and a `cost_ledger` row: the provider call happened
and billed, so the money is spent and metered (ADR-012 rule 5 amended — a
validation_failed attempt *has* usage, unlike a throttled/lost one).

11.1 routes a `validation_failed` verdict **terminal**: the step dead-letters
(source `retries_exhausted` — the semantic-retry budget, implicitly one
attempt in 11.1, is exhausted; 11.4 makes the budget configurable and inserts
the feedback-augmented re-attempt *before* this terminal step, changing
nothing recorded here), the verdict is stored on the attempt and the failure
summary references it, and the run's `on_failure` disposition applies
(fail_fast fails the run; continue_independent_branches writes off the
descendants) — the ordinary DLQ machinery, no new disposition logic.

`validation_failed` stays **rejected in a step's `retry_on`**: a validation
failure is *not* a mechanical retry, and admitting it there would conflate the
two counters. The transport-retry policy (`retry`) and the semantic-retry
policy (`validation.max_attempts`, 11.4) are separate fields with separate
budgets — this ADR overrides ADR-006's forward-looking note that M11 would
"unlock validation_failed in retry_on": it does not; it gives validation its
own policy instead. (ADR-006's table is amended accordingly.)

### The routing decision table (the 11.1 deliverable)

The completing worker faces four dispositions after an attempt; each is decided
by a different signal, counted against a different budget, and re-dispatched
under a different reason:

| Disposition | Trigger | Recorded outcome | Counter / budget | Re-attempt input | Re-dispatch reason |
|---|---|---|---|---|---|
| **Transport failure** | executor raised a classified error (ADR-006) | `transient` / `timeout` / `permanent` / `cancelled` | transport retry budget (`CountCountedFailures`: transient+timeout; `retry.max_attempts`) | **identical** request | `retry` (delayed) |
| **Throttle** (ADR-010) | rate-limit denial *before* execute | `throttled` (administrative) | none — budget-exempt by construction | identical request | `throttle` (delayed) |
| **Budget** (ADR-012) | claim-time projection over budget | `budget_exceeded` (administrative, park) *or* `permanent` (fail policy) | none (park) / transport (fail) | identical request | unpark / DLQ |
| **Validation failure** | executor succeeded, chain verdict `fail` | **`validation_failed`** (ADR-006 class) | **semantic** budget (`validation.max_attempts`, 11.4; implicitly 1 in 11.1) — *disjoint from transport* | **different** request (feedback-augmented prompt, 11.4) | `retry` (semantic, 11.4) / DLQ when exhausted |

The rows differ on the two axes that matter: **what the re-attempt sends**
(identical for transport/throttle/budget; different for validation) and **which
budget bounds it** (transport for retries, none for the administrative
outcomes, semantic for validation). That difference is the whole reason
validation is not "just another retry class".

### Interplay summary (pre-wired, now honored)

- **Invalid outputs are never cached** — `validate` sits before `cacheWrite`,
  and a `fail` verdict skips it (ADR-011).
- **Cache hits are re-validated** — validity depends on the current chain, not
  the key (ADR-011).
- **Semantic attempts are cost-metered** — a `validation_failed` attempt keeps
  its `usage` and ledgers a `cost_ledger` row (ADR-012 rule 5 amended).
- **Judge cost is overhead** — 11.5 attributes the judge's provider call to the
  serving step, flagged `overhead` (ADR-012 rule 4); 11.1 reserves nothing
  physical for it (the `cost_ledger.entry` discriminator from migration 0016
  already accommodates the overhead row with no schema change).
- **Judges are terminal** — never validated, never semantically retried; no
  recursion.

### Built-in deterministic validators (as built, 11.2)

11.2 fills the empty `NewBuiltins()` with the five deterministic validators,
all `cacheable`-only at `1.0.0` (pure functions of output + config), each in
its own file under `internal/validate`:

| Name | Config | Judges | Fail codes |
|---|---|---|---|
| `json_schema` | `{schema}` | the target as a JSON document (a **string** target is re-parsed as JSON — LLM JSON arrives as text) against a compiled JSON Schema | `invalid_json`, one per violating instance location (`type_mismatch`, `required`, `additional_properties`, `below_minimum`, `pattern_no_match`, …) with the RFC 6901 pointer in `Path` |
| `regex` | `{pattern, negate?}` | the target's string form (a JSON string → its contents; any other value → its compact JSON) against an RE2 pattern | `pattern_no_match` / `pattern_matched` |
| `contains` | `{substring, case_insensitive?, negate?}` | same string form, substring scan | `substring_missing` / `substring_present` |
| `cel` | `{expr, parse_json?}` | a CEL boolean predicate over `value` (targeted sub-tree), `output` (whole output), `step_type` | `predicate_false`, `cel_eval_error`, `invalid_json` |
| `numeric_range` | `{min?, max?, exclusive_min?, exclusive_max?}` | the target as a number (a numeric **string** like `"42"` is accepted; NaN/±Inf are not) against the bounds | `below_min`, `above_max`, `not_a_number` |

Two rules fell out of the target model:

- **String targets are content, not envelopes.** `regex`/`contains` stringify
  a non-string target rather than failing outright (grep-like semantics);
  `json_schema` re-parses a string target as JSON (its purpose is structural
  JSON, and the `/text` default hands it a JSON string); `cel` re-parses a
  string only under `parse_json: true`. A string that should be JSON but is not
  is an `invalid_json` **fail verdict**, never a panic — the ticket's explicit
  "malformed JSON → structured issue list" criterion.
- **Config errors are pre-flight; content errors are verdicts.** A validator
  whose config compiles into an artifact the config JSON Schema cannot fully
  vet — an unparseable regex, a CEL expression that will not typecheck or is
  not boolean, a JSON Schema that will not compile, a `numeric_range` with no
  bound or an inverted one — implements the new optional `ConfigCompiler`
  (`CompileConfig(config) error`). The registry calls it from `ValidateConfig`
  after the schema check, so a bad artifact is a permanent `*ConfigValidationError`
  fired at claim **before any spend**, exactly like a schema violation. A
  *runtime* failure on a particular output (a `cel` predicate referencing a
  field this output lacks) is by contrast a **fail verdict** (`cel_eval_error`),
  not a transport error — it is deterministic in the content and repairable by
  the semantic retry.

**Compiled artifacts are reused across attempts.** `json_schema`, `regex`, and
`cel` hold a `compileCache` keyed by the SHA-256 of the config bytes;
`CompileConfig` warms it and `Validate` reads it, so an artifact is compiled
**once per distinct config per worker process** and reused across every attempt,
retry, takeover, and run of the definition (materialized policy bytes are
byte-stable). Benchmarked: a warm `json_schema` validation is ~20× the speed of
a cold one that recompiles.

`NewBuiltins()` registers all five, so both `cmd/worker` (the validate stage)
and `cmd/api` (`GET /v1/plugins`) describe the same validators; the listing
carries each validator's generated config schema (UI-ready). No migration, no
config var, no metric, no engine change — the 11.1 stage runs the built-ins
unchanged.

### JSON repair & structured-output modes (as built, 11.3)

11.3 adds a repair pass ahead of the chain and provider-native structured
output, both authored on the llm step's config as an optional `output_format`
block (llm-only, like `model_fallbacks`):

```jsonc
"output_format": {
  "type": "json" | "json_schema",   // json = any JSON; json_schema enforces `schema`
  "schema": { ... },                 // required iff json_schema; a JSON object
  "mode": "auto" | "repair_only"     // auto (default) = native + repair; repair_only = repair only
}
```

Validation (`config_field_invalid` / `config_field_required`): `type` is
required and one of the two values; a `json_schema` type requires a
JSON-object `schema`; a plain `json` type must carry no schema; `mode` (when
present) is one of the two values. The schema's *compilability* is not checked
at submit — it is a runtime artifact (the tool-args / validator-config
precedent) checked at claim pre-flight by the implicit validator below.

**Three mechanisms, no change to the verdict shape or the routing table:**

1. **Deterministic JSON repair (`internal/jsonrepair`, a stdlib-only leaf).**
   `Repair(text)` runs a fixed sequence of conservative, cumulative passes —
   strip a Markdown code fence, extract the first balanced JSON value from
   surrounding prose, drop trailing commas, quote bare object keys — never
   touching string contents, short-circuiting the moment the working text
   parses. The result is `raw` (already valid), `repaired` (a pass fixed it,
   with the passes named), or `unrepairable`. The llm executor runs it over a
   completion's text when the step declared an `output_format` and the provider
   did not answer natively.

2. **Provider-native structured output.** `mode: auto` asks the provider for
   structured output through its native mechanism — Anthropic via a forced
   synthetic tool (`agentloom_structured_output`, `tool_choice` forced, the
   tool's input lifted onto `ChatResponse.Structured` and kept out of the
   user-visible tool calls); OpenAI via `response_format` (`json_object` for a
   plain json format, a strict named `json_schema` otherwise), a valid-JSON
   content lifted onto `Structured`; the mock echoes `{"echo": <text>}`
   natively under a `ResponseFormat` request. `mode: repair_only` leaves the
   provider request byte-identical to a plain-text step and only post-processes
   — the escape hatch for models whose native structured mode suppresses
   reasoning the workflow needs.

3. **An implicit `json_schema` validator enforces the declared shape.** At
   `resolveChain`, an llm step with an `output_format` gets a `json_schema`
   entry **prepended** to its authored chain (the author's schema for a
   `json_schema` type, an empty `{}` — parseability only — for a plain `json`
   type), targeting the `/text` default. Consequences: an unrepairable output
   is a proper `invalid_json` **fail verdict** (routing to `validation_failed`
   and, from 11.4, the semantic retry) rather than a silent success with a
   missing `json` field; an uncompilable schema is a permanent
   `*ConfigValidationError` at claim before any spend (11.2's `ConfigCompiler`);
   the author never duplicates the schema in `validation`. This reuses the
   whole 11.1/11.2 machinery unchanged — no new code inside `internal/validate`.

**The persisted output** gains a `json` field alongside `text` when a
structured-output step's output parsed (native or repaired), so
`${{ steps.<id>.output.json.field }}` flows structured data downstream; `text`
is overwritten with the canonical compact JSON so the `/text` default target
(the implicit and any author-supplied validator) sees the repaired structure.
An unrepairable output leaves `text` as the raw model output (so the verdict's
`invalid_json` issue reports the real problem) and omits `json`.

**Repair provenance** is recorded per attempt on `step_attempts.repair`
(migration 0019, the `usage`/`verdict` precedent — one nullable JSONB column,
no new outcome or class): `{schema_version, status, steps?, raw_text?}` where
status is `native | raw | repaired | unrepairable`. `raw_text` (the pre-repair
model text) is kept only on `repaired` so the repair is diffable. It rides
onto the attempt inside the completion transaction (both the success and the
validation_failed paths), is carried across a response-cache hit (the
`cacheEntry` gained a `repair` field), and is surfaced as `attempts[].repair`
in `GET /v1/runs/{id}` (`RepairProvenance` OpenAPI component).

**Cache key.** The `output_format` block is a cache-key component (appended to
`cache.LLMRequest` only when present, so a step with no format keys exactly as
it did pre-11.3 and existing entries stay valid): the same request yields a
different output under a different format (native vs. repaired vs. raw), and a
`repair_only` format changes the output without changing the provider request,
so the format must re-key the entry. No `KeySchemaVersion` bump — the trailing
component keeps every existing key stable (ADR-011 amended).

No new config var and no new metric (the repair-rate metric is 11.6). One
migration (0019), a single new leaf package (`internal/jsonrepair`), and no
new engine hook interface — the repair happens inside the llm executor and the
enforcement reuses the existing validate stage.

### Semantic retry engine (as built, 11.4)

11.4 activates the loop 11.1 reserved: a failing verdict no longer dead-letters
on the first attempt (unless the semantic budget is one) but rebuilds the
prompt from the critique and re-attempts, under a semantic counter disjoint
from the transport one. Nothing in the verdict shape, the routing table, or the
outcome vocabulary changes — the loop is inserted *before* the terminal
dead-letter the decision table already describes.

**Contract (`internal/dag`).** `ValidationPolicy` gains two fields, both
strict-decoded (the reserved keys 11.1 rejected are now admissible):

- `max_attempts` — the semantic budget, `1..MaxSemanticAttempts` (10; each
  attempt is a paid provider call, so the ceiling is far below the transport
  cap of 100). Absent (0) means one attempt — the exact 11.1 behavior every
  unset step keeps, so no existing definition changes meaning.
  `EffectiveMaxAttempts()` is the one site of the "absent means one" rule.
- `feedback` — a `FeedbackPolicy{template?, max_output_chars?}`. It is
  **llm-family-only** (a critique template on a step whose executor cannot
  inject it is an authoring mistake) and requires `max_attempts >= 2` (a
  critique nobody re-attempts with is meaningless — the
  model_fallbacks-without-budget precedent).

The feedback template is a **deliberately narrow surface**, not the 8.2
expression engine: `internal/dag/feedback.go` admits exactly four tokens
(`${{ feedback.prior_output }}`, `${{ feedback.issues }}`,
`${{ feedback.attempt }}`, `${{ feedback.max_attempts }}`), sharing one scanner
between submit-time validation (`CheckFeedbackTemplate`) and retry-time
rendering (`RenderFeedback`) so a template the definition accepts can never
reference a token the renderer does not know. A feedback fragment lives on the
envelope and is never step-config-rendered, so a fuller expression language
would be both unnecessary and a footgun. The default template is used when
`feedback` is nil (any llm step with `max_attempts >= 2`). One relaxation: a
`validation` block with **no validators** is admissible on an llm step that
declares an `output_format` (the implicit `json_schema` validator is the
chain), so `output_format` + `max_attempts` works without a dummy validator.

**Where the critique lives.** Migration 0020 adds two nullable JSONB columns
(the 0012/0018/0019 precedent — no new table, outcome, or class):

- `run_steps.feedback` — the critique the step's **next** attempt must carry.
  The validation-failure completion writes it; the claim CAS
  (`CreateStepAttempt`) copies it onto the new attempt and it stays on the step
  until success/DLQ clears it. Storing it on the step (not only the attempt) is
  what makes it survive a crash/takeover and an interleaved transport retry
  between semantic attempts.
- `step_attempts.feedback` — the critique **this** attempt was given, so the
  attempt history is a durable, diffable record of what each re-attempt was
  told.

The record is `exec.Feedback{schema_version, semantic_attempt, max_attempts,
prior_attempt, text}` — the `exec.Repair` precedent (exec owns the type, the
store treats it as opaque JSONB, so `finishAttempt`'s eight call sites are
untouched).

**Injection is an executor hook** mirroring `ModelDowngrader`:
`exec.FeedbackInjector{ WithFeedback(sc, Feedback) (config, error) }` on the llm
executor — a raw-message re-key that appends the critique to a prompt-form
config's `prompt` (blank-line separator) or as a trailing `user` message on a
messages-form config, leaving every sibling byte-identical. Injection happens
in `execute()` **after `resolveChain` and before `cacheRead`**, so the
augmented prompt feeds the cache key, the budget estimate, the limiter
estimate, and the provider call alike — a semantic retry is **cache-bypassed by
construction** (asserted at the exec layer, the `TestDowngradeChangesCacheKey`
precedent) and re-priced. A non-llm-family step (or an injector error, or empty
text) re-attempts with the un-augmented config — the convention `WithModel`
established: a binding failure proceeds rather than fails the attempt.

**The routing** (`completeValidationFailure`, now a router): inside the one
completion transaction, after the cancelling-run check, it reads
`CountValidationFailures` (validation_failed attempts past the requeue
baseline, the exact mirror of `CountCountedFailures`, so requeue re-arms both
budgets independently), computes `n = prior + 1` (this attempt's semantic
number), and branches:

- `n < max_attempts` → `store.SemanticRetryStep` records the failing attempt
  `validation_failed` (usage + verdict + repair persisted, cost ledgered),
  stamps the next critique onto `run_steps.feedback`, and re-dispatches
  **immediately through the transactional outbox** (reason `semantic_retry`,
  `next_attempt_at = now`) — not the delayed ZSET, because a semantic retry has
  no backoff rationale; a crash between commit and drain is healed by the same
  overdue-retrying reconciler scan (the row is `retrying`, due now). Post-commit
  it records `engine_step_retries_total{class="validation_failed"}` and nudges
  the dispatcher. The event is `step_semantic_retry_scheduled` (distinct from
  the transport `step_retry_scheduled`).
- `n >= max_attempts` (or a corrupt policy) → the terminal dead-letter 11.1
  already had, but the DLQ `error` payload now carries `semantic_attempts` and
  a `verdict_history` (`[{attempt_no, semantic_attempt, verdict}]` collected
  in-tx), so the exhausted-retry record is self-contained evidence.

**The two budgets are disjoint by construction and both visible.** The
transport budget (`CountCountedFailures`: transient+timeout) and the semantic
budget (`CountValidationFailures`: validation_failed) are separate queries over
separate outcome sets, so provider flakiness and bad outputs cannot consume
each other's budget. The run-status API surfaces both per step as
`transport_failures` / `validation_failures`, and each augmented prompt as
`attempts[].feedback`.

**Budget interplay** (the ticket's second criterion): the semantic re-attempt
is an ordinary claim, so the M10 budget check runs on it. If the augmented
attempt's projected spend exceeds the run budget the run **parks** — the
semantic loop halts mid-flight, the parked attempt records `budget_exceeded`
(never reaching the provider), and the pending feedback stays on the step;
`SetBudget` + `Unpark` re-dispatches the still-ready step, which carries the
critique into its resumed attempt. No new interplay logic — the loop simply
respects the existing middleware ordering.

One migration (0020), no new config var, no new metric (semantic-retry depth is
11.6), no new outcome or class. `MaxSemanticAttempts = 10`; the semantic policy
is materialized with the rest of `validation_policy` at instantiation, so the
claim path reads it off the row and never reparses the snapshot.

### LLM-judge validator (as built, 11.5)

11.5 fills the last built-in slot with the first **cost-bearing** validator,
`llm_judge` — a semantic quality gate whose failing rationale is exactly the
critique 11.4's loop folds back into the next attempt. It reuses the whole
11.1–11.4 substrate unchanged (verdict shape, routing table, chain semantics,
outcome vocabulary): **no migration, no new config var, no new metric, no new
outcome or class.**

**The validator (`internal/validate/llm_judge.go`).** `llm_judge` is
`cost_bearing: true` + `cacheable: true`, never `side_effectful` — so the
chain runs it **last** (cheap-first ordering: a judge grades only an output
the free validators already accepted, never one a schema check rejected), and
the engine attributes its provider call as **overhead** on the serving step.
Its config:

```jsonc
"config": {
  "model": "mock/judge-1",             // required; routed through the llm.Registry like an llm step
  "fallback_models": ["mock/judge-2"], // optional ordered availability chain, distinct from model + each other
  "rubric": "…grading criteria…",      // required, non-blank; static text (the envelope is never templated)
  "threshold": 0.7,                    // required, in [0,1]; pass iff score >= threshold
  "max_tokens": 512,                   // optional (default 512)
  "temperature": 0,                    // optional (default 0 — a deterministic grade)
  "timeout": "60s",                    // optional Go duration (default 60s, <= 10m — the judge bounds its own call)
  "on_error": "fail",                  // optional: fail (default) | skip
  "max_output_chars": 8000,            // optional; truncates the judged output in the prompt
  "max_rationale_chars": 2000          // optional; truncates the stored rationale
}
```

The `CompileConfig` pre-flight gate (11.2's `ConfigCompiler`) makes every
content error the config schema cannot express a **permanent config error at
claim, before the productive step spends a cent**: a blank rubric, an
out-of-range threshold, a duplicate/blank fallback, an unparseable or oversized
timeout, a bad `on_error`, and — crucially — a `model` (or any fallback) that
does **not route** against the registry (a nil registry, the keyless build,
routes nothing). The compiled artifact (parsed config + resolved timeout +
model chain) is cached by config bytes like the deterministic validators, so a
judge config compiles once per process.

**The judge call.** `Validate` builds a request with a fixed content-free
system prompt (grade against the rubric, answer **only** JSON
`{"score": <0..1>, "rationale": "…"}`), a user message pairing the rubric with
the delimited candidate output (`stringOf(in.Value)` — the `/text` default for
llm steps — truncated to `max_output_chars`), a `ResponseFormat` requesting
provider-native structured output (so a real Anthropic/OpenAI judge answers
natively), and `temperature: 0`. It calls the model chain in order under a
child `context.WithTimeout(judge timeout)`: a **provider error** (any class) or
the judge's own timeout falls through to the next model; a **caller-context**
cancellation/deadline passes through **unwrapped** so the engine keeps the
timeout/cancelled judgment (the `http_request` self-timeout-vs-engine-ctx
precedent). The answer is read from `ChatResponse.Structured` when present,
else from the completion text through the deterministic `jsonrepair` pass
(reusing 11.3) — a numeric `score ∈ [0,1]` and a string `rationale` are
required.

**The verdict.** `score >= threshold` → a **pass** verdict carrying the score
and one `Results[0]` with the `rationale` and the judge's `Usage`; below → a
**fail** verdict with a `rubric_below_threshold` issue whose message carries
the rationale (so `formatVerdictIssues` folds it into 11.4's feedback with no
change to the feedback builder) plus the same result detail. Two new optional
fields ride on `ValidatorResult` — `rationale` and `usage` (a `ValidatorUsage`
= resolved resource + served model + token counts) — plus `error` for the skip
case; the `Chain` runner lifts them from a validator's single-result verdict
into the chain verdict. No verdict-shape or migration change: these are
additive JSON fields on the `verdict` JSONB (the `StatusError` per-validator
status 11.1 already reserved is now produced by the skip path).

**`on_error` policy (the ticket's third criterion).** A judge that cannot
render a verdict is routed by the config:
- `fail` (default) → a classified **`*validate.Error`** (a transport failure of
  the validation stage, ADR-013's decision table): a provider availability
  failure is **transient** (a retry may reach a healthy provider), a malformed
  answer is **permanent** (the same model on the same output re-produces it).
  The engine routes it through the existing `completeFailure` path.
- `skip` → a **pass** verdict whose sole `Results[0]` records status `error`
  and the message (the chain does not fail; the workflow proceeds). An
  unavailable judge never blocks a workflow when the author opts into this.

**Cost attribution as overhead (ADR-012 rule 4, exercised now).** The judge's
provider call is metered on the **serving step's** attempt, flagged
`overhead: true`, under a `judge:<chain index>` `cost_ledger.entry` — the
same-attempt slot migration 0016 reserved, so two judges on one attempt never
collide on the `(run, step, attempt, entry)` PK, and no migration is needed.
The engine's pure `priceOverheads` reads the judge usage off the verdict's
per-validator results and prices each against the catalog (`PolicyEstimate` —
post-call pricing never fails a completed step; an unknown judge model is
priced at the fallback with a `cost_unknown_model` warning), applying the rows
inside the **same completion transaction** as the productive attempt on every
verdict-carrying route: `completeSuccess` (a cache **hit** re-judges, so its
overhead is real spend even though the productive row is a $0 saved),
`completeValidationFailure` (both the semantic-retry and the terminal DLQ
branch — a below-threshold judge is exactly what produced the spend), and (the
malformed-under-fail case) `completeFailure`. Each overhead row bumps
`runs.spent_nano_usd`, emits a `cost_updated` event, and records the
`engine_cost_spent_usd_total{resource="<judge model>"}` metric — so judge spend
counts against the run budget on the **next** claim's projection (park /
downgrade) and against a step `max_usd` cap (`SumByStep` sums all entries). The
cost API surfaces it as `entries[].overhead: true` with the `judge:*` entry,
and a new per-step `overhead_nano_usd` roll-up
(`AggregateCostByStep`) separating validation machinery from productive spend.

**The productive-spend metering fix (a pre-existing gap closed here).** Before
11.5, a step whose executor **succeeded and billed** but whose validation chain
then **errored** (an `llm_judge` under `on_error: fail`) dropped the productive
attempt's usage and cost — `completeFailure` never saw the output. 11.5 threads
the executor `exec.Output` into `completeFailure` (every mechanical-failure
caller passes `exec.Output{}`, so the change is a no-op there — an errored
provider call carries no usage): the failing attempt now records its usage
(`RetryStepArgs` gained a `Usage` field; `DeadLetterStepArgs` already had one)
and ledgers its productive cost on both the retry and DLQ branches, and the
judge's own billed call (a malformed answer that spent money before failing)
rides its usage on `*validate.Error.Usage` and is metered as a `judge:e`
overhead row. So **every** judge spend is now metered regardless of outcome
(pass, below-threshold fail, skip, malformed-under-fail); only a provider error
that billed nothing ledgers nothing.

**Guardrails.** Judges are **terminal** — a validator has no `validation`
envelope, the chain never recurses, and a judge is not a step (the run has one
step, not two), so a judge's output is never itself validated or judged and
overhead never nests. Two accepted limitations, documented rather than fixed
in 11.5: judge calls **bypass the M9 fleet rate limiter** (they are not routed
through the `ResourceClaimer` middleware — a heavy-judge fleet is not
back-pressured yet; a follow-up), and judge overhead is **not pre-projected at
claim** (it is metered after the fact, the same bounded-overshoot tolerance
concurrent fan-out already accepts). The rubric is **static** (the feedback and
rubric surfaces are deliberately un-templated); step-aware rubrics are backlog.

Wiring: `validate.NewBuiltins` gained a `*llm.Registry` parameter (the
`exec.Builtins` precedent) — `cmd/worker` passes the provider registry the llm
executor already builds, `cmd/api` passes the one it builds for the plugin
listing (so `GET /v1/plugins` describes `llm_judge` with its config schema even
though the API never runs it). One new leaf-free dependency (`internal/validate`
now imports `internal/llm` and `internal/jsonrepair`, no cycle). ADR-012 rule 4
and ADR-009's validator flag table are amended; ADR-010 gains the judge-bypass
note.

## Consequences

Positive:

- The engine's differentiator #1 has a durable, inspectable substrate: one
  verdict shape the SPI produces, the DLQ records, the status API serves, and
  11.4's feedback builder consumes — no re-derivation at any layer, exactly as
  ADR-006's one-vocabulary property does for error classes.
- The transport/validation counter separation is structural, not conventional:
  `CountCountedFailures` counts transient+timeout only (unchanged), and
  `validation_failed` is refused in `retry_on`, so neither budget can drain the
  other by construction.
- The middleware placement makes the M9/M10 pre-wiring pay off with no rework:
  invalid outputs skip the cache, semantic attempts meter, and the chain
  resolves before spend — all because `validate` sits where the earlier ADRs
  said an output check would sit.
- Every later M11 ticket is additive: 11.2 registers validators, 11.3 adds a
  repair pass ahead of the chain, 11.4 inserts the feedback re-attempt before
  the terminal DLQ, 11.5 adds a cost-bearing validator — none changes the
  verdict shape or the recorded outcome.

Negative:

- **A validation stage on every cost-bearing step is latency the pure-transport
  engine did not pay.** Mitigated by the cheap-first ordering (a judge runs
  only when the free validators pass) and by validators being nil-registry-safe
  (a step with no chain does zero work — `resolveChain` returns nil).
- **Re-validating cache hits costs the validators' work on a hit** (though not
  the provider call). This is the correct trade — serving a stored-but-invalid
  output silently corrupts the workflow — but it means a hit is not free when a
  chain is present.
- **11.1 dead-letters on the first bad verdict** (semantic budget of 1). Until
  11.4 lands, a validated step has no self-correction — it is stricter than the
  un-validated engine, not more forgiving. This is deliberate sequencing: the
  verdict and its persistence must be proven before the loop that consumes them.
- **The verdict is load-bearing state that can fail to marshal.** Unlike
  `usage` (which the success path drops with a warning on a marshal error), a
  verdict drop would lose the failure's evidence; the engine treats a verdict
  marshal failure on the failing path as a permanent step failure rather than a
  silent success.

## Alternatives considered

- **Validation as executor code (no SPI).** Each executor validates its own
  output. Rejected: it forbids reusing a JSON-schema check across step types,
  gives the UI no config-schema contract, and buries the verdict where the DLQ
  and status API cannot reach it uniformly — the exact anti-pattern ADR-009's
  kind vocabulary exists to prevent (it already reserved kind `validator`).

- **`validation_failed` as a retry class in `retry_on`.** ADR-006's
  forward-looking note suggested M11 would unlock it there. Rejected on
  implementation: a validation retry sends a *different* request under a
  *different* budget; folding it into `retry_on` would share one counter with
  transport retries and one backoff policy, so provider flakiness and bad
  outputs would consume each other's budgets. Separate fields, separate budgets
  — this ADR supersedes that note.

- **Validate before cacheWrite only, never re-validate cache hits.** Simpler
  and cheaper. Rejected: a definition can tighten a validator (or add one)
  without changing the request, so a previously-cached output that is now
  invalid would be served silently — the corruption ADR-011 warns about. The
  cache key deliberately does not cover the validation envelope, so the hit
  path must re-validate.

- **Run all validators unconditionally (no cheap-first ordering).** Simplest
  chain semantics. Rejected: it pays for a judge on outputs a free schema check
  already rejected — the exact waste ADR-012's overhead accounting is meant to
  make visible. Cheap-first collects the full free critique and gates the
  expensive validators on it.

- **Ship 11.1 as ADR + SPI only, defer all engine wiring to 11.4** (the
  9.1/9.4/10.1 "contract half" pattern). Rejected: the 11.1 acceptance
  criterion "verdict persistence visible in run status API" requires something
  to *write* a verdict, which requires the engine `validate` stage. 11.1 ships
  the stage (tested with in-test validators, since 11.2 has not shipped the
  built-ins) and routes a bad verdict terminally; 11.4 inserts the loop. The
  contract *and* its minimal producer land together, so the persistence is real
  and testable now.
