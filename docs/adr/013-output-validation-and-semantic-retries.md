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
