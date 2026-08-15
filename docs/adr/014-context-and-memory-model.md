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
