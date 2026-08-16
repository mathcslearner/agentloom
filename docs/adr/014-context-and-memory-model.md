# ADR-014: Context & memory model

- **Status:** Accepted
- **Date:** 2026-08-15
- **Ticket:** ROADMAP.md ticket 12.1

<!--
This ADR opens M12 (context & memory management). 12.1 delivers the token
counters (internal/tokens) and fixes the context model the rest of the
milestone conforms to. Later M12 tickets build on it: 12.2 (the blackboard
store), 12.3 (declarative context assembly), 12.4 (deterministic compaction
strategies), 12.5 (summarization compaction), 12.6 (provider-window
guardrails). Sections marked "(arrives in 12.x)" fix the contract now so those
tickets conform without re-litigating it.
-->

## Context

Every long multi-step AI workflow eventually collides with a hard wall the
transport layer cannot smooth over: the model's context window. A step that
assembles "everything relevant" — prior step outputs, retrieved documents, a
running conversation, a scratchpad of intermediate decisions — will, on a long
enough run, hand the provider more tokens than the model accepts, and the call
comes back a permanent `400 context_length_exceeded`. M5's retry taxonomy does
not help (a bigger prompt fails identically every time), M9's rate limiter does
not help (the request is well-formed, just too large), and M10's budget does
not help (the run has money left). The differentiator M12 sells is that
agentloom makes this failure mode *structurally impossible*: compaction lives
in the pre-execution path of the LLM executor, so any workflow benefits without
bespoke code, and the pre-flight window check (12.6) turns "overflow 400" into
"auto-compact, or a typed error before the call."

That machinery rests on one primitive that must exist first, and be trustworthy:
**counting tokens.** You cannot budget, assemble-to-a-cap, compact-until-under,
or guard a window without knowing, *before the call*, how many tokens a request
will cost. The forces on that primitive:

- **It must be model-aware.** Different model families tokenize differently: an
  OpenAI model uses a published BPE (tiktoken); Anthropic does not publish its
  tokenizer at all; a mock provider tokenizes however its estimator says. A
  single chars/4 heuristic is off by 20–40% on code and JSON — far outside the
  ±5% the milestone demands — so the counter must be selected per resolved
  (provider, model), the same identity ADR-010's limiter and ADR-012's catalog
  already key on.

- **It must count the *request*, not just text.** The provider bills (and the
  window is spent on) the fully-framed request: system prompt, every message's
  role framing, tool definitions, structured-output schemas, reply priming.
  Counting bare concatenated text misses 5–15 tokens of framing per message and
  drifts a multi-turn conversation well past ±5%. The counter's unit is the
  unified `llm.ChatRequest`.

- **It must be deterministic and cacheable.** 12.2's blackboard stores a
  `token_count` per entry; 12.3's assembly computes a pre-flight total; 12.4's
  compaction reports tokens-before/after. If those numbers were re-derived by a
  non-deterministic or drifting counter, the byte-identical-assembly golden
  tests (12.3) and the ≤-budget property tests (12.4) would be untestable. A
  stored count is valid only against the exact counter that produced it, so
  every counter carries a stable fingerprint.

- **The network is not on the hot path.** Anthropic offers a free
  `count_tokens` API and OpenAI reports `prompt_tokens` on every response, but
  calling either to *decide* a prompt's size before sending it adds a round-trip
  to every LLM step and couples compaction to provider availability. Counting
  must be a pure, in-process, offline function; the provider APIs are for
  *calibration* (deriving the estimator's constants once, offline, from a
  recorded corpus), not for per-call use.

M9.2 already ships a deliberately rough estimator (chars/4 + declared
`max_tokens`) for the limiter and M10 budget, and 9.3 reconciles the post-call
actual−estimate error back onto the token bucket so a biased estimator cannot
drift the fleet. That estimator is *fine for rate limiting* (an over/under-count
self-corrects across the fleet) but *not fine for window guardrails* (a single
under-count is a hard 400). 12.6 swaps it for these counters; 12.1 only builds
them.

## Decision

### Token counting (`internal/tokens`, as built in 12.1)

We add a leaf package `internal/tokens`. It imports `internal/llm` (for the
unified `ChatRequest`/`Message`/`Block`/`ToolDef` shapes it counts) and stdlib
and the tiktoken library; it imports no other agentloom package, so
`exec`/`engine`/`contextmgr` can depend on it without a cycle (the
`internal/cost`, `internal/cache`, `internal/limits` precedent). It never
touches the clock or the network.

The package exposes:

```go
type Counter interface {
    // ID is the counter's stable fingerprint, e.g. "openai/o200k_base@1",
    // "anthropic/estimate@1;factor=1.15", "mock/estimate@1", "fallback/chars4@1".
    // A stored token_count is valid iff the storing counter's ID matches the
    // current one; on a mismatch the consumer recomputes.
    ID() string
    // Count returns the token count of a bare string in this counter's
    // tokenization. Deterministic and side-effect-free.
    Count(text string) int
    // CountRequest returns the token count of a fully-framed request:
    // system prompt, per-message role framing, content blocks (text,
    // tool_use input JSON, tool_result content), tool definitions, the
    // structured-output schema when present, and reply priming.
    CountRequest(req llm.ChatRequest) int
}
```

**Counter families.**

- **OpenAI** — exact BPE via tiktoken (`github.com/pkoukk/tiktoken-go` with
  `github.com/pkoukk/tiktoken-go-loader`'s offline `go:embed`'d rank tables, set
  once via `SetBpeLoader`; the default network loader is never used — an
  `http.DefaultTransport` tripwire in the test suite proves the counter makes no
  request, the 8.5 mock-provider precedent). The encoding is chosen by model
  prefix: `o200k_base` for the current families (`gpt-4o`, `gpt-4.1`, `gpt-5`,
  the `o1`/`o3`/`o4` reasoning series), `cl100k_base` for `gpt-4`/`gpt-3.5`
  legacy, defaulting to `o200k_base` for an unrecognized OpenAI model. For text
  this is the reference tokenizer OpenAI itself uses; only the chat framing
  constants are an approximation (below).

- **Anthropic** — an *estimating* counter, because Claude's tokenizer is not
  public. It counts with the o200k BPE (the closest public reference) and
  multiplies by a per-family **calibration factor** derived once, offline, from
  a recorded `count_tokens` corpus (`AnthropicCalibrationFactor`, a pinned
  constant; the gated recorder re-derives it by least-squares over the corpus
  and a test guards the fixtures against >0.5% drift from the constant).
  Optional use of the Anthropic `count_tokens` API is *flagged and off by
  default* (`WithCountTokensAPI`, a declared seam that 12.1 does not implement —
  it is a calibration/pre-flight aid, never on the per-call path); the default
  path is pure and offline.

- **Mock** — mirrors `internal/llm`'s mock provider estimator exactly
  (`len(text)/4 + 1` per component with the same framing the mock reports as
  `Usage.input_tokens`), so a mock-driven fixture's counted total equals the
  mock's reported usage — the property that lets the M12 exit fixture run
  offline in CI with real accuracy assertions.

- **Fallback** — `ceil(len(text)/4)` (never below 1 for non-empty text). Used
  for any provider/model with no registered counter. Selection logs the
  fallback **once per (provider, model) per process** (a `sync.Map` of
  `sync.Once`), never per call.

**Selection.** `Registry.Select(provider, model)` takes the *resolved* provider
name (the one `llm.Registry.Resolve` returns — routing is not re-implemented in
`tokens`) and returns the counter plus a `Selection` describing the match
(family, encoding, whether it fell back). This is the only place model→counter
mapping lives; 12.3/12.6 call it after they have already resolved the model.

**Chat framing.** `CountRequest` follows the documented OpenAI chat-completion
accounting (per-message overhead + per-name overhead + a fixed reply-priming
constant) generalized to the unified request: the system prompt is one framed
message; each `Message` is `perMessage + Σ block-content tokens`; a `tool_use`
block counts its name plus its input JSON; a `tool_result` block counts its
content; each `ToolDef` counts name + description + input-schema JSON plus a
per-tool constant; a non-nil `ResponseFormat` counts its schema JSON. The
constants are family-scoped and documented at their definitions; they are what
the ±5% accuracy fixtures calibrate and guard.

**Determinism & audit.** Counters are pure functions of their input and their
embedded tables; `ID()` changes only when the tokenization changes (encoding
swap, calibration re-derivation, framing-constant change bumps the `@N`
suffix). Consumers persist `(token_count, counter_id)` together; a mismatch is a
recompute, never a silently-stale number. No counter logs on the count path;
selection logs the fallback once.

### Context model (contract now; code in 12.2–12.6)

The following fixes the vocabulary and guarantees the rest of M12 implements, so
those tickets conform without re-deciding.

**Sources & precedence.** A step's `context` spec is an *ordered* list of
sources; order is precedence (earliest-declared wins a token-budget contest and
appears first in the assembled message list). Source kinds:

- `step_output` — a named upstream step's output (a JSON path into it, like the
  8.2 templating refs);
- `blackboard` — run-scoped entries selected by key or by tag (12.2);
- `retrieval` — ranked results from a retriever query (8.8);
- `literal` — an inline constant (system-prompt fragments, instructions).

Each source may declare a per-source token cap. Assembly (12.3) is
**deterministic given store state**: the same blackboard/step-output state
produces a byte-identical message list and an identical pre-flight token total.

**Pinning.** An entry (or source) tagged `pinned` is *never* dropped or
truncated by any compaction strategy. A pipeline that cannot fit the budget with
the pinned set alone yields a typed error, never a silently-dropped pin (12.4
asserts this across every strategy).

**Per-step context budget.** Default budget = `model_context_window − max_tokens
− headroom`, where the window comes from the pricing/model catalog (12.6 adds
`context_window` to ADR-012's catalog), `max_tokens` is the step's completion
bound, and `headroom` is a small safety margin (default 5% of the window, with a
floor) absorbing framing-estimate error and the Anthropic calibration slop. A
step may override the budget explicitly. The budget is computed with the same
counter the assembly and guardrail use, so "assembled ≤ budget" and "assembled +
max_tokens ≤ window" are the same arithmetic.

**Strategy pipeline.** When assembly exceeds the budget, the step's configured
*ordered* strategy pipeline is applied, each strategy only while still over
budget:

- `drop_lowest_priority` — evict whole non-pinned sources/entries by ascending
  priority (declaration order is the default priority);
- `truncate_oldest` — middle-out truncation of the oldest content with an
  explicit elision marker (12.4);
- `sliding_window` — keep only the last N messages (12.4);
- `summarize` — replace an evicted span with a cheap-model summary written to
  the blackboard (12.5), cost attributed as **overhead** on the serving step
  (ADR-012 rule 4, the `entry=compaction:<i>` ledger slot 0016 reserved), the
  summary prompt deterministic at `temperature=0` and cached; a summarizer
  failure falls back to the *next* deterministic strategy and never blocks the
  step.

**Hard guarantee.** After the pipeline runs, either the assembled context is
≤ budget, or the step fails with a typed `ContextOverBudget` error *before any
provider call* (12.4/12.6). Overflow is never sent to the provider.

**Determinism & audit.** Every strategy application writes a `context_revision`
event (or attempt-scoped record) naming what was dropped/kept and the tokens
before/after, queryable per step attempt (12.4). Summaries are visible on the
blackboard as their own versioned entries (12.5). The whole pre-execution
context decision is reconstructable after the fact from durable state.

### Blackboard store (as built, 12.2)

The run-scoped **blackboard** is the shared, versioned key/value memory steps
read and write during a run — the source of the `blackboard` context source
above, the substrate M14's multi-agent handoffs build a thread on, and where
12.5's summaries land as their own entries. It is deliberately a separate
concern from token counting; 12.2 delivers the store, its executor-facing
API, its declarative write sugar, and its read API.

**Package shape.** `internal/blackboard` is a leaf (stdlib only): the domain
types (`Entry`, `PutArgs`, the `Board` interface), the key/tag/value rules,
and the shared `CanonicalValue`/`ResolvePointer` helpers — so `exec`, the
engine, and the coming `internal/contextmgr` depend on it without a cycle.
The Postgres implementation is the subpackage `internal/blackboard/pgboard`
(the only importer of `internal/store`), the `retrieval`/`pgfts` precedent.

**Schema (migration 0021).** `blackboard_entries` is **append-only per key**:
a write never overwrites, it inserts the next `version` (1-based per
`(run_id, key)`), so `History` reconstructs every revision — the audit this
milestone rests on. A row carries `value` (JSONB), `token_count` +
`token_counter` (the `tokens.Counter.ID()` that produced the count, stored
together so a consumer recomputes on a fingerprint mismatch), `tags`
(`TEXT[]`, GIN-indexed), and the author step + attempt. Tags are
**per-version and immutable**: re-tagging (including pinning) is a new
version, so the history stays honest. `run_steps.blackboard_policy` is the
materialized declarative-write block (like `cache_policy`). Retained for the
life of the run and beyond for post-run audit; pruning is M21.

**Write protocol.** `store.PutBlackboardEntry` is a transition-style helper:
inside the caller's transaction it takes the run lock (existence + the
uniform run→step ordering, and the serialization that makes version
allocation collision-free), optionally **fences** on the authoring step's
`claim_id`, evaluates an optional compare-and-swap guard against the current
head, inserts `head+1`, and appends a `blackboard_updated` event — all
atomically. A rejected CAS (`*BlackboardVersionConflict`) or fence
(`ErrBlackboardFenced`) writes nothing, burning no event seq (the 2.6
discipline). Because every writer holds the run lock, two concurrent
**unconditional** writers of one key never collide — they serialize into v1
and v2, no lost update. Two concurrent **CAS** writers (`expected_version`)
race: one lands, the other gets a version conflict.

**CAS conflict class.** A version conflict is *state moved*, so an executor
surfaces it as a **transient** error: the M5 retry re-attempts, re-reads the
head, and can succeed at the next version. Invalid key/tag/value and a fence
rejection are **permanent** (deterministic). This is the disjoint-from-
transport judgment the engine's classifier applies.

**Executor API.** `StepContext.Blackboard` is a step-scoped `Board`
(programmatic `Get`/`History`/`List`/`Put`), attributed to the step and
fenced on its claim — a taken-over zombie's write is rejected, unlike the
5.5 journal's first-wins result. Each `Put` is its own short transaction (the
effects-journal model). Token counts use the step's counter: the llm
executor's new `exec.TokenCounterProvider` hook resolves the model's
tokenizer; every other step gets the chars/4 fallback (the honest "no model
tokenizer here" choice).

**Declarative writes.** A step's `blackboard` envelope block declares
`write: [{key, from, tags, pinned}]`: on success, the value at the `from`
RFC-6901 pointer into the step's *own* output is published under `key`. It is
applied **in the completion transaction**, after the success CAS and under
the run lock it holds — so a fenced zombie completion never writes, and the
entry is durable exactly with the step's success. Planning (pointer
resolution) is pure and pre-transaction; a pointer that does not resolve is a
permanent step failure (ADR-006 row 15), carrying the output so its
productive cost still ledgers. A cache-hit completion applies the writes too
(the output is the same). **Declarative reads into a prompt are 12.3's
context assembly** (the `blackboard` context source), not 12.2 — this ticket
delivers writes + the programmatic read/write API; the `blackboard.<key>`
template read root is deferred with them, since it is only useful once
assembly consumes it.

**Cache-key note.** Programmatic blackboard reads during execution are *not*
cache-key inputs (the key is built before execution). A step that depends on
blackboard state must therefore either be uncacheable (the `blackboard_write`
test executor is `side_effectful`) or receive that state through the config
(templating / 12.3 assembly, both of which *are* in the key). 12.3's
declarative sources are assembled into the request pre-cache and thus keyed
correctly.

**API.** `GET /v1/runs/{id}/blackboard` (read scope) serves each key's head
by default, or every version with `?history=true`, filterable by `?key=` and
`?tag=` (AND), keyset-paginated. Read-only — the API never writes the
blackboard (steps do, in the worker), so ADR-002 is untouched. `ctl
blackboard <run-id>` mirrors it.

### Context assembly (as built, 12.3)

Context assembly is the declarative-read half the blackboard section deferred:
a step declares an *ordered* `context` spec whose sources are assembled into
the provider request **before the call**, deterministic given store state.
12.3 delivers the contract, the assembler, the engine middleware, and the
audit record; the compaction that shrinks an over-budget assembly is 12.4 and
the provider-window guardrail is 12.6.

**Where the spec lives.** `context` is a **step-envelope block**
(`dag.Step.Context *ContextSpec`), like `cache`/`validation`/`blackboard`, not
a field on the per-type config — it is uniform across the llm family (llm,
planner, agent), and materialized at instantiation onto
`run_steps.context_policy` (migration 0022, the `blackboard_policy`
precedent), so the engine reads the effective spec off the claimed row and
never reparses the snapshot. `context` on a non-llm-family step is a
submit-time error (`context_field_invalid`) — a spec on a step that issues no
provider request can never fire.

**Source schema.** Each `ContextSource` carries a `kind`
(`step_output | blackboard | retrieval | literal`), an optional `name` (the
label the assembled request and the audit event wrap it under; defaults to
`<kind>#<index>`, unique within the spec), the per-kind selector fields
(`step`+`path` for step_output; `key` XOR `tags` for blackboard; `retriever`+
`query`+`top_k` for retrieval; `text` for literal), an optional per-source
`max_tokens` cap, a `pinned` flag, and an `on_missing` policy. Wrong-kind
fields are rejected at submit; `step_output` sources are checked for
normal-edge ancestry through the **same** `classifyUpstreamRef` the 8.2
template lint uses (so the two produce identical codes and cannot drift).
Templating inside the block (`${{ ... }}`) is deliberately **unsupported** — a
dynamic query flows through a `retrieve` step and a `step_output` source,
keeping 12.3 out of the template engine (the 12.2 stance).

**Assembler.** `internal/contextmgr` is a leaf (imports `dag`, `blackboard`,
`retrieval`, `tokens` — all leaves — plus stdlib). `Assemble(spec, counter,
Sources) → Assembly` resolves each source in declaration order (precedence and
message order), renders it into a stable `<context name=… kind=…>…</context>`
block (blackboard values that are JSON strings render as their text, else
compact canonical JSON; tag-selected entries wrap as `<entry key=… version=…>`;
retrieval docs as `<doc id=…>`), joins the blocks with blank lines, and counts
everything with the step's counter. A per-source `max_tokens` cap truncates the
source's text on a rune boundary with a fixed `…[truncated]…` marker (a binary
search over the **final** string, since BPE counts are not additive, so the
result is guaranteed ≤ cap); a `pinned` source is never truncated (`pinned` and
`max_tokens` on one source is a submit error). Assembly is deterministic: the
same store state and corpus produce a byte-identical preamble and identical
counts (the 12.3 golden goldens rest on this).

**Missing-source policy.** A source that resolves to nothing — an upstream step
that did not succeed, a `step_output` pointer that does not resolve, a
blackboard key with no head, a tag matching no heads, a retriever returning no
results — is governed by `on_missing`: **`error` (the default)** fails the step
permanently *before any provider call* (`*contextmgr.MissingSourceError` →
ClassPermanent, the 8.2 strict-reference stance), while **`skip`** omits the
source and records it on the audit event. A source whose backend is unwired
(no board, no retriever registry) or names an unknown retriever, or an executor
that cannot inject context, is a permanent **config error** regardless of
policy (`*contextmgr.ConfigError`). A transport failure of a source read
(a store/retriever hiccup) is neither — it decides nothing and redelivers.

**Engine middleware.** The stage sits in `execute()` **after feedback
injection and before the cache read** (`engine/context.go`), so the assembled
preamble is prepended into the request the cache binding keys on — the
assembled context is a cache-key input by construction (ADR-011/014). The llm
executor implements two new hooks: `ContextInjector.WithContext(sc, preamble)`
prepends the preamble (a leading user message, or a prompt prefix — the
`WithFeedback` raw-message re-key mirror), and `PreflightCounter.PreflightTokens
(sc, counter)` counts the whole framed request. Blackboard and step-output
sources need no new wiring (the board bound in 12.2, the store); a `retrieval`
source resolves against the same retriever registry the retrieve executor uses
(`engine.WithRetrievers`, wired by `cmd/worker`).

**Audit.** Every context-bearing attempt appends a **`context_assembled`
event** before the request runs (`store.RecordContextAssembled`, the
`RecordModelDowngrade` fenced-record precedent — no state transition, fenced on
the caller's live claim, no new table): the payload carries the counter
fingerprint, the assembled `context_tokens`, the pre-flight request total, and
each source's disposition (`included | truncated | skipped`, its ref, tokens,
and pinned flag). Because a mock-driven request's `CountRequest` equals the
mock provider's reported `Usage.InputTokens` exactly, the recorded
`preflight_tokens` equals the attempt's input tokens on the offline fixture —
the ±5% accuracy criterion, exact by construction on the mock. The pre-flight
total is the number 12.6's window guardrail compares against the model context
window; 12.4's `context_revision` events hang off the same log.

**Recompute-on-mismatch note.** Assembly recounts every source on its rendered
text with the step's counter rather than trusting the blackboard entry's stored
`token_count` — the rendered wrapper framing differs from the raw value, and the
honest count is the one over the text actually assembled. ADR-014's
"cache `(count, counter_id)`, recompute on mismatch" is thus trivially honored:
the assembly count is always fresh under the current counter.

### Deterministic compaction (as built, 12.4)

Compaction is the shrink-to-budget half the assembly section deferred: when an
assembled context makes the framed request exceed a step's token budget, the
step's ordered strategy pipeline runs before the provider call, until the
request fits or a typed failure fires. 12.4 delivers the three deterministic
strategies, the budget guarantee, and the revision audit; the summarizer
strategy (12.5) and the provider-window default budget (12.6) build on it.

**Where the budget lives.** `budget_tokens` and `compaction` are new fields on
the same `context` envelope block (`dag.ContextSpec`), so they materialize onto
`run_steps.context_policy` (0022) with the sources — **no new migration**. A
budget is an explicit positive token cap; the pipeline is an ordered list of
strategies. In 12.4 a `compaction` pipeline with no `budget_tokens` is inert
(nothing to compare against); 12.6 defaults the budget from the model context
window so a bare pipeline fires. `context.Priority` on a source (default 0)
orders `drop_lowest_priority`.

**The budget is over the whole framed request, not the preamble.** ADR-014's
arithmetic identity — "assembled ≤ budget" is the same number as "assembled +
`max_tokens` ≤ window" — means compaction shrinks the movable part (the context
preamble) while measuring the *whole* request each pass through the engine's
`PreflightCounter` (the same count 12.3 records and 12.6 guards). Because BPE
counts are not additive, every strategy re-measures the framed request after it
acts rather than trusting a per-entry sum.

**The compaction unit is a source (one entry).** A source renders to exactly
one `<context>` block (a tag-selected blackboard source wraps several
`<entry>`, a retrieval source several `<doc>`, but as one block), so
`contextmgr.Render(entries)` reproduces 12.3's byte-identical preamble and the
strategies operate at source granularity — "keep the last N messages" is the
last N non-pinned sources, "drop lowest priority" evicts whole sources, "middle-
out truncate the oldest" truncates a whole source's rendered content.

**The three strategies (`internal/contextmgr/compact.go`), each while over
budget:**

- **`sliding_window` (n)** — one-shot: keep only the last `n` non-pinned
  sources in message order, drop the earlier ones. Pinned sources are always
  kept and do not count toward `n`.
- **`drop_lowest_priority`** — evict one non-pinned source at a time, lowest
  `priority` first (ties broken by later declaration), re-measuring after each,
  until the request fits or no droppable source remains.
- **`truncate_oldest` (min_tokens?)** — middle-out-truncate the oldest
  (lowest-index) non-pinned source above the floor with a fixed
  `…[elided]…` marker (a binary search over the rune split, guaranteed ≤ the
  local target), moving to the next-oldest while still over, until the request
  fits or every non-pinned source is at its `min_tokens` floor (default 0).

`summarize` is a **reserved-and-rejected** strategy name (the
`retry_on: validation_failed` precedent) until 12.5.

**Pinned exemption & the hard guarantee.** No strategy ever drops or truncates a
pinned source. After the pipeline, either the framed request is ≤ budget, or
`Compact` returns a typed `*contextmgr.OverBudgetError` — which the engine turns
into a **permanent** step failure *before any provider call* (ADR-006 row 21).
The error distinguishes `PinnedOnly` (the pinned set alone exceeds the budget —
the pins cannot be honored) from an ordinary shortfall (the non-pinned set could
not be shrunk enough). Overflow is never sent; a context-overflow 400 is
unreachable by construction for any budgeted step.

**Determinism & audit.** Every ordering is by declaration position or an
author-declared priority, and truncation is a deterministic binary search with a
fixed marker, so the same store state produces byte-identical compacted output
and identical revisions (the 12.4 property/determinism tests). Each strategy
that *runs* (i.e. the request was still over budget at its turn — a strategy
that no-ops because it was already under budget does not run) appends a
**`context_revision` event** (`store.ContextRevisionEvent`): the strategy, its
parameters, the framed-request tokens before/after, and the per-entry
drop/truncate actions. The N revision events plus the final `context_assembled`
event (now carrying `budget_tokens`, `raw_context_tokens`/`raw_preflight_tokens`
= the pre-compaction totals, `revisions` = the count, and the *post*-compaction
`sources` dispositions incl. the new `dropped`) are appended in **one fenced
transaction** under the claim (the 12.3 `RecordContextAssembled` grew a
`Revisions` slice), so the log reads raw → revision* → assembled in seq order,
queryable per step attempt. A hermetic property that survives: on the mock the
recorded post-compaction `preflight_tokens` still equals the attempt's reported
`Usage.InputTokens` exactly. An over-budget failure's summary rides the DLQ
reason (the descriptive dead-letter is the record; the per-revision events are
not persisted for a failed attempt — a documented limitation).

**Worked examples** (mock counter, illustrative token totals):

1. *Sliding window fits.* Five turns (~90 tok each) + a pinned instruction
   (~20 tok), budget 200, pipeline `[sliding_window n=3]`. Raw request ~370 >
   200 → the window keeps turn₃,₄,₅ + the pin (~290)… still > 200 → with a
   trailing `truncate_oldest`/`drop_lowest_priority` the oldest survivor is
   truncated or dropped until ≤ 200. `context_revision`: `sliding_window`
   (dropped turn₁,turn₂), then the trailing strategy. Result: pin + recent turns,
   under budget, dropped turns absent from the prompt.
2. *Pin alone too big.* Two pinned 200-token literals, budget 5, any pipeline.
   No strategy may touch a pin → `OverBudgetError{PinnedOnly:true}` → the step
   dead-letters permanent, zero provider calls, no `context_assembled` event.
3. *Pure guardrail.* A budget with an **empty** pipeline and an over-budget
   assembly → immediate `OverBudgetError{Applied:[]}` (no strategy ran) → the
   same permanent pre-call failure. This is the shape 12.6 reuses for the
   provider-window check.

## Consequences

**Easier.**

- Any LLM step gets window-safety for free once 12.6 lands — no per-workflow
  context code, and "context overflow 400" becomes unreachable by construction.
- Budgets, assembly caps, compaction, and guardrails all speak one honest token
  number instead of a 30%-wrong heuristic, so their tests can assert real
  accuracy (±5%) rather than "roughly."
- The counter is a pure leaf, so it is trivially unit-testable and reusable by
  the limiter/budget refinement (12.6) and any future context-aware feature
  without a dependency tangle.

**Harder / accepted costs.**

- Two new dependencies (tiktoken-go + its offline loader) and ~6.7 MB of
  embedded BPE rank tables in the binary. Accepted: exact OpenAI counting is
  worth it, and the tables are `go:embed`'d so there is no runtime download or
  network dependency.
- Anthropic counts are an *estimate*, not exact, and the calibration factor
  needs one offline recording pass against the `count_tokens` API to be trusted
  to ±5%. Until that pass runs, the Anthropic factor is a seeded default and its
  accuracy box is pending. This is inherent — Anthropic publishes no tokenizer —
  and the ±5% headroom in the default budget is sized to absorb the residual
  slop.
- Chat framing constants are provider-versioned and will drift as providers
  change their accounting; the `@N` fingerprint suffix and the accuracy fixtures
  are the tripwire that catches drift, at the cost of an occasional re-record.

## Alternatives considered

- **chars/4 everywhere (no tokenizer).** Rejected: 20–40% error on code/JSON,
  far outside ±5%, and a single under-count is a hard provider 400 that 12.6
  exists to prevent. Kept only as the last-resort fallback for unknown models,
  where being approximately right beats refusing to count.
- **Call the provider count-tokens / prompt_tokens on the hot path.** Rejected:
  a per-step network round-trip before every LLM call, coupling compaction to
  provider availability and latency, to compute a number a local tokenizer gives
  for free. Kept as an *optional, flagged, off-by-default* calibration aid only.
- **Vendor a reverse-engineered Claude tokenizer.** Rejected: none is public or
  stable; a wrong "exact" tokenizer is worse than an honestly-calibrated
  estimate whose error the budget headroom is sized for.
- **Count at the provider adapter (in `internal/llm`).** Rejected: too late —
  the point of counting is the *pre-flight* decision (assemble-to-budget,
  compact, guard) that happens before the request is built and sent. The counter
  is a pre-execution primitive, not a provider concern.
- **Store only text and recount on every read.** Rejected: 12.3's
  byte-identical-assembly goldens and 12.4's before/after audits need a stable,
  stored count; recomputing per read is both wasteful and a determinism hazard
  if the counter ever changes mid-run. Caching `(count, counter_id)` and
  recomputing only on fingerprint mismatch is the middle path.
