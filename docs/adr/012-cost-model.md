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
