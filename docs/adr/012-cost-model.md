# ADR-012: Cost model — attribution, estimation & the pricing catalog

- **Status:** Accepted
- **Date:** 2026-08-14
- **Ticket:** ROADMAP.md ticket 10.1

## Context

Cost awareness is differentiator #3, and the roadmap is explicit that it must
sit *inside* the scheduler path — checked at claim time, enforced by parking a
run or downgrading a model — not bolted on as a dashboard that reports spend
after the money is gone. That framing forces several decisions up front, before
the ledger (10.2) or the claim-time budget check (10.3) can be built:

- **What a cost *is*, precisely enough to sum without drift.** A run's spend is
  the sum of its attempts' costs, and 10.2 must prove that a run aggregate
  equals the exact sum of its ledger rows even under concurrent completions.
  Floating-point dollars do not sum exactly; the representation has to be
  decided here.
- **Where a price *comes from*.** Providers publish $/1M-token list prices that
  change over time and differ per model. A run priced today and re-examined next
  month must keep the price that was in effect when it ran, so pricing data is
  versioned and effective-dated, with embedded defaults an operator can
  override without editing the binary.
- **How usage maps to cost — the attribution rules.** An LLM attempt is
  tokens × rate, but the fleet also runs priced tools, serves cache hits (which
  cost $0 but must record what they *would* have cost, for the "saved" metric
  M9 pre-wired), and will soon run judge/summarization overhead (M11/M12) that
  belongs on the triggering step's bill, flagged as overhead. Every one of
  these needs a stated rule.
- **What to do before the call, and about models we don't know.** Budgets are
  checked against *projected* spend, so there must be a pre-flight estimate. And
  a step may name a model the catalog has never heard of; silently pricing it at
  $0 would let an unbudgeted model run free, so the unknown-model case needs a
  deliberate, configurable policy.
- **Hard vs soft budgets.** "Park exactly at the cap" and "downgrade at 80%" are
  different semantics on the same number; the vocabulary has to be fixed before
  enforcement is written.

This ADR decides the cost representation, the pricing-catalog format and
resolution, the attribution rules (with worked examples), the estimation
approach, the unknown-model policy, and the budget-semantics vocabulary.
Ticket 10.1 delivers the contract half — the `internal/cost` leaf (catalog
parse/validate/merge/resolve, the pure pricing arithmetic, the unknown-model
policy, the warning-event payload) and `config.CostConfig` — with **no
migration, no store change, and no engine/executor runtime change**, exactly as
9.1 and 9.4 opened their milestones. Tickets 10.2 (ledger & aggregation), 10.3
(budget enforcement at claim time), 10.4 (model downgrade chains), and 10.5
(metrics & events) implement against it and append their "(as built)"
subsections here as they land.

## Decision

### Money is integer nano-USD

Every computed or stored cost is an `int64` count of **nano-USD** (1 USD =
1e9 nano-USD, `cost.NanoPerUSD`). Catalog rates are *authored* as decimal
$/1M-token numbers for legibility, but they are converted to nano-USD at
pricing time and every cost that crosses a package boundary or lands in a row
is an integer.

The reason is 10.2's exactness requirement: a run's `spent_usd` is the sum of
its ledger rows, updated in the same transaction as each attempt completion and
asserted (property test) to equal the independent sum of those rows under
concurrent completions. Integer addition is associative and exact; floating
dollars are neither, and a fraction-of-a-cent drift multiplied across thousands
of attempts is a real discrepancy. Nano granularity is fine enough that even a
single token on the cheapest model is a non-zero cost (1 token at $0.10/1M =
100 nano-USD), and `int64` nano-USD saturates near $9.2 × 10⁹ — far beyond any
real run. Display USD (`$0.0123`) is derived from the integer at the API edge;
the integer is the truth.

Pricing rounds **half away from zero** per component (input cost and output
cost computed and rounded separately, then added), so the arithmetic is fully
determined by the inputs and reproducible on any platform.

### The pricing catalog

A **pricing catalog** is a versioned JSON document mapping a **resource name**
to an effective-dated price. The resource name is deliberately the *same key
the ADR-010 rate limiter uses* — `<resolved-provider>:<model>` (e.g.
`anthropic:claude-sonnet-5`, `openai:o3`, `mock:sim-1`), the provider wildcard
`<provider>:*`, or `tool:<name>`. A step's model `mock/sim-1` resolves to
provider `mock` and resource `mock:sim-1` for *both* rate limiting and pricing,
so one naming convention governs both scheduler-path features and the cost
attribution keys off the string the executor already builds.

The document shape (`schema_version: 1`):

```json
{
  "schema_version": 1,
  "models": [
    {"name": "anthropic:claude-sonnet-5", "effective_from": "2025-08-01",
     "input_per_mtok": 3.0, "output_per_mtok": 15.0},
    {"name": "anthropic:*", "effective_from": "2025-08-01",
     "input_per_mtok": 3.0, "output_per_mtok": 15.0}
  ],
  "tools": [
    {"name": "tool:paid_search", "effective_from": "2025-08-01",
     "per_call_usd": 0.01}
  ],
  "fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}
}
```

**Effective-dating.** Every entry carries an `effective_from` UTC calendar date
(day granularity — a within-day price change is not expressible, deliberately).
A model may appear several times with different dates; resolution at query time
`at` picks the newest entry whose `effective_from ≤ at`. A scheduled price
change is therefore authored as a *second* entry, and the old entry keeps
pricing runs that executed before the change. 10.2 passes the attempt's
completion time as `at`, so a run is always priced at the rate in effect when it
actually ran. A name whose entries are all future-dated relative to `at`
resolves as not-found (the unknown-model policy applies).

**Resolution order** mirrors `internal/limits.Lookup`: exact resource name
first, then the `<provider>:*` wildcard (provider = the segment before the first
colon), then not-found. The wildcard is what lets one entry price a whole
provider's model family — including served-model-id variants a step never named
explicitly (a provider may report `claude-sonnet-5-20250801` where the step
said `claude-sonnet-5`) — and it is why `mock:*` can price every mock model.

**Names are namespaced.** Model names must not carry the `tool:` prefix; tool
names must. This keeps the two price spaces from ever colliding and makes a
catalog self-describing. The name rules otherwise reuse ADR-010's (non-empty,
no whitespace, `*` only as a trailing `:*`).

**Embedded defaults + override, merged by name.** `internal/cost` embeds a
default catalog (`defaults.json`) compiled into the binary, carrying illustrative
current list prices for the Anthropic/OpenAI families plus a `mock:*` entry at
synthetic rates. An operator override — an inline JSON document
(`AGENTLOOM_PRICING`) or a file path (`AGENTLOOM_PRICING_FILE`), mutually
exclusive — is **merged onto** the embedded defaults: an override entry for a
name *replaces that name's entire entry list* (whole-list-per-name, so a model's
price history is authored in one place), unlisted default names survive, and the
override may replace the `fallback`. Merge-by-name (rather than
whole-catalog-replace) means adding one private model does not require copying
the default list, and an operator can never accidentally *un-price* a model by
omission. The embedded numbers are a starting point revisited from real billing
data, not an authority — the ledger's per-row rate snapshot (10.2), not the
catalog's current content, is what makes historical spend auditable.

### Attribution rules

Cost is attributed to the **attempt**, then rolled up to the step and the run
(10.2). The rules, each with a worked example at $3/$15 per-1M input/output
unless noted:

1. **LLM attempt** — `cost = input_tokens × input_rate + output_tokens ×
   output_rate`, priced at the model that *actually served* the call (the model
   the executor persists in its output, which 10.4's downgrade depends on), at
   the attempt's completion time.
   *Example:* 1,000 input + 500 output tokens → `1000 × 3000 + 500 × 15000` =
   3,000,000 + 7,500,000 = **10,500,000 nano-USD** ($0.0105).

2. **Cache hit** — actual cost **$0** (no provider call happened). The hit's
   counterfactual usage (ADR-011's `Usage.CacheHit` snapshot: the would-have-cost
   input/output token counts the original miss recorded) is priced by rule 1
   into a **"saved"** figure stored alongside the $0 actual. The saved figure
   feeds 10.5's saved-by-cache metric and the M18 meter; it never counts against
   a budget.
   *Example:* a hit whose snapshot is 1,000 + 500 tokens → actual 0, saved
   10,500,000 nano-USD.

3. **Priced tool** — flat `per_call_usd` from the `tool:<name>` entry, one
   charge per invocation. A tool with **no** catalog entry is **free** ($0, no
   warning): most built-in tools genuinely cost nothing, so absent-means-free is
   correct here — tools are *not* subject to the unknown-model policy (which is
   about unpriced *models*, where free would be a dangerous default).
   *Example:* `tool:paid_search` at $0.01 → 10,000,000 nano-USD per call.

4. **Overhead (judge, summarization)** — an M11 `llm_judge` validator's call or
   an M12 context-compaction summarization is attributed to the **step it
   serves**, on that step's bill, flagged `overhead: true` so a breakdown can
   separate productive spend from machinery. The rule is stated now; the flag is
   exercised when those milestones land. Judges are never themselves validated
   or judged (no recursion), so overhead never nests.

5. **No usage ⇒ no cost row.** A throttled attempt (ADR-010), a `lost`
   administrative outcome (ADR-005), or an errored provider call carries no
   usage and ledgers nothing. This is deliberately conservative in the same
   direction as 9.3's token reconciliation: we never invent spend for a call
   that did not bill.

6. **Provider-side prompt-cache token pricing is deferred.** `llm.Usage` today
   omits provider-reported cache-read/write token counts (a noted omission);
   pricing them (they bill at different rates) is out of scope until a later
   ticket needs it, and `llm.Usage` is untouched by this ADR.

### Estimation (pre-flight)

Budgets are checked against *projected* spend, so pricing needs a pre-flight
estimate available before the call:

```
estimate = estimated_input_tokens × input_rate + max_tokens × output_rate
```

The input-token estimate reuses ADR-010's shape (`chars/4` over the rendered
request, the same estimator the limiter already computes), and output is priced
at the step's configured `max_tokens` — the ceiling, so the estimate never
under-counts output cost. The estimate is deliberately an **upper bound**: 10.3
checks `spent + estimate` at claim time, and post-attempt re-evaluation against
the *actual* usage corrects the difference — the same estimate-then-reconcile
structure 9.3 applied to the token bucket, now applied to money.

### Unknown-model policy

When a step's model resolves to neither an exact entry nor a provider wildcard,
the behavior is configurable (`AGENTLOOM_COST_UNKNOWN_MODEL_POLICY`):

- **`estimate`** (default) — price the attempt at the catalog **`fallback`**
  rate (a deliberately high, conservative rate in the embedded defaults) and
  emit a **`cost_unknown_model`** warning event carrying the model name and the
  fallback rate. The run proceeds; the operator is told to add a catalog entry
  or accept the conservative estimate.
- **`fail`** — refuse to price the model. Enforced **pre-flight** (10.3): a
  claim for an unpriced model under fail-closed is a *deterministic
  misconfiguration*, so it fails **permanent** (ADR-006) to the DLQ **before any
  money is spent** — this is the whole point of fail-closed.

Two rules keep this coherent:

- The policy governs **pre-flight** decisions only. **Post-call ledger pricing
  never fails a succeeded attempt.** At ledger-write time the money is already
  spent; refusing to price it would *understate* real spend. So 10.2 always
  prices with the `estimate` behavior (fallback + warning) regardless of the
  configured policy, and only 10.3's pre-flight check honors `fail`. In the
  `internal/cost` API this is expressed by the caller passing the policy:
  `PriceModel(name, at, PolicyEstimate)` for the ledger, `PriceModel(name, at,
  configuredPolicy)` for the pre-flight gate.
- The warning event is emitted per fallback-priced attempt (bounded by the
  retry budget, so no dedup state is needed). Its **payload contract** — the
  model name and the fallback rate — is fixed here; the *physical* append lands
  in 10.2's attempt-completion transaction (there is no ledger transaction to
  hang a per-run event on yet), where `store.EventCostUnknownModel =
  cost.EventTypeUnknownModel` joins the event vocabulary. `events.type` is
  free-form TEXT in schema v1, so this needs no migration then either.

### Budget semantics

The vocabulary the enforcement tickets build on (implemented 10.3/10.4, fixed
here):

- **Run `budget_usd`, step `max_usd` / `max_tokens`.** A run has an overall USD
  budget; a step may cap its own USD cost and its token count.
- **Hard budget** → the run **parks** with reason `budget_exceeded` (ADR's M5.6
  park primitive: no lease or worker slot held while parked), resumable by
  raising the budget (`PATCH /v1/runs/{id}/budget`) and unparking — or **fails**,
  per the configured action.
- **Soft threshold** → a **downgrade** trigger (10.4): crossing, say, 80% of the
  run budget routes the next claim to a cheaper model in the step's
  `model_fallbacks` chain. A different model is a different resource name, hence
  a different cache key (ADR-011) — asserted in 10.4.
- **Checked at claim** on projected spend (`spent + estimate`); a step already
  in flight is never killed mid-execution by a budget crossing. This is what
  makes budget enforcement a *scheduling* feature: the decision is at the same
  point as the readiness/claim decision, not after the fact.
- **Step-level `max_tokens`** is enforced pre-flight against the request so an
  oversized request is never sent (10.3).

### As built (10.1)

The contract half shipped in `internal/cost` and `config`:

- **`internal/cost` leaf** (stdlib only, imports no other agentloom package —
  the `internal/cache`/`internal/limits` precedent, so engine/exec/store can
  depend on it without a cycle): `Catalog` with `Parse` (strict decode,
  all-errors-joined validation: `schema_version`, name rules + namespace split,
  non-negative finite rates, required `effective_from`, duplicate
  `(name, date)`), `Load(inline, file)` (mutual-exclusion, merge onto embedded
  defaults), `Merge` (whole-list-per-name replacement, fallback override, copies
  so the shared default is never mutated), `Default()` (embedded `defaults.json`,
  parsed once and cached), and `Lookup` / `PriceModel` / `ToolPrice` with
  effective-date selection, wildcard fall-through, and the unknown-model policy.
  `price.go` is the pure arithmetic: `Cost`, `Estimate` (the upper-bound
  formula), `ToolCost`, all in integer nano-USD with half-up rounding.
  `event.go` defines `EventTypeUnknownModel` + the `UnknownModelWarning`
  payload. `Source` (exact/wildcard/fallback) records rate provenance for the
  10.2 ledger.
- **`config.CostConfig`** — `Inline` / `File` (`AGENTLOOM_PRICING[_FILE]`,
  mutually exclusive, checked at config load *and* in `cost.Load`) and
  `UnknownModelPolicy` (`AGENTLOOM_COST_UNKNOWN_MODEL_POLICY`, `estimate` |
  `fail`, default `estimate`, validated at load). `config` stays a leaf: it
  carries the policy *string* and duplicates the two-value validity check rather
  than importing `internal/cost`, exactly as it does for `internal/limits`;
  cmd/worker maps the string onto `cost`'s typed enum at the 10.3 claim-time
  check.
- **`cmd/worker`** boot-loads and validates the catalog (a malformed override
  fails boot, not the first priced attempt) and logs the model/tool counts,
  override source, and policy. No runtime consumer yet — 10.2 hands the catalog
  to the engine. `cmd/api` is untouched (it reads ledger rows from Postgres in
  10.2; it never prices).
- **No migration, config store, or engine/executor runtime change in 10.1.**
  The `cost_ledger` table, the attempt-completion pricing, the claim-time budget
  check, downgrade chains, the physical `cost_unknown_model` append, and the
  cost metrics are 10.2–10.5.

### As built (10.2)

The cost ledger and aggregation — a `cost_ledger` row per cost-bearing attempt
priced against the 10.1 catalog, written in the same transaction as the
success CAS, with run-level aggregates and a cost API on top.

- **The resource channel.** Attribution keys off the ADR-010 resource name, but
  the *resolved* provider is known only inside the executor, so `exec.Output`
  gained a `Resource` field. The llm executor sets
  `<provider>:<served-model>` — the model that actually served the call
  (`resp.Model`, so a dated variant a step never named prices via the provider
  wildcard, and 10.4's downgrade will price the actual model); the tool
  executor sets `tool:<name>`. Empty means the attempt is not cost-bearing.
  The engine's cache middleware carries the resource across a hit (a new
  `cost_resource` field on the stored `cacheEntry`, `omitempty` so a pre-10.2
  entry decodes to `""` and simply ledgers no saved figure).

- **Pricing is a pure pre-transaction function.** `engine/cost.go`'s
  `priceAttempt` reads no database: given the attempt's resource, usage, and
  the catalog effective at completion time it returns a `store.AttemptCostArgs`
  (or nil when the attempt ledgers nothing — pricing disabled, no resource, an
  unpriced/free tool, or a model attempt with no usage). It always passes
  `PolicyEstimate` (post-call pricing never fails a succeeded attempt), so an
  unknown model is priced at the fallback with the `cost_unknown_model` warning
  attached — **except on a cache hit**, which spent nothing and whose original
  miss already warned. `complete.go` computes the row before the transaction
  and calls `store.ApplyAttemptCost` *after* `SucceedStep` landed (a fenced
  zombie completion returns before it, so it never ledgers) under the run lock
  the CAS already holds.

- **Migration 0016.** The `cost_ledger` table keyed `(run_id, step_id, attempt,
  entry)` — `entry` discriminates the charge kind (`attempt` in 10.2; ADR-012
  rule 4's `judge`/`compaction` overhead rows are the same-attempt slot M11/M12
  fill without a schema change), with the `resource`, `usage`, `rate` snapshot,
  `rate_source`, `cache_hit`, `overhead`, `cost_nano_usd`, and `saved_nano_usd`
  columns. The claim-fenced success CAS lands at most one attempt-completion per
  attempt, so the productive row can never conflict. Run aggregates are two
  scalar `BIGINT` columns on `runs` (`spent_nano_usd`, `saved_nano_usd`)
  incremented in the same transaction — the exact-sum property is then an
  integer `spent == SUM(cost_nano_usd)`, proven by the concurrent-fan-out
  property test. **By-step and by-model breakdowns are `GROUP BY` reads over
  `cost_ledger` at API time**, not materialized: the ledger rows commit in the
  same transaction as the aggregate, so a read-time group is exactly
  consistent and adds no write contention. (The columns carry the nano unit in
  their names — `spent_nano_usd`, not the roadmap's shorthand `spent_usd` —
  matching the ADR-008 units-in-name convention; the derived `*_usd` display
  strings live only on the wire.)

- **Store surface.** `store.ApplyAttemptCost` (transition-style, `ErrNoTx`
  guarded, called inside the completion tx) inserts the row, bumps the
  aggregate, and appends `cost_unknown_model` when a warning rides along;
  `store.CostRepo` (`Ledger()`) reads the ledger and its two breakdowns for the
  API and the property test's independent sum. `store.EventCostUnknownModel`
  joins the event vocabulary (mirroring `cost.EventTypeUnknownModel`).

- **API.** `GET /v1/runs/{id}` gained a `cost` summary on the run view;
  `GET /v1/runs/{id}/cost` returns the summary plus `by_step`, `by_resource`
  (per-model / per-tool, with token sums), and the full per-attempt `entries`.
  Money is integer nano-USD on the wire; the `*_usd` strings are rendered by
  exact integer division (never float). `cmd/api` never prices — it reads the
  ledger rows Postgres already holds.

- **Attribution corners realized.** A cache hit ledgers a $0 row with
  `cache_hit=true` and the counterfactual `saved_nano_usd`; a priced tool
  ledgers a flat row (its rate snapshot is the per-call nano cost); an
  **unpriced tool writes no row** (free, ADR-012 rule 3 — keeps the ledger from
  a row per `json_transform`); a no-usage or non-cost-bearing attempt writes
  nothing. `overhead` is always `false` in 10.2 (the flag and its PK slot are
  pre-wired for M11/M12).

### As built (10.3)

Budget enforcement at claim time — the check that makes cost a *scheduling*
feature, evaluated at the same point as the readiness/claim decision, before
any money is spent.

- **The contract.** `internal/dag` gained the run-level `budget_usd` (a
  positive USD number; nil = unbudgeted) and `on_budget_exceeded` (`park` the
  resumable default | `fail`), plus the step-envelope `budget` block
  (`max_usd`, `max_tokens`), validated under new codes `budget_field_required`
  / `budget_field_invalid` (positivity, at-least-one-cap, and `max_tokens`
  restricted to llm steps — a cap that can never fire is an authoring
  mistake). Money is materialized as integer **nano-USD** on `runs`
  (`budget_nano_usd` nullable, `on_budget_exceeded` NOT NULL DEFAULT `park`,
  migration 0017) — the same unit as `spent_nano_usd`, so the projection
  compare is exact — and the step caps ride `run_steps.budget_policy` JSONB,
  read off the claimed row like `cache_policy`.

- **The estimate hook.** A new optional executor interface
  `exec.CostEstimator` projects `{Resource, InputTokens, MaxTokens}` — the
  input/output token split (the llm executor's `chars/4` input estimate and
  its `max_tokens` output ceiling, `<resolved-provider>:<model>`; the tool
  executor's `tool:<name>`, no token dimension). Its error routes like
  `ResourceClaim`/`CacheBinding`: skip the check, let Execute land the
  classified failure, so the routing judgment lives in one place.

- **The middleware.** `engine/budget.go`'s `budgetCheck` runs in `execute()`
  **after the cache read and before the rate limiter** — a deliberate
  deviation from the middleware chain's nominal cache→limit→budget order,
  because a cache hit is $0 (never budget-gated) and parking *after* a limiter
  acquire would strand debited tokens 9.3 only reconciles on success. The pure
  `budgetDecide` prices the estimate against the catalog (upper-bound
  `cost.Estimate`), then routes: **step `max_tokens`** over cap → permanent
  fail (the oversized request never runs); an **unknown model under
  fail-closed** (`WithUnknownModelPolicy`, mapped from
  `AGENTLOOM_COST_UNKNOWN_MODEL_POLICY`) → permanent fail before spend; **step
  `max_usd`** (cumulative step ledger + estimate) over cap → permanent fail; a
  **run-budget** projection (`spent + estimate`, from the claim-time
  `ClaimOrigin` spend read under the run lock) over budget → park or fail per
  the run's disposition. A `$0` estimate (a free tool) never triggers a run
  park. The decision uses the claim-time spend on the origin — exact for a
  sequential chain (the predecessor committed before the claim), and
  conservative-low for concurrent fan-out (spend only rises after the read, so
  the check never *over*-parks), the accepted bounded-overshoot tolerance.

- **Park vs fail.** Park is atomic: `store.BudgetParkStep` releases the step
  `running → ready` (the `BudgetParkRunStep` CAS, fenced on the caller's
  claim), closes the attempt with the administrative outcome
  **`budget_exceeded`** (the third such outcome after `lost` and `throttled`,
  outside ADR-006's taxonomy — never counted against the retry budget, the
  step never ran; migration 0017 extends the outcome CHECK), appends the
  `budget_exceeded` event with the full projection detail, and parks the run
  with reason `budget_exceeded` — all in one transaction. Parking only when
  the run is still `running` under the lock makes concurrent fan-out siblings
  release without a duplicate `run_parked`. The released step is `ready` (not
  `retrying`), so unpark re-dispatches it with **zero reconciler dependency**.
  Fail routes through the existing `completeFailure(permanent, "budget_
  exceeded: …")` — DLQ + the run's ordinary `on_failure` disposition — so the
  descriptive dead-letter is the record and the fail path needs no new event.

- **Resume.** `PATCH /v1/runs/{id}/budget` (submit scope) raises the budget
  (`store.SetRunBudget`, guarded to non-terminal runs, `run_budget_updated`
  event); the client then unparks. `engine.Control.SetBudget` is the
  registry-free op the API and `ctl budget <run-id> <usd>` call; no dispatch
  nudge (ADR-002 — unpark dispatches). `GET /v1/runs/{id}` and `/cost` carry
  the budget on the cost summary (`budget_nano_usd`/`budget_usd` nullable,
  `on_budget_exceeded`); OpenAPI still lints 100/100.

- **Headline test.** A sequential mock-llm chain under a $0.005 budget (each
  step ~$0.002) parks at exactly the step whose projection first exceeds the
  cap (`spent ≤ budget < spent + one step's estimate` — the defined
  tolerance), emits `budget_exceeded`, then — budget raised to $1 and
  unparked — resumes and completes with every step's cost ledgered. Plus the
  fail-policy DLQ, the oversized-`max_tokens` zero-provider-calls guard, and
  the decision unit matrix.

- **Not in 10.3.** Model downgrade chains (`model_fallbacks`; 10.4 slots
  before park/fail in `budgetDecide`), budget metrics and `cost_updated`
  events (10.5), a submit-time `budget_usd` override on `POST /v1/runs`, and
  reservation-based exact accounting for parallel fan-out (backlogged).

### As built (10.4)

Model downgrade chains — routing a claim to a cheaper model as the run
approaches its budget, instead of parking at the primary's price:

- **The contract.** `model_fallbacks: [{model, at_budget_fraction?}]` on the
  **llm step config** in `internal/dag` (an ordered, cheapening chain;
  `ModelFallback`). Validation (code `config_field_invalid`): each `model`
  non-empty and distinct from the primary and from every other fallback;
  `at_budget_fraction` in `(0, 1)` and requiring the definition to carry a
  `budget_usd` (a fraction of nothing cannot fire); thresholds non-decreasing
  along the chain (cheaper tiers trigger at higher spend); and a chain with no
  budget to trigger against at all (no run budget and no step `max_usd`)
  rejected. **No migration** — the actually-used model is already durable on
  10.2's `cost_ledger.resource` and the attempt output, so "record the used
  model on the attempt" needs no new column.

- **Two triggers, evaluated at claim.** The **soft** trigger: once the run's
  spend reaches a fallback's `at_budget_fraction`, claims route proactively to
  that tier — the deepest (cheapest) tier whose threshold is met — even if the
  primary would still fit, to conserve budget. The **hard** (projection)
  trigger: if the primary's projected spend (`spend + estimate`) would exceed a
  budget, route to the least-aggressive fallback that fits, avoiding a park. A
  tier is chosen only if it is **priceable and its projection fits every
  budget**, so the engine never downgrades to a model it would immediately have
  to park on; when no fitting tier exists the chain is *exhausted* and the check
  falls through to 10.3's ordinary park/fail on the primary. A primary that is
  itself unpriceable (unknown model under fail-closed) yields no downgrade —
  `budgetDecide` lands its permanent failure, keeping the fail-closed judgment
  in one place.

- **The executor hook.** A new optional `exec.ModelDowngrader`: `ModelFallbacks`
  reports the current model and the chain; `WithModel` rewrites **only** the
  rendered config's `model` field (a raw-message re-key, leaving every sibling
  value byte-identical). Because the response-cache key, the resource limiter's
  bucket, the cost estimate, and the provider call all derive from that model,
  the one rewrite re-targets the whole executor pipeline — so a downgrade is a
  **different cache key** by construction (asserted at the exec layer:
  `TestDowngradeChangesCacheKey`), the fallback bills to its own resource, and
  the ledger prices the model that actually served.

- **The decision.** `engine/budget.go`'s `budgetCheck` was restructured so the
  model-independent `max_tokens` guard runs first, the step's cumulative cost is
  read once and threaded into both the downgrade fits-check and the step
  `max_usd` cap, and — when the executor is a `ModelDowngrader` with a chain —
  `selectDowngrade` prices the primary and each tier and calls the pure
  `chooseDowngrade` (`engine/downgrade.go`, unit-matrix-tested:
  threshold / projection / rescue-when-the-threshold-tier-doesn't-fit /
  exhaustion). The decision is **single-shot** (no budget re-entry): it picks
  the final fitting tier in one pass, so a multi-tier threshold jump is one
  event. On a downgrade the middleware records a **`model_downgraded`** event
  and returns the re-targeted config; the claim path re-keys the response cache
  on the fallback model (a fallback hit still short-circuits the provider call)
  and proceeds. `budgetDecide` (reached only when no downgrade applied) keeps
  the 10.3 park/fail/proceed routing, now IO-free.

- **The event.** `store.RecordModelDowngrade` appends `model_downgraded` in its
  own short `step.downgrade` transaction — a downgrade is **not** a state
  transition (no status change; the used model is durable on the ledger), so it
  only **fences on the caller's live claim** (a zombie cannot record a
  misleading downgrade) and appends the event with the from/to models and
  resources, the trigger (`budget_threshold` | `budget_projection`), and the
  spend/budget projection. A fenced caller surfaces a `*store.TransitionError`
  so `budgetCheck` abandons like any other fenced write; a transport error
  redelivers.

- **Headline test.** A catalog pricing `mock:expensive` 50× `mock:cheap`: an
  expensive-model chain under a $0.25 budget with a `0.5` threshold runs the
  first two steps expensive, then downgrades the rest to cheap — the ledger
  prices each attempt at the model that served it (asserted per-step resource +
  output model), three `model_downgraded` threshold events land, and the run
  aggregate equals the exact ledger sum. Plus a projection-trigger test (budget
  too tight for the primary → immediate downgrade, `budget_projection` against
  the run limit) and an exhausted-chain test (even the cheap tier over budget →
  park with **no** downgrade event, then raise-budget + unpark → completes on
  the primary).

- **Not in 10.4.** Budget/downgrade metrics and `cost_updated` events (10.5),
  and rescuing an unpriceable primary via a known fallback (deliberately kept
  out — an unknown primary fails closed, crisply).

## Consequences

**Easier.**

- 10.2's exact-sum property is trivial: ledger rows are `int64` nano-USD, and an
  aggregate is an integer sum with no rounding reconciliation.
- Pricing keys off the string the executor already computes for rate limiting,
  so cost attribution needs no new identity plumbing, and one naming convention
  documents both features.
- Effective-dating means a price correction is additive (a new entry), never a
  destructive edit, and historical runs stay correctly priced.
- Merge-by-name + embedded defaults means a working catalog exists with zero
  configuration (the `mock:*` wildcard prices the offline M10 exit workflow), and
  an operator overrides incrementally.
- The estimate-then-reconcile structure is the same one 9.3 proved for tokens,
  so 10.3's post-attempt correction is a known pattern.

**Harder / accepted costs.**

- Nano-USD integers require a display-conversion at every API/UI edge; a raw
  `spent_usd` of `10500000` is not human-readable without dividing.
- Authoring a within-day price change is impossible (day-granularity dates) — an
  accepted limitation, since sub-day provider repricing is not a real workflow.
- The embedded defaults will drift from real list prices over time; they are
  explicitly illustrative, and a production deploy is expected to override them.
  The anti-drift guard is the defaults test (the embedded document must parse,
  carry a fallback, and price `mock:*`), not price accuracy.
- Two small duplications are accepted for the leaf boundary: the resource-name
  validation rules (copied from `internal/limits`) and the policy-value check
  (in `config`, not importing `cost`). Both are cross-referenced.
- The unknown-model policy's split behavior (fail-closed pre-flight, always-price
  post-hoc) is subtle and must be honored by callers passing the right policy;
  it is stated explicitly so 10.2/10.3 cannot get it wrong silently.

## Alternatives considered

- **Postgres `NUMERIC` dollars end-to-end.** Exact like integers, but it leaks
  decimal handling into every Go computation (no native decimal type — a
  `big.Rat`/`shopspring/decimal` dependency), and the arithmetic is slower and
  noisier than `int64` for no benefit the nano-USD integer doesn't already give.
  Rejected: integers are exact, native, and fast; USD display is a trivial edge
  conversion.
- **Float64 dollars.** Simple and readable, but non-associative summation makes
  10.2's exact-aggregate property impossible to state honestly, and
  sub-cent drift accumulates. Rejected outright.
- **Whole-catalog-replace on override** (operator supplies the *entire* catalog,
  no embedded defaults merged). Simpler merge semantics, but it forces an
  operator adding one private model to copy and maintain the whole default list,
  and a partial override silently un-prices every model it omits — the exact
  footgun that lets an unbudgeted model run at $0. Rejected in favor of
  merge-by-name; whole-list-per-name replacement keeps a single model's history
  in one place without the un-pricing hazard.
- **Fail-closed pricing at ledger-write.** Symmetric with the pre-flight policy,
  but the money is already spent by then — refusing to ledger a succeeded
  attempt would understate real spend and lose the cost record entirely.
  Rejected: post-hoc pricing always succeeds (fallback + warning); only the
  pre-flight gate fails closed.
- **Pricing by opaque plugin identity instead of the resource name.** Would
  decouple cost keys from the limiter's names, but then two subsystems key the
  same external call two different ways, and the executor would compute a second
  identity string. Rejected: one convention, one string, both features.
- **Per-date merge of override onto defaults** (union entry lists by date rather
  than replace the whole list). More flexible, but it makes a model's effective
  price depend on the interleaving of two documents, which is hard to reason
  about and audit. Rejected: a name's price history lives in exactly one
  document.
