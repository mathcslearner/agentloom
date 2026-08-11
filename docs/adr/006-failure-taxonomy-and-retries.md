# ADR-006: Failure taxonomy & retry semantics

- **Status:** Accepted
- **Date:** 2026-08-11
- **Ticket:** ROADMAP.md ticket 5.1

## Context

M4 ended with the bluntest possible failure semantics: any executor error
lands a claim-fenced `FailStep`, and a failed step immediately fails its
run (4.3's "v1-minimum rollup"). Three earlier ADRs left deliberate holes
that all point here:

- **ADR-003** decreed that CEL evaluation errors are step failures, "how
  that failure is then classified and retried is owned by ADR-006", and
  reserved an additive per-step `retry` field. Its join semantics defer
  "whether a failed parent fails the run immediately or lets independent
  branches finish" to the workflow failure policy defined here.
- **ADR-004** reserved the step statuses `retrying`, `dead_lettered`,
  `cancelled` and the run statuses `parked`/`cancelling`/`cancelled`, put
  the M5 rows in its transition matrix, and left `step_attempts.outcome`
  CHECK-less because "the full taxonomy — `timeout`, `cancelled`,
  `validation_failed` — is ADR-006's".
- **ADR-005** built the transport half: poison detection via delivery
  count with a callback M5.4 wires to dead-lettering, a delayed-delivery
  ZSET with no production tenant yet (retry re-dispatch is its intended
  first), and a `reason` vocabulary anticipating `retry re-dispatch
  M5.2, DLQ requeue M5.4`.

The forces in tension:

- **Not every failure deserves a retry.** A rate-limited LLM call heals
  with backoff; a nonexistent model name never does. Retrying permanent
  failures wastes budget (real dollars, for LLM steps) and delays the
  truth; failing fast on transient blips makes the engine fragile
  exactly where an AI workload is flakiest.
- **Two retry-ish mechanisms already exist and must not tangle.** The
  queue redelivers (crash recovery, at-least-once transport) and counts
  deliveries toward a poison threshold; the retry engine re-attempts
  (failure recovery, policy-driven). If one consumed the other's budget,
  a flaky host could exhaust a step's retries with zero recorded
  failures — or a crash-looping handler could retry forever.
- **Semantic retries (M11) need a hook, not an implementation.** The
  taxonomy must reserve `validation_failed` — retry with a
  critique-augmented prompt — without letting anyone use it early and
  bake in wrong semantics.
- **A failed branch should not always doom a run.** Fan-out workflows
  (research N sources, summarize the ones that worked) want partial
  results; approval pipelines want fail-fast. Both are legitimate, so it
  is a per-workflow policy, not an engine constant.
- **The definition format is a strict, versioned contract** (ADR-003):
  the retry policy must land as typed, validated, additively-introduced
  fields — with bounds, so a typo'd `max_attempts: 1000000` is rejected
  at submit time, not discovered as an infinite-looking retry storm.

M5.2–5.6 implement directly against this ADR; deferring it would mean
implementing retry mechanics against guessed semantics.

## Decision

### Error classes

Every recorded attempt failure carries exactly one **error class**:

| Class | Meaning | Retryable |
|---|---|---|
| `transient` | The attempt failed for a reason that can plausibly heal on its own: network error, provider 5xx/429, timeout of a dependency, resource contention. | yes (default) |
| `permanent` | No number of identical re-executions can succeed: unknown executor type, invalid/corrupt config, a deterministic content failure (CEL evaluation error, unparseable stored state), provider 4xx that is not a rate limit. | never |
| `timeout` | The attempt exceeded its configured execution timeout (M5.3) and was cancelled by the worker's watchdog — the worker is **alive to report it**, which is what distinguishes a timeout from a crash. | yes (default) |
| `cancelled` | The attempt was cancelled by run-level control flow: run cancel, park, worker drain handing back work (M5.6/5.7). Not a failure of the step's content. | never |
| `validation_failed` | **Reserved for M11** (ADR-013): the executor succeeded but output validation rejected the result; the semantic-retry loop re-attempts with a critique-augmented prompt. Until M11 this class is rejected everywhere it could be written or referenced. | M11 |

Classes are attempt **outcomes**: `step_attempts.outcome` gains the
vocabulary `succeeded | transient | permanent | timeout | cancelled`
(plus the pre-existing administrative `lost`, which stays deliberately
*outside* the taxonomy — it records a lease-expiry takeover closing a
dead holder's dangling attempt, not a judged failure). M5.2's migration
adds the CHECK constraint ADR-004 postponed. The bare outcome `failed`
(written by 2.6–4.x) is retired by that migration in favor of the
classed vocabulary; existing rows are backfilled to `permanent` (under
pre-M5 semantics every failure was terminal, which is what `permanent`
now means).

### Classification: where a class comes from

Classification is decided **worker-side, at completion time**, by a
pure function over the error, and recorded durably on the attempt. The
mechanism (implemented in 5.2):

- `internal/exec` exports a typed wrapper — an `ErrorClass` (aliasing
  the dag package's contract type, below) and a `ClassifiedError` that
  executors return when they know better than the default
  (`exec.Permanentf(...)`, `exec.Transientf(...)`). `errors.As`
  unwraps it anywhere in the chain.
- **Unclassified executor errors default to `transient`.** A generic
  error is far more often a flaky dependency than a deterministic bug,
  idempotency keys and the side-effect journal (5.5) make re-execution
  safe, and the cost of misclassifying a permanent error as transient
  is bounded (a policy's worth of futile attempts) while the reverse —
  failing a run on a network blip — is exactly the fragility this
  milestone exists to remove. Executors that *know* an error is
  permanent must say so.
- The engine assigns `timeout` and `cancelled` itself from context
  state (watchdog deadline vs. run-control cancellation, M5.3/5.6);
  executors surface `ctx.Err()` and do not classify it.
- A short list of engine-internal error shapes is force-classified
  `permanent` regardless of wrapping, since they are deterministic
  content failures (taxonomy table below).

**The taxonomy table.** Every failure path that exists in the engine
today (4.x), with its class and disposition. "Retry per policy" means
the retry engine (5.2) consults the step's effective policy; "DLQ"
means the dead-letter path (5.4).

| # | Path (where it surfaces) | Class | Disposition |
|---|---|---|---|
| 1 | Executor returns an unclassified error (`Engine.execute`) | `transient` | retry per policy; exhausted → DLQ |
| 2 | Executor returns a `ClassifiedError` | as declared | per class: `transient` → row 1; `permanent` → row 4 |
| 3 | Executor context cancelled at the step-timeout deadline (M5.3 watchdog) | `timeout` | retry per policy (counts against the budget); exhausted → DLQ |
| 4 | `exec.ErrUnknownType` — no executor registered for the step type (`Engine.execute`) | `permanent` | no retry; DLQ |
| 5 | `exec.ErrInvalidConfig` — executor-side config decode failure | `permanent` | no retry; DLQ |
| 6 | CEL edge-predicate failure at completion — compile error on stored expression, evaluation error, non-bool result (`planEdges`; ADR-003 "evaluation errors are failures") | `permanent` | no retry; DLQ. The class attaches to the *completing* step's failure |
| 7 | Corrupt stored content discovered mid-completion — run params that no longer decode, a join target's config failing `dag.DecodeStepConfig` (`completeSuccess`, post-M4 audit routing) | `permanent` | no retry; DLQ |
| 8 | Executor context cancelled by run cancel / park / drain (M5.6/5.7) | `cancelled` | no retry, no DLQ; step follows the run-control transition (→ `cancelled`, or back to `ready` on drain) |
| 9 | Handler panic (`recover` in the consumer loop) | *none recorded* | no ACK; redelivery → takeover re-executes; the delivery count walks a deterministic panic to the poison threshold → DLQ (source `poison`) |
| 10 | Worker crash / SIGKILL mid-execution | `lost` (administrative, on takeover) | fresh attempt via lease-expiry takeover (ADR-005); **does not consume retry budget** (below) |
| 11 | Claim/completion transaction transport failure (`WithTx` error, Postgres down) | *none recorded* | no ACK; redeliver (nothing was decided) — ADR-005's discipline unchanged |
| 12 | Fenced completion — terminal CAS rejected on `claim_id` (`abandonFenced`) | *none recorded by the fenced worker* | abandon without ACK (ADR-005/4.5); the new holder's own completion records the real outcome |
| 13 | Malformed / unknown-version envelope (consumer decode) | *none* | no ACK; delivery count → poison → DLQ (source `poison`) |
| 14 | `ResolveEdge` graph-integrity error mid-completion (counter/resolution corruption) | *none* | deliberate redeliver-to-poison (post-M4 audit: bookkeeping corruption is not step content; a `FailStep` over the same corrupt rows is no safer) → DLQ (source `poison`) |

Rows 4–7 are the force-classified `permanent` set: they are
deterministic functions of stored state, so re-execution is provably
futile — 4.x already routes them to a real failure completion instead
of a redelivery loop, and 5.2 only changes what gets recorded, not the
routing.

**Retry budget vs. delivery count.** The two counters guard different
failure modes and never mix:

- The **retry budget** (`max_attempts`) counts *judged* attempt
  failures — outcomes `transient` and `timeout`. `lost` attempts do not
  count: a crashed host recorded no judgment about the step, and
  letting infrastructure churn silently eat a step's budget would make
  retry behavior depend on where the step happened to run.
- The **delivery count** (poison threshold, ADR-005) bounds
  crash-loops: a step that keeps killing its workers (row 9/10
  repeating) never records a failure, but its entry's delivery count
  rises through reclaims and lands it in the DLQ with source `poison`.

Both bounds are always live; whichever trips first wins.

### Retry policy: schema, defaults, bounds

The definition format (ADR-003) gains the reserved per-step `retry`
field — a sibling of `config`, uniform across all step types:

```json
{"id": "summarize", "type": "llm",
 "config": {"model": "anthropic/claude-sonnet-5", "prompt": "..."},
 "retry": {
   "max_attempts": 3,
   "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2},
   "jitter": "full",
   "retry_on": ["transient", "timeout"]
 }}
```

Semantics:

- **`max_attempts`** — total execution attempts, counting the first
  (`1` = no retries). The budget counts attempts with outcomes
  `transient`/`timeout` plus the in-flight one; `lost` closures are
  excluded (above).
- **`backoff`** — exponential: the delay before budget-eligible retry
  *n* (1-based: the delay after the *n*-th counted failure) is
  `min(cap, initial × multiplier^(n−1))`.
- **`jitter`** — `full` (default): the actual delay is drawn uniformly
  from `[0, computed]` (AWS full jitter — best de-correlation for
  thundering herds of fanned-out siblings retrying against one rate
  limit); `none`: the computed delay exactly (tests, and workloads
  where predictability beats de-correlation).
- **`retry_on`** — which error classes are retryable for this step.
  Only `transient` and `timeout` may appear; `permanent` and
  `cancelled` are never retryable by construction, and
  `validation_failed` is rejected until M11 wires the semantic-retry
  loop (at which point it becomes admissible here — that is the hook).
  A class outside the step's `retry_on` is treated as permanent-for-
  this-step: straight to the DLQ path.

**Engine defaults** (applied field-wise when `retry` or any of its
fields is absent — an explicit `retry` block overrides only what it
sets):

| Field | Default | Rationale |
|---|---|---|
| `max_attempts` | `3` | Two retries absorb the common transient blip without turning a permanent-misclassified error into a storm |
| `backoff.initial` | `1s` | Above typical connection-reset recovery; below human perception of "stuck" |
| `backoff.cap` | `1m` | A step that has not healed after a minute of backoff is waiting for an incident, not a blip; longer caps belong to explicit policies |
| `backoff.multiplier` | `2` | Standard doubling |
| `jitter` | `full` | De-correlation is the safe default for fan-out siblings |
| `retry_on` | `["transient", "timeout"]` | Everything retryable is retried by default |

**Validation bounds** (enforced at definition validation, 1.3-style
path-qualified issues; new codes `retry_field_required` /
`retry_field_invalid`):

| Field | Bound |
|---|---|
| `max_attempts` | integer, 1 ≤ n ≤ 100 (absent key = engine default, matching the `max_iterations` absent-vs-zero convention) |
| `backoff.initial` | required when `backoff` present; positive Go duration string, ≤ 1h |
| `backoff.cap` | **required when `backoff` present** — an uncapped exponential is a latent multi-hour stall; positive Go duration string, ≥ `initial`, ≤ 24h |
| `backoff.multiplier` | 1 ≤ m ≤ 100 (absent = default 2; 1 = constant backoff) |
| `jitter` | enum `full` \| `none` (decode-time check, like `join.mode`) |
| `retry_on` | non-empty when present; entries from `{transient, timeout}`; duplicates rejected; `permanent`/`cancelled`/`validation_failed` rejected with a message naming why (never-retryable / reserved for M11) |

Durations are Go duration strings validated for parseability at
definition validation, exactly like `sleep.duration` — a definition the
engine accepts must not explode mid-run on a malformed literal.

**Where the policy lives at runtime.** 5.2 materializes the *effective*
policy (explicit fields merged over engine defaults) onto the
`run_steps` row at instantiation, exactly as `config` is materialized:
the failure path must not reparse the definition snapshot, and a run's
retry behavior must be frozen at submit time (per-run snapshots are the
compatibility surface, ADR-001). Runtime-injected steps (M13
expansion) inherit the same materialization path.

### Step failure lifecycle

`failed` changes from a terminal state to a **routing state** the
completion transaction passes through; the terminal failure state
becomes `dead_lettered`. Per ADR-004's reserved matrix rows:

```
running ──(attempt fails, class recorded)──▶ failed
failed ──(policy admits another attempt)──▶ retrying ──(delayed promotion)──▶ ready
failed ──(budget exhausted / class not retryable)──▶ dead_lettered
```

The transition out of `failed` happens **in the same completion
transaction** that records the failed attempt — `failed` is never left
resting across transactions (a crash mid-transaction rolls back to
`running` and the delivery redelivers; ADR-005's discipline). The
retry delay is served by the delayed-delivery ZSET (ADR-005 3.5 — its
first production tenant), the delayed envelope carrying the new reason
`retry` added to ADR-005's vocabulary; a crash between the failure
commit and the delayed-schedule is healed by the reconciler (5.2
extends the staleness sweep to `retrying` steps whose due time is long
past with no pending outbox row, the same anti-join shape as
`reconcile_ready`).

*As built (5.2):* both hops through transient states are realized as
single CASes — the routing state `failed` is passed through inside the
completion transaction as a direct `running → retrying` CAS, and the
`retrying → ready` hop collapses into the claim (delayed promotion is a
pure Redis event with no Postgres write): the claim CAS accepts a
`retrying` step once `next_attempt_at ≤ now`, which is also what makes
backoff enforceable against early duplicate deliveries. `next_attempt_at`
is stamped durably on the step row by the retry transaction — the state
the reconciler heals a lost delayed entry from (ADR-005 crash cell P3).
ADR-004's transition matrix records the as-built rows.

### Dead-letter model

**Postgres is the DLQ**, not a Redis stream: the queue is redeliverable
transport (project invariant), and dead-letter records must survive
anything Redis can lose, join against attempt history, and support
audited requeue. Concretely (5.4's migration):

- Step status **`dead_lettered`** — the terminal failure state.
- A **`dead_letters`** table: one row per dead-lettering, keyed to
  `(run_id, step_id)` plus a monotonic sequence (a step can die, be
  requeued, and die again), carrying the **source** —
  `retries_exhausted | permanent | poison` — the final error payload,
  the class, the attempt count at death, and for poison entries the
  raw envelope contents (ADR-005 preserves them for exactly this).
- The queue's poison callback (3.4's `PoisonHandler`) is wired to
  dead-letter the step (source `poison`) and then ACK — the one ACK
  that happens on a failure path, because the DLQ row *is* the durable
  consumption of the message.
- Events: `step_dead_lettered` (payload: source, class, attempts),
  joining the free-form event vocabulary.

**Requeue** (internal op in 5.4; API in M6.5) is the guarded CAS
`dead_lettered → ready` plus an outbox row with reason `dlq_requeue`.
Attempt history is immutable — `attempt_count` never resets (attempt
rows are keyed by `attempt_no`). Instead, the requeue records the
attempt baseline (the `dead_letters` row already carries
`attempts_at_death`), and the retry budget after requeue counts
attempts *since that baseline*. Requeue re-arms the step with its full
policy, deliberately: an operator requeues because something changed.

### Run disposition: the workflow failure policy

A new optional top-level definition field:

```json
{"schema_version": 1, "name": "...", "on_failure": "fail_fast", ...}
```

- **`fail_fast`** (default, matching 4.3's behavior): the transaction
  that dead-letters a step also transitions the run `running → failed`.
  In-flight sibling attempts complete and record normally (their
  completions land against an already-failed run; the rollup conflict
  is dropped, as today), but the dispatcher stops dispatching for
  failed runs and the claim path refuses steps of terminal runs (5.2
  adds the run-status guard to `ClaimStep` — the mechanism 5.6's
  park/cancel also needs).
- **`continue_independent_branches`**: the run stays `running`; only
  steps *downstream* of the dead-lettered step are written off, so
  independent branches finish and deliver partial results. The
  dead-lettering transaction resolves the consequences eagerly —
  ADR-004's "failed parent permanently blocks" becomes a real
  transition instead of a permanent limbo: every step whose readiness
  is now impossible (all paths from a fired-or-pending frontier pass
  through the dead step; computed by the same worklist walk as skip
  propagation, over the dead step's out-edges) transitions
  `pending → cancelled` with event `step_cancelled` (payload reason
  `upstream_dead_lettered`). `join any` targets survive so long as any
  live parent can still fire (their guard ignores `remaining_deps`,
  so they are simply *not* written off unless every parent is
  dead-or-cancelled with none fired). The run terminalizes when every
  step is terminal — the existing rollup guard — landing `failed`
  whenever any step is `dead_lettered` (partial success is still a
  failed run; the per-step statuses and outputs carry the nuance).

Eager write-off is chosen over lazy ("run ends when no runnable work
remains") because it keeps the rollup a counter check, gives the UI an
honest per-step answer immediately, and reuses machinery (worklist
propagation in the completing transaction) that 4.3 already proved.
The `cancelled` step status arrives with 5.4's migration rather than
waiting for 5.6 — it is the same status 5.6's run-cancel sweep writes,
with a different event reason.

`on_failure` is decoded strictly (unknown values rejected at decode
time, like enum fields everywhere), omitted from canonical encoding
when absent, and absent means `fail_fast`.

### Enforcement points

**5.1** (this ticket) — the schema: `retry` on steps, `on_failure`
top-level, validation with the bounds table, JSON Schema regenerated,
fixtures carrying explicit policies. **5.2** — classification mechanism
in `internal/exec`, attempt-outcome vocabulary + CHECK migration,
`retrying` status + transitions, backoff computation, delayed-ZSET
scheduling, reason `retry`, run-status guard on `ClaimStep`,
reconciler coverage for the failure-commit/schedule gap. **5.3** — the
`timeout` class assignment and watchdog. **5.4** — `dead_lettered` +
`cancelled` statuses, `dead_letters` table, poison wiring, requeue op
with reason `dlq_requeue`, the failure-policy run disposition and
downstream write-off. **5.6** — the `cancelled` class for run-control
cancellation. **M11** — unlocks `validation_failed` in `retry_on` and
as an outcome. ADR-004's transition matrix and ADR-005's reason
vocabulary are extended by those tickets exactly as both ADRs
anticipated; no prior decision changes.

## Consequences

Positive:

- Transient infrastructure noise stops failing runs; the flakiest
  dependency class an AI workload has (provider rate limits, tool-call
  timeouts) is absorbed by policy instead of by luck.
- One vocabulary end to end: the class recorded on the attempt is the
  class the retry engine consults, the DLQ records, the UI displays,
  and M11 extends — no re-derivation at each layer.
- Crash recovery and failure recovery stay orthogonal: `lost` outside
  the budget and poison outside the taxonomy means neither mechanism
  can starve or mask the other, and each has a hard bound.
- Fan-out workflows get partial results without giving up fail-fast
  pipelines — per workflow, one field.
- The strict-contract properties (ADR-003) extend to failure handling:
  a typo'd policy is a path-qualified submit-time error, and the bounds
  make the worst accepted policy finite (100 attempts, 24h cap).
- `validation_failed` reserved-but-rejected means M11 gets its hook
  with zero risk of early misuse baking in wrong semantics.

Negative:

- **Default-transient retries some hopeless errors.** A deterministic
  executor bug burns a policy's worth of attempts (and LLM dollars)
  before dead-lettering. Bounded by the default budget of 3, and the
  alternative — default-permanent — fails runs on every unwrapped
  network error, which is worse.
- **`failed` stops being terminal**, which every consumer of step
  status must now know (the 4.x-era reading of `failed` as final is
  retired by 5.2's migration; the API's status surface changes shape).
- **Eager downstream write-off duplicates reachability logic** in the
  dead-lettering transaction (a blocked-descendant walk alongside skip
  propagation). Lazy evaluation would avoid it at the cost of an
  unbounded "run ends when nothing can move" predicate on the hot
  rollup path.
- **Requeue-counts-from-baseline** makes "attempts used" a derived
  quantity (attempt count minus baseline) rather than a column read —
  slightly more bookkeeping in exchange for immutable attempt history.
- **Per-field default merging** means a definition with a bare
  `"retry": {"max_attempts": 5}` silently inherits four engine
  defaults; a reader must know the defaults table. The alternative —
  all-or-nothing blocks — punishes the common "just give me more
  attempts" case.

## Alternatives considered

- **Redis stream as the DLQ** (a `dead` stream, XADD on poison).
  Rejected: violates "Postgres is the source of truth; any Redis data
  loss must be recoverable" — dead-letter records are exactly the data
  an operator consults *after* something went wrong, the worst time to
  discover transport loss. The queue already has its poison mechanism;
  durability belongs in the store.
- **Default-permanent for unclassified errors** (retry only what is
  explicitly marked transient). Safer against wasted attempts, but it
  fails runs on every unwrapped network hiccup and demands perfect
  classification discipline from every executor author from day one —
  the failure mode is silent fragility, discovered in production.
- **HTTP-status-style numeric codes** (or free-form strings) instead of
  a closed class enum. More expressive, but the retry engine only ever
  branches on retryability-shaped questions; a closed enum keeps
  `retry_on` validatable and the attempt CHECK constraint meaningful.
  Provider-specific detail lives in the error payload, not the class.
- **Counting `lost` attempts against the retry budget.** Simpler (one
  counter), but couples a step's failure budget to infrastructure
  churn: two host OOMs plus one real transient failure would exhaust a
  3-attempt policy having judged the step's content exactly once. The
  poison threshold already bounds crash-loops.
- **Retry policy resolved at execution time from the definition
  snapshot** (no materialization onto `run_steps`). Avoids a column,
  but puts a JSONB parse of the whole definition on every failure path
  and — worse — re-resolves engine defaults at execution time, so a
  worker upgrade could change the effective policy of an in-flight run.
  Materialization at instantiation freezes semantics with the run.
- **Lazy run termination under `continue_independent_branches`** (run
  ends when no step is runnable). Avoids the downstream write-off walk,
  but "no runnable work" is a whole-graph predicate the rollup would
  evaluate on every completion, blocked steps sit in `pending` forever
  (indistinguishable from not-yet-ready in every view), and the UI
  cannot tell a user their step will never run until the run ends.
- **Per-class backoff schedules** (different curves for `timeout` vs
  `transient`). Deferred, not rejected: nothing in the schema precludes
  adding per-class overrides later; no current workload justifies the
  authoring surface.
- **Jitter modes beyond `full`/`none`** (equal jitter, decorrelated
  jitter). The AWS analysis shows full jitter within a few percent of
  optimal for contended retries; more knobs would be speculative. The
  enum admits additive growth.
