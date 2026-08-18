# ADR-016: Multi-agent orchestration — agent roles, handoff, loop unrolling

- **Status:** Accepted
- **Date:** 2026-08-16
- **Ticket:** ROADMAP.md ticket 14.1 (opens Milestone 14)

<!--
This ADR opens M14. Ticket 14.1 fixes the agent model — the `agents` section,
the `agent` step type, the deterministic merge, and the tool-allowlist contract
— so the later M14 tickets conform to it:

  - 14.2 handoff conventions & the blackboard message thread
  - 14.3 loop-edge runtime (unrolling via ExpandRun)
  - 14.4 run guards & termination policies
  - 14.5 the flagship research → write → critique example

Sections tagged "(arrives in 14.x)" state the contract now; those tickets add
"### … (as built, 14.x)" subsections under ## Decision as they land, the way
ADR-014 / ADR-015 grew across their milestones.
-->

## Context

Differentiator #5 is workflows modeled as multiple **agents** with distinct
roles — a researcher, a critic, a writer — that hand off to one another and,
where the work needs it, loop: a critic rejects the writer's draft and sends it
back with feedback, bounded and durable. M14 builds this on everything before
it. An agent turn is an LLM call (M8) with validators (M11), a context spec
(M12), budgets (M10), and — for loops — the runtime graph expansion of M13. The
milestone adds almost no new execution machinery; its job is to compose the
existing machinery under a role abstraction and a handoff convention.

Ticket 14.1 must fix the **agent model**: how a role's defaults are declared,
how an `agent` step references a role and overrides its defaults, and how the
resolved step reaches the executor. The forces:

- **No new executor pipeline.** An agent turn is an LLM call. The planner
  (ADR-015, 13.3) already showed how to reuse the whole llm pipeline — routing,
  request framing, response cache, rate limiting, cost estimate, semantic-retry
  feedback, context assembly, window guard — by projecting a per-type config
  onto an `LLMConfig` in one place (`llmConfigView`). An agent must reuse that
  same seam, not fork it.

- **The merge touches the envelope, not just the config.** A role's defaults
  are not only model-call fields (system prompt, model, fallbacks). They include
  a **default validator chain** and a **default context spec** — envelope-level
  policies the engine resolves from the run's materialized `run_steps` rows
  (`validation_policy`, `context_policy`), never re-read from the definition at
  claim time. For a role's default validators/context to take effect, they must
  be merged into those columns *before* materialization.

- **Determinism.** "Agent defaults merge deterministically" is an acceptance
  criterion. The precedence rule must be simple enough to test exhaustively and
  stable across runs.

- **A tool allowlist, without a tool loop.** A role declares an allowed
  toolset, and a tool call outside it must be rejected with a typed error. But
  agentloom has no autonomous tool-execution (ReAct) loop today — an llm step
  makes exactly one model call and does not execute the tools the model names.
  14.1 must honor the allowlist contract without building that loop.

The persistence and scheduling groundwork already fits. ADR-004 gives every run
its own materialized graph copy, so a role's defaults can be baked into an agent
step's row at instantiation with no shared-definition mutation. ADR-002's
event-driven completion and ADR-015's `ExpandRun` are what 14.3's loop
unrolling will use; 14.1 does not touch them.

## Decision

### The `agents` section and the `agent` step (14.1)

We add an optional top-level **`agents`** section to the workflow definition: a
`map[string]*AgentDef` library of named roles, decoded/encoded/validated exactly
like the `templates` library (ADR-015). A role (`AgentDef`) carries a role's
defaults:

- **Config-level:** `system` (the role's system prompt), `model`,
  `model_fallbacks`, `tools` (the allowed toolset — a list of tool names),
  `max_tokens`, `temperature`, `output_format`.
- **Envelope-level:** `validation` (a default validator chain) and `context` (a
  default context spec).
- **Metadata:** `role` (a human-readable name, used by 14.2's handoff thread).

A role has **no prompt of its own** — the task text always comes from the step.

The `agent` **step type** (already reserved in the ADR-003 catalog) references a
role by name and supplies the task plus any per-field overrides. Its config
(`AgentConfig`) carries `agent` (the required ref), the task (`prompt` XOR
`messages`), and optional overrides of the role's config-level fields. The
step's **envelope** `validation`/`context` blocks are the step-level overrides
of the role's envelope defaults — the existing `Step.Validation`/`Step.Context`
fields, not new config keys.

An `agent` step is **llm-family** (`IsLLMFamily()` already returns true for it):
it shares the `/text` default validation target, `output_format` eligibility,
and the semantic-retry feedback template.

### The merge: deterministic, per-field, at instantiation

`dag.ResolveAgentStep(def, step)` is the single pure function that merges a
role's defaults under a step's overrides into an effective step. The rules:

1. **Per-field precedence.** A value present on the step wins; otherwise the
   role's default applies; otherwise the engine default. Fields merge
   independently (shallow, per-field replacement — no deep merge within a
   field), so the outcome is deterministic and testable.
2. **The task is the step's.** `prompt`/`messages` are never taken from the
   role (a role has no prompt).
3. **Envelope blocks replace wholesale.** A step-level `validation` (or
   `context`) block replaces the role's entirely; an absent block inherits the
   role's. This is block-level, not field-level, replacement — deliberately
   simple.
4. **Tools inherit unless explicitly overridden.** A nil step toolset inherits
   the role's; an explicit list (including an empty `[]`, which forbids all
   tools) overrides.
5. Retry, timeout, cache, budget, and blackboard are step-level only — a role
   has no opinion on transport policy — and pass through unchanged.

The merge runs at **run instantiation** (`store.instantiate`), producing a
fully-configured step whose config is a fully-populated `AgentConfig` and whose
envelope carries the merged validation/context. The materialized `run_steps` row
is therefore an ordinary llm-family step: the executor projects its config onto
an `LLMConfig` (`llmConfigView`, the planner seam), and the engine's validate
and context stages resolve the merged policies from `validation_policy` /
`context_policy` with **no agent-specific code**. The `agents` section is needed
only at validation and instantiation — never at claim time (unlike `templates`,
which the runtime map expansion re-reads). Instantiation is also where an
agent step could be injected by a planner in the future; 14.1 resolves only
**authored** agent steps, and a planner-injected agent step is deferred.

Validation runs `ResolveAgentStep` too, checking that every `agent` ref names a
declared role (`agent_ref_unknown`) and that the **merged** step is a valid
model call — model present, exactly one of prompt/messages. A role's own
defaults (its `model_fallbacks`, `output_format`, `validation`, `context`) are
validated by reusing the existing llm-family checks over a synthetic llm step,
and its role/tool names are shape-checked (`agent_section_invalid`). A
step-output context source on a **role's** default context is field-checked but
not ancestry-checked — the role is not a graph node; a step-level context
override gets full ancestry via the ordinary graph check.

### The executor: the planner pattern, plus tool-allowlist rejection

`exec.AgentExecutor` embeds `LLMExecutor` and overrides only `Type()` →
`"agent"`, `PluginManifest()` (distinct plugin identity, so an agent never
shares a response-cache entry with an llm or planner step; cacheable +
cost-bearing), and `Execute()`. The `llmConfigView` agent branch projects the
merged `AgentConfig` onto an `LLMConfig` — including its `system` prompt, which
is a new (additive) `LLMConfig.System` field framed onto the provider request's
system field, useful for plain llm steps too. Every optional hook (resource
claim, cost estimate, cache binding, model downgrade, feedback injection,
context injection, preflight tokens) is inherited verbatim and re-targets
through that one projection, exactly as the planner's does. The system prompt is
a cache-key input, so two agents differing only in system prompt never collide.

The tool allowlist is enforced by **rejection only** in 14.1. `Execute` runs the
embedded llm completion, then inspects the model's `tool_use` blocks: a tool
named outside the role's allowed toolset fails the step **permanently** (a
deterministic function of the config — no identical retry can pass, and the
allowlist is a hard authoring boundary). An empty/absent allowlist forbids every
tool. The tools are **not offered to the model** (the request carries no tool
definitions) and **not executed** — agentloom has no autonomous tool-execution
loop yet. Offering the allowlisted tools to the model and running them is
deferred; 14.1 ships the declaration and the rejection guardrail, which is
enough to keep a misbehaving or future tool-emitting path inside the boundary.

### Handoff conventions & the message thread (as built, 14.2)

14.2 turns the blackboard into the handoff substrate the milestone needs — a
standardized **message thread** agents write to and read from — composing the
12.2 blackboard and the 12.3 context assembly with **no migration, no new config
var, and no new metric**. The append-only-per-key blackboard is exactly a
conversation log: each agent turn appends a new *version* of a thread key, and
`History` reconstructs the ordered conversation.

**The write side — auto-append (default-on for agents).** Every `agent` step,
on success, appends its turn to the run's thread key (`blackboard.DefaultThreadKey`
= `"thread"`) atomically with its success CAS, reusing the completion-transaction
blackboard-write path (`engine.planThreadAppend` / `applyThreadAppend`). The
stored value is a `blackboard.ThreadMessage` — `{author, role, iteration,
content, created_at}` — tagged `TagThread`:

- `author` is the step id; `role` is the agent's role, carried onto the merged
  `AgentConfig.Role` by `ResolveAgentStep` (the role's `Role`, else the agent
  ref); `iteration` is parsed from the step's `#k` instance suffix (0 for an
  authored step — 14.3's loop unrolling mints the `#k` instances that carry a
  real iteration); `content` is the step's `/text` output (falling back to the
  whole output, so auto-threading never fails a succeeded step).
- The append is unconditional and unfenced (the success CAS already fenced this
  completion), token-counted with the step's counter, and attributed to the
  step — so it also rides the ordinary `blackboard_updated` event and the run's
  token accounting. It is default-on for agent steps only (a plain llm step does
  not thread); making it configurable (a custom key, an opt-out, or opting an
  llm step in) is a small later extension, not needed for the handoff flows.

**The read side — the `thread` context source ("conversation view").** A new
context source kind `thread` (ADR-014's context assembly) reads a thread key's
full **History** — not just the head, unlike a `blackboard` source — and renders
the turns oldest-first as `<message author=… role=… iteration=… version=…>`
elements under one `<context kind="thread">` block, with an optional `role`
filter (`ContextSource.Role`) selecting a single role's turns. A role's default
`context` spec (14.1) carries the "conversation view" preset — a `thread` source
plus a pinned handoff source — so every agent in that role inherits it and a
downstream agent sees the prior turns without per-step wiring.

**Explicit handoff payloads** are the existing pinned blackboard write: an agent
writes a structured payload to a key with `pinned: true` (the 12.2 sugar); the
next agent reads it through a pinned `blackboard` context source. Because the
thread source is one compaction unit and the pinned handoff is a separate pinned
source, a long conversation compacts (e.g. `truncate_oldest`) while the pinned
handoff survives — the ADR-014 pinning guarantee carries the handoff contract.

**Decisions.** The thread is one key whose versions are the messages (not a key
per message), so the append-only store *is* the ordered log and `History` reads
it — no ordering scheme, no cross-key sort. Metadata rides in the message value
(plus author/attempt/created_at already on the row) and the `thread` tag, so no
schema change is needed. Auto-append is default-on for agents (the "standardized/
auto" contract) rather than an opt-in block. The thread source renders as one
compaction unit (the whole conversation), which satisfies "long thread compacts,
pinned handoff survives" without changing the 1-source-1-entry assembly model;
per-message windowing is deferred.

### Loop-edge runtime — unrolling (as built, 14.3)

14.3 executes M1's marked loop edges by **unrolling one iteration at a time
through `store.ExpandRun`** — the same primitive planners (13.3) and maps (13.4)
use, with an engine-generated delta. A loop is **edge-driven, not step-typed**:
any completing step whose *authored* node (its id with the `#k` instance suffix
stripped) has an outgoing marked loop edge is a loop source. The completion
transaction evaluates the loop edge's `condition` against the step's output and
branches — the loop logic sits beside the map path in `completeSuccess`, no new
executor. **No migration, no new config var, no new metric.**

**The unrolling model.** The loop body is the normal-edge span from the loop
edge's `to` (the entry) to its `from` (the loop source), inclusive. On a
*continue* (`condition` true, iteration `k < max_iterations`), a pure
`dag.GenerateLoopExpansion` builds the iteration-`k+1` delta and the completion's
`ExpandRun` applies it:

- **after-splice** `<current loop-source instance> → <entry>#k+1` — readies the
  next iteration's entry when the origin's fan-out runs.
- internal body edges cloned with the `#k+1` suffix; the loop edge itself is
  **not** cloned (the next loop-back is detected lazily when `<source>#k+1`
  completes).
- **before-splice** `<source>#k+1 → <exit target>` for each non-loop out-edge of
  the loop source, carrying its `when`. This widens the pending exit target's
  `remaining_deps`, so the completing iteration's own exit edge resolves and the
  exit target waits on exactly the newest iteration until one fires it. The clean
  idiom is an **unconditioned exit edge**: the loop condition is the sole
  brancher, and the before-splice keeps the exit target pending across
  iterations; a conditioned exit must be false whenever the loop continues.

**Body-only reference rewriting** is the one divergence from map's
rewrite-everything (`mapexpand.go`): a loop body may reference pre-loop steps, so
only `${{ steps.<x> }}` / `step_output` context references to *body members* gain
the `#k+1` suffix; references to steps outside the body and to `run.params` are
left untouched. Agent body steps are resolved (`ResolveAgentStep`) before
cloning, so an injected agent instance carries the same merged, self-describing
config an authored agent step gets at instantiation.

**Feedback threading reuses 14.2 with no bespoke path.** The critic is an agent,
so its verdict is auto-appended to the run `thread`, tagged with
`Iteration = threadIteration("#k")`; the writer's role context preset (a `thread`
source filtered to the critic role, `on_missing: skip`) surfaces it, and
`rewriteInstanceContext` leaves thread sources untouched so every cloned writer
instance re-reads the growing thread. Each revision's prompt therefore carries
the critic's prior notes.

**Termination.** A new **loop-edge-only field `on_exhausted` (`proceed` default |
`fail`)** governs the cap. When `condition` is true but `k >= max_iterations`:
`proceed` records a `loop_exhausted` event inside the completion transaction and
lets the ordinary fan-out route the loop source's normal exit edges (so an
unconditioned exit runs `publish`); `fail` records the event and dead-letters the
loop source permanently (the run fails via its `on_failure` disposition). A
`condition`-false completion is an ordinary *exit* (no expansion, the exit edge
fires); a malformed condition is a deterministic permanent step failure (like a
`when` error). A rejected loop delta is **always permanent** (engine-generated —
no model to re-prompt), routed like a map rejection (`expansion_cap_exceeded` for
a run-guard cap, else `loop_expansion_invalid`).

**Iterations do not nest depth.** A loop's iterations are sequential, not nested,
so `ExpandRun` pins every iteration's instances to the loop source's *authored*
depth (a new `ExpandRunArgs.DepthOverride`): each instance carries a constant
depth and the loop is bounded by `max_iterations`, **not** by `MaxDepth` (which
would otherwise cap a loop at 4 iterations). `max_iterations` semantics: expand
iff the completing instance's iteration `k < max_iterations`; at `k ==
max_iterations` the loop exhausts (so `max_iterations` is the maximum number of
loop-backs, and total body runs = `max_iterations + 1`).

**Crash-safety is inherited for free.** The loop expansion rides the same fenced
completion transaction as a planner's (13.3/13.5): `SucceedStep` (claim-fenced) →
`ExpandRun` → fan-out, all atomic. A zombie loop source is fenced at
`SucceedStep` and never expands; a crash after `ExpandRun` but before commit
rolls the whole iteration back, so a resumed takeover completes the *same*
iteration exactly once — no duplicate or half-expanded iteration.

**Decisions.** Edge-driven detection (any step type may be a loop source, keyed
on an authored loop edge, not a step type); unconditioned exit as the idiom (the
before-splice, not complementary `when`s, keeps the exit pending); reuse the
14.2 thread for feedback (no per-iteration feedback record); constant iteration
depth via `DepthOverride` (loops bounded by `max_iterations`, not `MaxDepth`);
`on_exhausted` as a loop-edge field (proceed routes to the exit, fail
dead-letters); permanent rejection routing (engine-generated deltas are not
re-promptable). **Tests:** `dag/loopexpand_test.go` (body computation, body-only
rewriting, splice structure, the delta passes `ValidateExpansion`); engine
integration `TestLoopWriterCriticConverges` (reject twice → 3 writer instances,
loop provenance, constant depth, feedback threaded into each revision),
`TestLoopCapExhaustedProceed` / `TestLoopCapExhaustedFail` (the cap event under
both policies), `TestLoopExpansionAtomicOnFailpoint` (abort after `ExpandRun`
rolls back — no injected iteration). `critic_loop.json` updated to the runnable
writer⇄critic shape; kitchen sinks carry `on_exhausted`.

### Run guards & termination policies (as built, 14.4)

14.4 makes every run-level halt a **typed disposition with an explanatory event**
carrying "which limit, current value, configured cap". Three guard families
already enforced themselves and keep their richer events; 14.4 fills the two gaps
and adds the opt-in no-progress detector. **No migration, no new config var, no
new metric** — the caps and deadline are materialized run data (13.2 / 5.6), and
the new events ride the `events` table like `loop_exhausted`.

**The guard → event map.** Each guard class has one canonical event, so nothing is
duplicated:

| Guard | Enforced at | Halt disposition | Event |
|---|---|---|---|
| `max_total_steps` / `max_expansions` / `max_depth` / per-expansion cap | expansion (`ValidateExpansion` inside `ExpandRun`, 13.1/13.2) | permanent step dead-letter → run fails | **`guard_tripped`** |
| `max_wall_clock` (5.6) | **claim time (new)** + reconciler (safety net) | run **cancel** (reason `deadline_exceeded`) | **`guard_tripped`** |
| run/step budget (10.3) | claim time (budget check) | park \| fail | `budget_exceeded` |
| loop `max_iterations` (14.3) | loop source completion | proceed \| fail | `loop_exhausted` |
| loop no-progress (opt-in, new) | loop source completion | proceed \| fail | **`loop_no_progress`** |

Halt vocabulary: `fail` (dead-letter, run fails via `on_failure`), `park`
(resumable), `cancel` (the wall-clock disposition — 5.6's choice, kept: a deadline
is absolute, so there is nothing to resume), `proceed`/`exit` (route the loop
source's normal exit edges). Expansion caps `fail` rather than `park` because a
cap is not runtime-adjustable — parking would be a dead end.

**Structured cap breaches.** `dag.ExpansionVerdict` gained `Breaches
[]CapBreach{Limit, Current, Cap}`, populated alongside the four
`expansion_cap_exceeded` issues, so the engine renders the exact numbers into
`guard_tripped` instead of parsing the free-text issue message. `store`'s
`*ExpansionRejectedError.Breaches()` surfaces them; both rejection routes
(`routeExpansionRejection` for a planner, `routeMapRejection` for a map/loop)
append one `guard_tripped` per breach in a short transaction before the
dead-letter (the completion transaction already rolled back).

**Claim-time wall-clock guard.** `guardDeadline` (`engine/guard.go`) is the first
pre-execution stage in `execute()`: a single time compare on the claim's origin
(`ClaimOrigin.DeadlineAt`, read from the same locked run row `LockRun` already
returns — no extra read). A run at or past its `deadline_at` is cancelled
(`cancelRunTx`, reason `deadline_exceeded`) with a `guard_tripped(max_wall_clock)`
event in one transaction, then the claimed step is settled `cancelled` through the
5.6 cancelling-run path. This halts a runaway loop **at the next claim** rather
than waiting for the reconciler's periodic sweep; the reconciler stays the safety
net (a deadline-exceeded run whose workers are all idle or dead) and records the
same event. Both compute `current` (elapsed = now − started_at) and `cap`
(deadline − started_at) in whole seconds.

**No-progress detection (opt-in).** A loop edge may carry a `no_progress` block
(`{step?, path?, policy?}`) — disabled by default (nil). When present, the loop
source's completion hashes the compared step's output (SHA-256 over canonical
JSON, at an optional RFC-6901 pointer; `step` defaults to the loop source, and
must be a loop-body member) at iteration `k` and `k−1`; two identical hashes mean
the loop is spinning, so it terminates early with a `loop_no_progress` event and
the policy's disposition (`proceed` routes the exit edges like a condition-false
exit; `fail` dead-letters the loop source). It is **purely additive**: evaluated
only when the loop would otherwise continue (`k ≥ 1`, condition true, under the
iteration cap), and any obstacle to comparing — an unresolvable pointer, an
unreadable prior instance — skips the check (the loop continues) rather than
introducing a new failure. Precedence: exit → exhaust → no-progress → continue.

**Decisions.** One `guard_tripped` event for the expansion caps and the wall
clock, reusing the richer `budget_exceeded` / `loop_exhausted` for those families
(no duplication); claim-time deadline enforcement so a runaway loop halts without
the reconciler, with the reconciler kept as the safety net; wall-clock halts stay
`cancel` (5.6), not park; structured breaches over message-parsing; no-progress
purely additive and opt-in (it can only force an *earlier* exit, never a new
failure mode); no new metric (the events are the observability surface, as in
14.3). **Tests:** `dag` — `no_progress` decode/validate matrix (forbidden on a
normal edge, policy enum, pointer syntax, body-membership), `ExpansionVerdict`
`Breaches` populated per cap; `engine` unit — `outputHash` (pointer /
canonicalization), `deadlineGuardEvent`, `capBreachUnit`, `loopInstanceID`;
engine integration `guard_integration_test.go` — the runaway writer⇄critic loop
halted by each guard in isolation (`TestGuardMaxTotalStepsHaltsRunawayLoop`,
`TestGuardMaxExpansionsHaltsRunawayLoop` → dead-letter + `guard_tripped` with the
right current/cap; `TestGuardWallClockHaltsLoopAtClaim` → manually driven on a
fake clock, deadline crossed mid-loop → run cancelled + `guard_tripped`), the
no-progress detector under both policies + disabled-by-default
(`TestLoopNoProgress{ProceedExits,FailDeadLetters,DisabledByDefault}`), and the
reconciler deadline test extended to assert the same guard event. Kitchen sinks
carry a `no_progress` guard.

### What is deferred (M14.4+)

- The autonomous tool-execution / ReAct loop, and offering tools to the model.
- Planner-injected agent steps (only authored agent steps are resolved).
- Thread configuration: a custom thread key, an opt-out, or opting a plain llm
  step into the thread; per-message (turn-level) compaction windowing.
- Loops nested inside a map body (nested expansion), and a body member other than
  the loop source having an edge leaving the body (rejected defensively).
- Body-membership re-check of a `no_progress.step` on a *planner-injected* loop
  (degrades safely — the guard simply never fires — so it is not re-validated
  against the merged graph); a guard-trip metric (events are the surface).
- The flagship research → write → critique example (14.5).

## Consequences

- **The whole llm pipeline is reused with one projection branch and a thin
  Execute override.** An agent turn inherits routing, caching, budgets,
  validators, context, and semantic retries for free — the composition M14 was
  designed to be.
- **A role's defaults are real, active policy, not documentation.** Because the
  merge lands in the materialized rows, a role's default validators and context
  spec flow through the ordinary engine stages. Changing a role changes every
  step that references it, resolved once at instantiation.
- **Determinism is a single pure function.** `ResolveAgentStep` is unit-tested
  for override precedence and purity (no mutation of the role or the authored
  step), which is the "defaults merge deterministically" acceptance.
- **The tool allowlist is a guardrail, not yet a capability.** Declaring
  `tools` and rejecting out-of-set calls is honest and testable, but until the
  tool loop lands the allowlist mainly bounds what a model *could* do rather
  than enabling tool use. This is called out here so 14.x does not mistake the
  guardrail for a working tool-calling agent.
- **No migration, no new config var, no new metric.** The `agents` section is
  definition data; the merge reuses the existing materialization; the agent
  executor reuses the llm executor's instruments. `LLMConfig.System` is additive
  (omitempty), so existing definitions and cache entries are unaffected.
