# ADR-003: Workflow definition format & versioning

- **Status:** Accepted
- **Date:** 2026-08-08
- **Ticket:** ROADMAP.md ticket 1.1

## Context

The workflow definition is the system's central contract, with four consumers
that must never drift apart:

- the **API** accepts and validates definitions (M6),
- **Postgres** snapshots a definition per run — the per-run graph copy (M2),
- **planner steps** extend the instantiated graph at runtime (M13), and
- the **visual builder** serializes to and from the same format (M17).

Several forces are in tension:

- **Strictness vs. evolvability.** A lenient decoder that ignores unknown
  fields silently swallows typos (`max_iteration` for `max_iterations`) and
  lets builder and engine drift apart. But the format *will* grow: retry
  policy (M5), caching (M9), budgets (M10), validators (M11), context specs
  (M12), and an `agents:` section (M14) are all scheduled additions.
- **The engine must round-trip data it does not understand.** The builder
  stores node positions and other layout state inside the definition; the
  engine ignores it semantically but must preserve it byte-for-byte — which
  contradicts a blanket unknown-field rejection unless carved out explicitly.
- **Loops are required but cycles are poison.** The critic⇄writer revision
  loop (M14) needs cyclic authoring, while every durability mechanism —
  per-run graph rows, `remaining_deps` counters, readiness computed in
  completion transactions (ADR-002) — assumes an acyclic instance graph.
- **One schema, one truth.** The JSON Schema published for docs and UI forms
  must be provably in sync with what the engine actually decodes.

Deferring these decisions is not an option: M1.2–1.6 implement this format
directly, M2 freezes it into Postgres rows, and every later milestone extends
it.

## Decision

Workflows are defined as **JSON documents**. Go structs in `internal/dag` are
the **source of truth**; the JSON Schema is **generated** from them
(invopop/jsonschema) into `docs/schema/` and drift-checked in CI (ticket 1.2).
All JSON field names are `snake_case`.

### Top-level shape

```json
{
  "schema_version": 1,
  "name": "support-triage",
  "description": "Classify a ticket, draft a reply, get approval.",
  "params": {
    "ticket_id": {"type": "string", "required": true},
    "tone": {"type": "string", "required": false}
  },
  "steps": [ ... ],
  "edges": [ ... ],
  "ui": { ... }
}
```

- `schema_version` (required, integer) — see [Versioning](#versioning-and-unknown-fields).
- `name` (required) — human-readable identifier; the stored-definition
  registry (M6) adds its own versioning on top, which is orthogonal to
  `schema_version`.
- `description` (optional).
- `params` (optional) — declares the run parameters callers may supply at
  submit time. Each entry has a `type` (`string | number | boolean | object |
  array`) and `required` flag. Declared params are exposed to CEL edge
  conditions and (from M8) input templating as `run.params.<key>`. Validation
  of submitted values against declarations happens at run creation, not
  definition validation.
- `steps` (required, non-empty) and `edges` (required, may be empty).
- `ui` (optional) — engine-opaque builder state; see
  [The `ui` block](#the-ui-block).

Later ADRs add top-level sections additively (e.g. `agents:` in ADR-016).

### Steps

Every step has:

```json
{"id": "draft_reply", "type": "llm", "config": { ... }}
```

- `id` (required) — unique within the definition, matching
  `^[a-z][a-z0-9_-]{0,63}$`. The characters `#` and `.` are **excluded by
  construction and reserved**: `#` because loop unrolling (M14) and map
  fan-out (M13) name runtime instances `{id}#k`, and `.` because CEL and
  templating use it as a path separator (`steps.draft_reply.output`).
- `type` (required) — one of the catalog below. Unknown types are a
  validation error.
- `config` (required for most types) — a JSON object whose shape is **typed
  per step type**. The decoder (1.2) decodes `config` into the Go config
  struct registered for the `type`, so unknown or mistyped config fields
  produce path-qualified errors like any other field.

Later milestones add optional per-step fields alongside `config` — `retry`
(ADR-006, M5), `cache` (ADR-011, M9), `budget`/`model_fallbacks` (ADR-012,
M10), `validators` (ADR-013, M11), `context` (ADR-014, M12). Each lands as an
additive optional field specified by its own ADR; this ADR is not superseded
by those additions.

#### Step-type catalog

M1 validates the *shape* of each config (required keys present, correct
types). Execution semantics belong to the milestone that implements the
executor; config shapes marked *provisional* are finalized by the owning ADR
and may gain fields there.

**`llm`** — one model call (executor: M8). Requires `model` and exactly one
of `prompt` or `messages`.

```json
{"id": "classify", "type": "llm", "config": {
  "model": "anthropic/claude-sonnet-5",
  "prompt": "Classify this ticket: ${{ run.params.ticket_id }}",
  "max_tokens": 1024,
  "temperature": 0
}}
```

**`tool`** — one tool invocation through the tool SPI (executor: M8).
Requires `tool`; `input` is the tool's input payload (templated from M8).

```json
{"id": "fetch_ticket", "type": "tool", "config": {
  "tool": "http_request",
  "input": {"method": "GET", "url": "https://support.internal/api/tickets/${{ run.params.ticket_id }}"}
}}
```

**`retrieve`** — retrieval query through the retriever SPI (executor: M8).
Requires `retriever` and `query`; `top_k` defaults to an executor-defined
value.

```json
{"id": "find_similar", "type": "retrieve", "config": {
  "retriever": "pg_fulltext",
  "query": "${{ steps.classify.output.summary }}",
  "top_k": 5
}}
```

**`map`** — runtime-sized fan-out over a list, instances created via the
expansion machinery (executor: 13.4; contract in ADR-015). Requires `items`
(an expression yielding the list, held as raw JSON so a whole-expression
template renders an array into it) and `body` (the name of a **definition-level
`templates` entry** to instantiate per item — added in 13.4); optional
`max_items` caps the fan-out width. A `gather` step (a fan-in barrier emitting
its resolved ordered `items` array) is generated per map to collect the ordered
per-instance results; authoring one is legal (a literal array) but rare.

```json
{"id": "summarize_each", "type": "map", "config": {
  "items": "${{ steps.find_similar.output.docs }}",
  "body": "summarize_one"
}}
```

**`templates`** (top-level, optional; ADR-015, 13.4) — a `name → {steps, edges}`
library of reusable sub-graphs a `map` body instantiates. Templates are a
library, **not active steps**: they are validated (each a self-contained
single-sink sub-graph) but never instantiated at run creation, and ride in the
run's definition snapshot so a map expansion reads them at runtime. A template
body references the current item through the reserved `${{ item }}` /
`${{ item_index }}` roots (valid only inside a template) and its own steps
through ordinary `${{ steps.<id>.output }}` refs; the engine rewrites both per
instance (`item` → the map's output, `steps.x` → `steps.x#k`).

**`planner`** — an LLM call whose validated output (a `PlanOutput` document,
ADR-015) injects new steps/edges into the running graph (executor: 13.3;
contract in ADR-015). Requires the same keys as `llm`, plus an optional
`max_added_steps` per-expansion cap (ADR-015).

```json
{"id": "plan_research", "type": "planner", "config": {
  "model": "anthropic/claude-sonnet-5",
  "prompt": "Break this goal into research steps: ${{ run.params.goal }}"
}}
```

**`agent`** — an LLM step bound to a named role from the `agents:` section
(executor: M14, ADR-016; config provisional). M1 requires `agent`; reference
validation arrives with the `agents:` section itself.

```json
{"id": "critique", "type": "agent", "config": {
  "agent": "critic",
  "prompt": "Review the draft on the blackboard."
}}
```

**`human_approval`** — parks the run (no lease held) until a human decides
(executor: M15; config provisional). Requires `prompt`.

```json
{"id": "approve_send", "type": "human_approval", "config": {
  "prompt": "OK to send this reply to the customer?"
}}
```

**`join`** — synchronization barrier for fan-in. Requires `mode`, one of
`all` (default fan-in: wait for every incoming edge to resolve) or `any`
(fire on the first successful parent). Semantics below.

```json
{"id": "gather", "type": "join", "config": {"mode": "all"}}
```

**`branch`** — exclusive routing. The branch step itself is a pass-through
(its output is its rendered `input`, or its primary upstream output if
`input` is omitted); what makes it a branch is the **edge-firing rule** on
its outgoing edges, defined below. `config` may be empty.

```json
{"id": "route", "type": "branch", "config": {}}
```

**`noop`** / **`echo`** — test executors (M4). `noop` requires no config and
produces an empty output; `echo` returns its `input` as output. Further test
executors (`sleep`, `fail_n_times`, …) register the same way in M4 without
touching this ADR.

```json
{"id": "start", "type": "noop"}
```
```json
{"id": "mirror", "type": "echo", "config": {"input": {"hello": "world"}}}
```

### Edges

```json
{"from": "classify", "to": "draft_reply", "when": "output.category == 'billing'"}
```

- `from`, `to` (required) — step IDs; both must exist.
- `when` (optional) — a CEL predicate evaluated when `from` completes, in an
  environment exposing `output` (the completed step's output, dyn) and
  `run.params` (dyn map). The full environment is documented in
  `docs/expressions.md` (ticket 1.5). `when` expressions are **compiled at
  definition-validation time**; syntax or type errors reject the definition
  with position info.
- `type` (optional) — `normal` (default) or `loop`.

**Fan-out is parallel by default:** a step with multiple unconditioned
outgoing edges fires all of them. Conditioned edges fire independently —
every edge whose `when` evaluates true fires. Exclusive routing is what
`branch` is for.

**Evaluation errors are failures, not `false`.** If a `when` (or loop
`condition`) evaluation errors at runtime — missing field, type error — the
evaluation error is recorded as a step-level failure of the completing step's
transition; it is never silently coerced to `false`. How that failure is then
classified and retried is owned by ADR-006 (M5).

#### Branch edge-firing rule

Outgoing edges of a `branch` step are evaluated **in declaration order**
(their order in the `edges` array); the **first edge whose `when` is true
fires, and all others are skipped**. At most one outgoing edge may omit
`when`; it must be declared last and acts as the default (fires only when no
conditioned edge matched). No match and no default ⇒ all outgoing edges skip
(skip propagation below). Validation (1.3) enforces: every outgoing edge of a
branch except at most one trailing default carries `when`; a branch has at
least one outgoing edge.

This is why `branch` is "sugar": the graph model stays pure — conditioned
edges everywhere — and the branch type merely switches the firing rule on its
out-edges from "all matching" to "first matching".

```json
{"steps": [
   {"id": "route", "type": "branch", "config": {}}
 ],
 "edges": [
   {"from": "route", "to": "refund_flow",  "when": "output.category == 'refund'"},
   {"from": "route", "to": "billing_flow", "when": "output.category == 'billing'"},
   {"from": "route", "to": "generic_flow"}
 ]}
```

#### Loop edges

A loop edge is the **only permitted cycle** in a definition:

```json
{"from": "critique", "to": "draft_reply", "type": "loop",
 "condition": "output.verdict == 'revise'",
 "max_iterations": 3}
```

- `condition` (required, CEL) — evaluated when `from` completes; true means
  "iterate again".
- `max_iterations` (required, integer ≥ 1, capped by the limits table) — hard
  bound on iterations regardless of `condition`.
- Structural rules (enforced in 1.4): the definition graph **with all loop
  edges removed must be acyclic**, and a loop edge's `to` must be an ancestor
  of its `from` in that acyclic graph. The `to`→`from` paths delimit the
  **loop body** — the segment cloned per iteration.

Loops execute by **unrolling** (M14): when the loop source completes with
`condition` true and iteration count < `max_iterations`, the engine expands
the run graph with fresh instances of the loop body named `{id}#k`. The
instance graph stays acyclic; every iteration is durably checkpointed;
`condition` false or the cap reached routes execution to the loop source's
normal (non-loop) outgoing edges.

### Readiness, skip propagation, and join semantics

These rules are what `ReadySteps` (ticket 1.4) and the completion transaction
(ADR-002) implement. Loop edges are excluded from all of them — they
participate in expansion (M14), not in readiness or skip propagation.

1. **Edge resolution.** When a step reaches a terminal state, each outgoing
   normal edge resolves to exactly one of *fired* (source succeeded, `when`
   absent or true, and for branch steps the firing rule selected it) or
   *skipped* (source was skipped; or `when` false; or the branch rule passed
   it over).
2. **Non-join readiness/skip.** A non-join step becomes **ready** when at
   least one incoming edge fired and every other incoming edge is resolved.
   It becomes **skipped** when *all* incoming edges resolved skipped. Entry
   steps (no incoming edges) are ready at run creation.
3. **Skip propagation.** A skipped step never executes; its outgoing edges
   all resolve skipped, propagating onward. Because loop edges are excluded,
   propagation runs over a DAG and terminates.
4. **`join all`.** Ready when every incoming edge is resolved and at least
   one fired — **skipped parents count as satisfied, not as blockers**.
   Skipped only when all incoming edges resolved skipped. A *failed* parent
   means the join can never become ready; whether that fails the run
   immediately or lets independent branches finish is the workflow failure
   policy, owned by ADR-006 (M5).
5. **`join any`.** Ready the moment any single incoming edge fires; later
   firings are absorbed (the join runs once). Skipped when all incoming edges
   resolved skipped; if every parent ends failed-or-skipped with at least one
   failure, the join is blocked and ADR-006's failure policy applies.

Worked example — conditional branch into a fan-in:

```
fetch → route(branch) → refund_flow  ─┐
                      → billing_flow ─┤→ gather(join all) → reply
                      → generic_flow ─┘
```

`route` outputs `{"category": "billing"}`. The branch rule fires only
`route→billing_flow`; `refund_flow` and `generic_flow` have their single
incoming edge skipped, so both are skipped, and their edges into `gather`
resolve skipped. `gather` (mode `all`) now has all three incoming edges
resolved with one fired — it is ready, runs, and `reply` proceeds. Had
`gather` been mode `any`, it would have become ready the moment
`billing_flow` succeeded, without waiting for the skip propagation to
resolve the other two edges.

### Versioning and unknown fields

- `schema_version` is a **single integer**, starting at `1`. It identifies
  the *format*, not the workflow revision (stored-definition versioning is
  M6's concern).
- The engine accepts exactly the versions it knows and rejects others with a
  clear error. **Breaking changes** (removing/renaming fields, changing
  semantics of existing fields) bump the integer and get a documented
  migration. **Additive changes** (new optional fields, new step types, new
  enum values) do **not** bump it — the engine and the schema ship together
  (ADR-001: datastores are the compatibility surface, and per-run definition
  snapshots insulate in-flight runs from any format change).
- **Unknown fields are rejected, everywhere, with the JSON path of the
  offender** — top level, steps, edges, and per-type `config` alike. A
  definition that decodes is exactly understood. The single exception is the
  `ui` subtree.
- **Duplicate keys within one JSON object are not detected** — per
  `encoding/json`, the last occurrence silently wins. Strictness here targets
  unknown and mistyped fields; catching duplicates would require a
  token-level rescan of every object, and the builder (M17) and canonical
  encoder never emit them. Accepted limitation, recorded during the post-M1
  audit.

#### The `ui` block

`ui` is the **one deliberately loose subtree**. The engine treats it as an
opaque JSON object: never validated beyond being an object, never
interpreted, **round-tripped byte-for-byte** through decode→encode (1.2
implements this by retaining raw JSON). The builder (M17) owns its internal
shape; the suggested convention is per-step layout keyed by step ID:

```json
{"ui": {"nodes": {"classify": {"position": {"x": 120, "y": 240}}}}}
```

Rationale: layout data changes at UI speed, not engine speed. Forcing it
through the strict schema would either block builder iteration on engine
releases or push layout into a separate store that can drift from the
definition it describes.

### Limits

Enforced during structural validation (1.3), all violations reported together
with path-qualified errors. Defaults are compiled into `internal/dag`; making
them configurable is deferred until a concrete need appears.

| Limit | Default |
|---|---|
| Max steps per definition | 10,000 |
| Max edges per definition | 20,000 |
| Max serialized definition size | 1 MiB |
| Step ID | `^[a-z][a-z0-9_-]{0,63}$` |
| Max `name` length | 128 |
| Max CEL expression length (`when`, `condition`) | 1,024 |
| Max `max_iterations` on a loop edge | 100 |

The 10k-step ceiling is deliberately at the M1 exit-criteria benchmark
(validate + compute readiness on 10k nodes in <100ms) so the limit is backed
by a measured performance envelope. Runtime expansion caps (`max_added_steps`,
`max_total_steps`, `max_expansions`, `max_depth`) are a separate concern owned
by ADR-015 (M13), which adds the optional top-level `expansion` block carrying
them (defaults 32 / 10,000 / 100 / 4).

### Structural validation: severities, orphans, edge-field rules

*Added 2026-08-08 while implementing ticket 1.3 — clarifies points the
original text left open; no prior decision is changed.*

- **Validation issues carry a severity and a stable code.** Structural
  validation reports every violation in one pass; each issue has a
  machine-readable `snake_case` code, a severity (`error` or `warning`), and
  a JSON path. Errors reject the definition; warnings are surfaced to
  callers (and later the builder UI) but do not block acceptance.
- **Orphan steps are a warning, not an error.** An *isolated* step — no
  incoming or outgoing edges, in a definition with more than one step — is
  not dead code under the readiness rules above: having no incoming edges
  makes it an entry step, so it executes in parallel with everything else.
  It is, however, the classic shape of an unconnected builder node, so it
  warrants a warning (`isolated_step`) rather than rejection.
- **Reachability needs no dedicated rule in 1.3.** With loop edges removed
  the graph must be a DAG (rule above), and in a DAG every node traces back
  along incoming edges to some zero-in-degree entry step — so a step can
  only be unreachable by sitting on a cycle, and all non-loop cycles are
  rejected by 1.4's cycle detection. 1.3's degree-based isolated-step check
  plus 1.4's cycle rejection together cover "no orphan/unreachable steps".
- **Loop-only fields are rejected off loop edges, and vice versa.**
  `condition` or `max_iterations` on a normal edge is a validation error,
  as is `when` on a loop edge (its predicate is `condition`). Every decoded
  field is either meaningful or rejected — mirroring the unknown-field
  policy at the semantic level.
- **The 1 MiB size limit is enforced at the top of `Decode`**, before any
  parsing (validating a definition requires decoding it first, and the
  point of a size cap is to reject oversized payloads cheaply). Like the
  `schema_version` gate, it is reported alone. All other limits are
  enforced in `Validate` and reported together with everything else.

### Graph algorithms: cycle reporting, readiness API shape

*Added 2026-08-08 while implementing ticket 1.4 — clarifies points the
original text left open; no prior decision is changed.*

- **Cycle and ancestry violations join the one-pass report.** The 1.4 rules
  run inside `Validate` as its final phase, gated on a well-formed graph
  (no duplicate IDs, no unknown edge endpoints) so a broken endpoint never
  cascades into spurious cycle reports. Two codes: `cycle_detected` (path =
  the edge that closes the cycle; the message carries the full step path,
  e.g. `a -> b -> a`; one issue per back edge found in a deterministic DFS,
  so independent cycles each get reported) and `loop_edge_not_ancestor`
  (path = the offending loop edge).
- **`ReadySteps` takes step-level state; per-edge outcomes are the engine's
  seam.** The pure library computes readiness from three disjoint step-ID
  sets — `completed`, `skipped`, `failed` — deriving edge resolutions as:
  out-edges of completed steps *fired*, of skipped steps *skipped*, of
  failed or pending steps *unresolved*. Runtime `when`-false and
  branch-pass-over outcomes (1.5/M4) enter by seeding the `skipped` set of
  the passed-over steps; the completion transaction owns per-edge CEL
  evaluation. `ReadySteps` returns both the ready frontier and the steps
  skip propagation newly resolved, which the engine persists and feeds back.
- **A ready step stays ready until it enters a terminal set.** The function
  is a pure, repeatable computation over run state; dispatch bookkeeping
  (claimed/running steps) is the engine's concern, not the graph library's.

### CEL compilation: environment shape, result-type rule, codes

*Added 2026-08-08 while implementing ticket 1.5 — clarifies points the
original text left open; no prior decision is changed.*

- **The environment declares exactly two variables.** `output` is `dyn`
  (a step's output shape is not statically known), and `run` is
  `map(string, dyn)` — declared as a map rather than a struct so
  run-scoped context can grow additively without an environment version
  bump. Only `run.params` is populated today. Standard CEL built-ins and
  macros only; no custom functions in 1.5. Full reference:
  `docs/expressions.md`.
- **Predicates must check to `bool` or `dyn`.** `when` and `condition`
  are predicates, so an expression whose checked type is anything else
  (`1 + 2`, `'abc'`) is rejected at validation time
  (`expression_not_boolean`). A `dyn` result is accepted at compile time
  and enforced at evaluation: a non-bool runtime value is an evaluation
  error under the errors-are-failures policy above, never a truthiness
  coercion.
- **Compile failures join the one-pass report.** Two codes:
  `invalid_expression` (syntax/typecheck failure; one issue per CEL
  error, message prefixed with the 1-based `line:col` position inside the
  expression) and `expression_not_boolean`. Only the semantically
  meaningful predicate of each edge kind is compiled (`when` on normal
  edges, `condition` on loop edges), and over-length expressions are not
  compiled — they are already rejected, and the 1,024-byte cap bounds
  compile work (~50µs per expression).
- **Evaluation is a typed helper, not an engine.** `CompileExpr` /
  `CompiledExpr.Eval` return the routing boolean or a typed `*EvalError`
  (`errors.As`-reachable); the completion transaction (M4) owns calling
  it and recording the failure, and failure classification stays with
  ADR-006 (M5).

### Step input templating (`${{ ... }}`)

*Added 2026-08-14 while implementing ticket 8.2 — makes the template
syntax the examples always carried informally a real, linted part of the
contract; no prior decision is changed.*

- **Where templates live.** `${{ ... }}` expressions may appear in JSON
  **string values inside a step's `config`** — and only there. Envelope
  fields (`retry`, `timeout`, `max_wall_clock`, `cache`) are materialized at
  instantiation, before any output exists, so they take literals only;
  CEL predicates (`when`/`condition`) have their own variable
  environment; the `ui` block stays engine-opaque; object keys are never
  rendered. Rendering happens in the worker, per attempt, just before
  the executor decodes the config — the stored `run_steps.config` keeps
  the authored templates, and `StepContext.Config` carries the rendered
  result.
- **Reference grammar.** Two roots: `steps.<id>.output[.<path>]` (only a
  step's *output* is addressable — never its status or config) and
  `run.params[.<key>[.<path>]]`. Path segments traverse objects by key
  and arrays by integer index (`steps.a.output.items.0.name`); keys
  outside `[A-Za-z0-9_-]` are unreachable by bare references (use `get`
  with a quoted path).
- **Engine and functions.** Go `text/template` with `${{`/`}}`
  delimiters — a bare `{{ ... }}` is inert literal text. Expressions are
  a single pipeline: control structures, variable declarations, and
  every text/template builtin (`printf`, `index`, `call`, ...) are
  rejected at parse time, so the function surface is exactly **`get`**
  (lenient path lookup → nil when absent), **`default`** (fallback for
  nil/empty, the `get` companion), **`toJson`** (compact JSON string),
  and **`truncate`** (first N runes). String literals inside expressions
  may be single-quoted (`get 'steps.a.output.x'`) to avoid `\"` noise in
  JSON documents; there is deliberately no escape for a literal `${{` in
  config text (revisit if it ever bites).
- **Strict by default, lenient by opt-in.** A bare reference that does
  not resolve — unknown step output, missing key, out-of-range index,
  undeclared or unsubmitted param — is a typed error, never an empty
  string; ADR-006 classifies the resulting step failure permanent. The
  sanctioned opt-out is `${{ get 'path' | default fallback }}`.
- **Type preservation.** A string that is exactly one expression
  (whitespace aside) is replaced by the resolved value itself — objects,
  arrays, numbers, booleans, and null survive as JSON, which is how
  structured data flows between steps. Mixed literal-and-expression
  strings render to strings: scalars interpolate naturally (numbers
  verbatim, `true`/`false`, nil as the empty string), composites as
  compact JSON. A config containing templates re-encodes canonically
  when rendered (sorted keys, no HTML escaping); template-free configs
  pass through byte-identical.
- **Injection is inert.** Rendering happens exactly once, on the
  authored definition; values arriving from outputs or params are data
  and are never re-parsed as templates.
- **Static lint at validation.** Five codes join the one-pass report:
  `template_invalid` (syntax, control structures, unknown functions),
  `template_ref_invalid` (malformed reference or unknown root),
  `template_ref_unknown_step`, `template_ref_not_upstream` (the
  referenced step must be a strict normal-edge ancestor — loop-edge-only
  reachability does not count, and self-references are rejected), and
  `template_ref_unknown_param`. Quoted `get` paths are exempt from the
  lint (resolving to nil is their contract) but still count toward the
  outputs the engine prefetches. One knock-on carve-out: a *templated*
  sleep `duration` skips the literal parseability check at validation
  (the executor re-validates the rendered value at runtime).
- **Runtime data scope.** The engine fetches exactly the referenced
  steps' rows (plus the run's params) before rendering; only a
  `succeeded` step contributes an output. Referencing a skipped or
  still-unfinished step (possible behind a `join any`) is a strict
  missing-reference failure by design — `get`/`default` is the tool for
  values that may legitimately be absent.

### The `blackboard` step envelope (as built, 12.2)

*Added while implementing ticket 12.2 (ADR-014).* A step may carry a
`blackboard` envelope block declaring declarative writes to the run-scoped
blackboard, applied on the step's success:

```json
"blackboard": { "write": [ { "key": "draft", "from": "/text", "tags": ["draft"], "pinned": true } ] }
```

`key` follows the blackboard key grammar (`[A-Za-z0-9_-]{1,128}`, no dots —
a dot is the path separator in 12.3's `blackboard.<key>` selectors); `from`
is an RFC-6901 pointer into the step's *own* output (default `""` = whole
output); `pinned: true` adds the reserved `pinned` tag. Validated at submit
(codes `blackboard_field_required`/`blackboard_field_invalid`, at most 16
writes, keys distinct). Like `cache`/`validation`, the block is uniform
across step types and materialized at instantiation. **Declarative reads into
a prompt are 12.3's context sources**, not a template root — 12.2 delivers
writes plus the programmatic `StepContext.Blackboard` read/write API.

### The `context` step envelope (as built, 12.3)

*Added while implementing ticket 12.3 (ADR-014).* An llm-family step may carry
a `context` envelope block declaring the ordered sources assembled into its
provider request before the call:

```json
"context": { "sources": [
  { "kind": "literal", "text": "You are a careful editor.", "pinned": true },
  { "kind": "step_output", "step": "draft", "path": "/text", "max_tokens": 2000 },
  { "kind": "blackboard", "key": "findings" },
  { "kind": "retrieval", "retriever": "pg_fulltext", "query": "style guide", "top_k": 3, "on_missing": "skip" }
] }
```

Each source's `kind` selects which fields apply (`step`+`path` for
step_output; `key` XOR `tags` for blackboard; `retriever`+`query`+`top_k` for
retrieval; `text` for literal); order is precedence and message order. A
per-source `max_tokens` caps the source (mutually exclusive with `pinned`,
which is never truncated); `on_missing` (`error` default | `skip`) governs a
source that resolves to nothing. Validated at submit (codes
`context_field_required`/`context_field_invalid`, at most 32 sources,
llm-family only, per-kind required/forbidden fields, key/tag/pointer grammar,
and — for a `step_output` source — the same normal-edge-ancestry rule as a
template ref). Templating inside the block is **not** supported (a dynamic
query flows through a `retrieve` step and a `step_output` source). Like
`cache`/`validation`/`blackboard`, the block is uniform across the llm family
and materialized at instantiation (`run_steps.context_policy`). The
declarative *reads* the 12.2 blackboard section deferred are exactly these
`blackboard` context sources.

*Ticket 12.4 (ADR-014) added the compaction fields to the same block:*
`budget_tokens` (a positive framed-request token cap; when the assembled
request exceeds it the pipeline runs), `compaction` (an ordered list of
`{ strategy, n?, min_tokens? }` — `sliding_window` requires `n ≥ 1`,
`truncate_oldest` admits an optional `min_tokens ≥ 0`, `drop_lowest_priority`
takes no parameter; `summarize` is reserved for 12.5 and rejected; at most 8,
no duplicates), and a per-source `priority` (default 0, orders
`drop_lowest_priority`). Same codes (`context_field_required`/
`context_field_invalid`); a parameter set on a strategy it does not belong to
is rejected. A `compaction` pipeline with no `budget_tokens` is admissible but
inert until 12.6 defaults the budget from the model window.

### Enforcement points

For conformance, the rules above land in specific tickets: **1.2** — strict
decoding, unknown-field rejection, `ui` raw round-trip, `schema_version`
gate, JSON Schema generation; **1.3** — ID rules, endpoint existence,
per-type config shape, branch edge rules, limits, multi-error reporting;
**1.4** — acyclicity-minus-loop-edges, loop-edge ancestry, readiness/skip/join
semantics; **1.5** — CEL compilation of `when`/`condition`, evaluation-error
policy; **8.2** — template parsing/rendering, the template lint codes, and
the worker-side render step.

## Consequences

Positive:

- One strict contract with path-qualified errors: a definition that decodes
  is fully understood, and builder/engine drift surfaces as an immediate,
  precise error instead of silent misbehavior.
- The generated JSON Schema cannot drift from the decoder — both derive from
  the same Go structs, and CI checks the generated artifact.
- The `ui` carve-out lets the builder iterate on layout state without engine
  releases, while layout still travels with the definition it describes.
- Loop-by-unrolling keeps every durability mechanism (per-run rows,
  `remaining_deps`, completion-transaction readiness) working on acyclic
  graphs; loops inherit crash-safety for free via the expansion machinery.
- Reserving `#` and `.` in IDs now costs nothing and prevents a painful
  migration when M13/M14 instance naming and CEL paths arrive.

Negative:

- **Strictness gates evolution on the engine.** A definition using a new
  field is rejected by an older engine — acceptable single-deployment, but a
  real constraint if definitions are ever shared across installations of
  different ages.
- **`branch` semantics live in two places.** The validator and the builder
  must both understand the first-match firing rule; it is not expressible in
  the JSON Schema alone.
- **Iteration bounds are static.** `max_iterations` is fixed at authoring
  time; a loop that needs data-dependent depth must over-provision the cap.
- **The `ui` block is a validation hole by design.** Junk under `ui`
  accumulates unnoticed; the engine will faithfully round-trip garbage.
- **Every new step type touches Go code** (a config struct + registration).
  Intended — typed configs are what make path-qualified errors possible —
  but it rules out defining new step types from data alone.

## Alternatives considered

- **YAML as the authoring format.** Friendlier to hand-authoring, but the
  primary producers are the API and the visual builder, which speak JSON;
  YAML's implicit typing (`no` → false) and anchors/aliases add failure modes
  to a contract whose whole point is strictness. A YAML front-end can be
  layered client-side later without touching the contract.
- **A Go/TypeScript DSL as the source format.** Great authoring ergonomics,
  but the visual builder needs a *data* format to serialize to, and any DSL
  compiles down to one anyway — that data format is the contract, so it is
  what gets specified.
- **Allowing real cycles, executed in place.** Rejected: readiness computed
  in completion transactions, `remaining_deps` counters, and skip propagation
  all assume a DAG; in-place cycles would need iteration-scoped state resets
  and make "resume from last completed step" ambiguous. Unrolling gives
  durable per-iteration checkpoints and cap enforcement through machinery
  M13 builds regardless.
- **Conditions on steps instead of edges.** A step-level condition cannot
  express per-successor routing (send billing tickets here, refunds there)
  without duplicating steps; edge conditions subsume step conditions.
- **Config-driven branch routing** (branch `config` lists cases with target
  IDs). Rejected: it duplicates routing between `config` and `edges`, giving
  two sources of truth the validator must cross-check and the builder must
  keep in sync. The edge-firing rule keeps routing in the graph.
- **Lenient decoding (ignore unknown fields).** Maximally
  forward-compatible, but silently swallows typos and schema drift — the
  exact failure class this contract exists to prevent. The `ui` carve-out
  covers the one legitimate need for looseness.
- **Semver string for `schema_version`.** Minor/patch distinctions only pay
  off when producers and consumers evolve independently; here the engine and
  schema co-deploy, so only breaking changes matter and an integer
  comparison suffices.
- **Hand-written JSON Schema as the source of truth.** Then the schema and
  the Go decoder are two artifacts that drift; generating the schema from
  the structs the engine actually decodes (with a CI drift check) keeps one
  truth.
