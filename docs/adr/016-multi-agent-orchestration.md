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

### What is deferred (M14.2+)

- The autonomous tool-execution / ReAct loop, and offering tools to the model.
- Planner-injected agent steps (only authored agent steps are resolved).
- Handoff conventions: the blackboard message **thread** (author/role/iteration
  metadata), per-agent "conversation view" context presets, and explicit pinned
  handoff payloads (14.2, ADR-014's blackboard).
- **Loop-edge runtime** — executing M1's marked loop edges by unrolling each
  iteration through `ExpandRun` (14.3, ADR-015), with the critic's feedback
  threaded into the next iteration's prompt; loop termination and the
  `loop_exhausted` event.
- Run-level guards and no-progress detection (14.4).
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
