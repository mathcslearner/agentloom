# ADR-019: Frontend architecture & the serialization boundary

- **Status:** Accepted
- **Date:** 2026-08-18
- **Ticket:** ROADMAP.md ticket 17.2 (Milestone 17)

<!--
This ADR governs the M17 visual builder and the M18 dashboard. Ticket 17.2 fixes
the frontend package layering and — the part that has to be right before any
canvas code exists — the *serialization boundary*: the pure, isolated module
that maps the workflow definition JSON to and from the builder's canvas state.
Later M17/M18 tickets consume it without re-litigating the mapping:

  - 17.3 canvas, palette & node components (React Flow over Flow nodes/edges)
  - 17.4 schema-driven config panels (GET /v1/plugins schemas)
  - 17.5 client-side graph validation (parity with internal/dag rules)
  - 17.6 import/export, save & submit (canonical byte-for-byte export)

Sections tagged "(arrives in 17.x)" state the contract now; those tickets add
"### … (as built, 17.x)" subsections under ## Decision as they land, the way
ADR-014/015/016/017/018 grew across their milestones.
-->

## Context

The visual builder (M17) and the live dashboard (M18) are a *view* over the
workflow definition contract that has existed since M1 (ADR-003). Nothing on the
engine side changes to accommodate them. The definition JSON is a versioned
contract: Go structs are the source of truth, `docs/schema/workflow-definition.v1.json`
is generated from them, and the decoder is **strict** — unknown fields are
rejected everywhere, with exactly one exception, the `ui` subtree, which the
engine keeps opaque and round-trips byte-for-byte (ADR-003 "The `ui` block").

A node-based builder does not edit that JSON directly. It edits a *canvas*: a set
of positioned nodes and the edges between them, backed by a store (zustand,
17.3), rendered by React Flow. Two representations therefore exist — the
definition document and the canvas state — and something must map between them,
in both directions, without losing information. That mapping is the highest-risk
piece of the whole frontend:

- **Losslessness is a contract, not a nicety.** A user opens a stored definition
  in the builder, moves one node, and saves. Every field the builder did not
  touch — every config value, every envelope policy, the `templates`/`agents`
  libraries, and *any field a newer backend added that this build has never heard
  of* — must survive untouched. If the builder silently drops a field on save, it
  corrupts a definition a human wrote, which is the exact failure the strict
  contract exists to prevent.
- **Forward compatibility is required.** The builder ships on a schedule
  independent of the engine. A definition may legally carry fields this build's
  generated types do not know about. The mapping must pass those through, not
  enumerate-and-copy only the known keys.
- **The mapping must be testable in isolation.** It has no business importing
  React, Next, or React Flow: it is pure data transformation, and coupling it to
  the UI would make it un-property-testable and would let a UI concern leak into
  the definition. The one place the whole frontend's correctness can be pinned
  against the backend's own fixtures is here — so it must be a leaf.

## Decision

We split the frontend into a thin set of **pure, dependency-light libraries**
under `web/lib/*` and one **React app** under `web/app` that is the sole consumer
of React/Next/React Flow. The libraries are:

- `@agentloom/api-client` (17.1) — the typed REST client (generated from
  `api/openapi.yaml`).
- `@agentloom/engine-client` (16.5) — the typed event-feed WebSocket client
  (generated from `docs/schema/events.v1.json`).
- **`@agentloom/graphdef` (17.2, this ADR)** — the serialization boundary:
  the canvas state model, the definition ⇄ canvas mapping, and (17.5) the
  client-side validator. **Zero React/UI imports**, enforced by a lint rule and
  by the package's own dependency graph (React is not a resolvable specifier
  inside it).

The data flow is one direction of dependency at every seam:

```
  definition JSON  ⇄  graphdef.Flow  ⇄  builder store (zustand, 17.3)  ⇄  React Flow
     (contract)        (this ADR)          (app)                            (app)
```

`graphdef` is the only module that knows both the definition shape and a
canvas-shaped structure. The store adapts `Flow` to React Flow's runtime types
(which are structurally compatible by design — see below); the app renders. An
API error on submit is mapped back onto a node by its path (17.4/17.5).

### Types are generated from the definition JSON Schema

`graphdef` does **not** reuse `api-client`'s generated `Definition` type. That
type comes from `openapi-typescript` rendering the spec's `$ref` to the
definition schema, and its `Step` `oneOf` renders as a base intersected with a
discriminated union in which `config` collapses to `Record<string, never>` on the
base — usable for REST payloads but wrong for a builder that must read and write a
step's typed config. Instead, `graphdef` generates its own types directly from
`docs/schema/workflow-definition.v1.json` with a small, **dependency-free,
deterministic emitter** (`scripts/gen-definition-types.ts`), the same discipline
`engine-client` uses for `events.ts`: byte-stable output is what a CI drift diff
needs. The emitter renders one interface/alias per `$defs` entry, string-literal
unions for enums, `Record<string, T>` for `additionalProperties: {$ref}` maps,
`unknown` for the `true` (any) schema, and — the one special case beyond the
event emitter — the `Step` `oneOf` as a discriminated union:

```ts
export type Step = { [K in StepType]: StepBase & { type: K; config?: StepConfigMap[K] } }[StepType];
```

so `switch (step.type)` narrows `step.config`. Drift chain: Go structs →
`workflow-definition.v1.json` (`make generate`, Go-CI-diffed) → `definition.ts`
(the `web` CI job regenerates and `git diff --exit-code`s
`lib/graphdef/src/generated`). A Go definition-struct change unreflected in the
committed TS fails CI on one side or the other.

The generated types are a **compile-time aid only**. The runtime mapping never
enumerates known keys (see forward compatibility below), so a type that lags the
schema cannot cause data loss — it only costs the builder editor UI for the new
field until the next regenerate.

### The canvas state model — `Flow`

`toFlow` produces, and `toDefinition` consumes, a single value:

```ts
interface Flow {
  doc: DocumentMeta;      // everything except steps/edges/ui, verbatim
  nodes: FlowNode[];      // one per step, in steps order
  edges: FlowEdge[];      // one per edge, in edges order
  ui: UIRest;             // the ui block minus the per-node position lift
}
```

- **`DocumentMeta`** is the definition object with `steps`, `edges`, and `ui`
  removed — `schema_version`, `name`, `description`, `on_failure`,
  `max_wall_clock`, `budget_usd`, `on_budget_exceeded`, `expansion`, `templates`,
  `agents`, `params`, **and any unknown top-level key**. It is stored as an
  opaque record and copied whole; the app edits its known fields through
  document-level panels. `templates` and `agents` are document-level libraries in
  M17, **not** canvas nodes (a template sub-canvas is deferred — see Consequences).

- **`FlowNode`** = `{ id, type, position: {x,y}, data: { step, positioned, ui? } }`.
  `data.step` is the entire step object — config, every envelope block
  (`retry`/`timeout`/`cache`/`budget`/`validation`/`blackboard`/`context`), and
  any unknown key — carried verbatim. `node.id`/`node.type` mirror
  `step.id`/`step.type` (an invariant `toDefinition` re-imposes). `position` is
  lifted from `ui.nodes[id].position` when present (`positioned: true`), else
  synthesized by `opts.defaultPosition(step, index)` — a deterministic grid by
  default — with `positioned: false`. `data.ui` is the rest of `ui.nodes[id]`
  (anything other than `position`), preserved so a builder-written per-node hint
  the engine ignores round-trips.

- **`FlowEdge`** = `{ id, source, target, sourceHandle?, targetHandle?, data: { edge } }`.
  `source`/`target` are the edge's `from`/`to`; `data.edge` is the entire edge
  object (`when`/`type`/`condition`/`max_iterations`/`on_exhausted`/`no_progress`/
  `decision` and any unknown key) verbatim. Handles (`sourceHandle`/`targetHandle`)
  are **presentation state owned by the canvas** (17.3 sets them from the port an
  edge attaches to); `graphdef` never reads them and does not persist them into
  the definition. Edges have no id in the definition and duplicate `(from,to)`
  pairs are legal, so `graphdef` synthesizes a **deterministic** id:
  `edgeId(from, to, n)` = `` `${from}->${to}` `` for the first occurrence and
  `` `${from}->${to}#${n}` `` (n ≥ 2) for later duplicates, assigned by
  scan order. The id is stable across a round-trip because edge order is preserved.

- **Order is preserved, never sorted.** `Flow.nodes` is in `steps` order and
  `Flow.edges` is in `edges` order. `toDefinition` writes them back in node/edge
  array order. Order is byte-relevant for canonical export (17.6), so the mapping
  is order-preserving and any reordering is an explicit user action in the store,
  not a serialization side effect.

### Mapping rules

**`toFlow(input, opts?)`** accepts an already-typed `Definition` or an `unknown`
parsed JSON value. It shape-checks the minimum a canvas needs — the value is an
object; `steps` is an array of objects each with a string `id` and string `type`;
`edges` is an array of objects each with a string `from` and string `to`; `ui`,
if present, is an object — and throws a typed **`GraphdefError` `{ code, path }`**
on any violation, plus on **duplicate step ids** (two nodes cannot share an id on
a canvas). It does **not** validate configs, edge endpoints, cycles, or CEL —
those are 17.5's job; a dangling edge or an invalid config is faithfully carried
into the `Flow`. Positions are lifted from `ui.nodes` as above.

**`toDefinition(flow)`** reassembles the document deterministically:
`{ ...doc, steps, edges, ui }` where

- `steps[i]` = `{ ...node.data.step, id: node.id, type: node.type }` — the step
  object with id/type re-imposed from the node (so a rename in the store lands on
  the step);
- `edges[i]` = `{ ...node.data.edge, from: edge.source, to: edge.target }`;
- `ui` = `flow.ui` (the non-node rest) **plus** `ui.nodes[id] = { ...node.data.ui, position }`
  for every `positioned` node, and **orphan `ui.nodes` entries** (ids that no
  longer name a step) preserved verbatim under `ui.nodes`. `ui` is omitted
  entirely when the reassembled block would be empty *and* the source had no `ui`
  key, so a definition with no layout data does not gain an empty `ui: {}`.

Both functions are pure and synchronous; `toDefinition(toFlow(d))` deep-equals
`d` at the JSON-value level, and `toFlow(toDefinition(f))` deep-equals `f`.

### The `ui` block — ownership and limits

The builder **owns** the `ui` block. ADR-003 already fixed the convention
`ui.nodes.<stepId>.position.{x,y}`; `graphdef` reads and writes exactly that key
and the viewport (`ui.viewport { x, y, zoom }`, carried in `Flow.ui` and left to
the app), and preserves everything else under `ui` untouched. Unknown keys under
`ui` (a future builder's per-node collapse state, a `ui.version`) survive a
round-trip because the block is copied and only the position sub-key is
lifted/rewritten.

**Limit — value-level, not byte-level.** Go preserves the `ui` block's *bytes*
(raw JSON retained). The browser side preserves it at the *JSON-value* level:
whitespace and number spelling (`300.5` vs `3.005e2`) cannot survive a
JSON.parse/stringify round-trip in JS. This is not a regression — the builder is
the *author* of `ui`, so it defines the canonical spelling, and the value is what
the engine (which never reads `ui`) and the builder both care about. The
byte-for-byte export guarantee (17.6) applies to the whole document via a TS
canonicalizer that mirrors Go's `Encode`, not to preserving an arbitrary input's
incidental formatting.

### Unknown-field passthrough (forward compatibility)

The runtime mapping is written as *copy-the-whole-object, then lift/overwrite the
few keys graphdef owns* — never *enumerate the known keys and rebuild*. The only
keys graphdef reads or writes are `id`, `type`, `steps`, `edges`, and
`ui.nodes.*.position`. Everything else — an unknown top-level field, an unknown
step field, an unknown key inside a config or an envelope block, an unknown edge
field, an unknown `ui` key — is carried by object spread and reappears unchanged
on `toDefinition`. A property test injects random `x_future_*` keys at every
object level across the fixture corpus and asserts the round-trip is still
lossless. Because the backend is strict, the builder never *invents* unknown
fields; it only preserves ones a human or a newer backend put there.

### Canonical export (arrives in 17.6)

Import accepts any legal definition; **export produces canonical bytes**. A TS
`canonicalize(def): string` will mirror Go's `Encode` — fixed top-level field
order, Go-struct field order within objects, sorted map keys, `omitempty`
semantics, and the `ui` block emitted with sorted keys — and will be pinned
against Go-produced goldens (the `UPDATE_GOLDEN` fixture discipline from 13.6), so
"export equals canonical backend serialization byte-for-byte" is a fixture
assertion. 17.2 ships the lossless *value*-level round-trip; 17.6 adds the
*byte*-level canonical form.

### Validation parity (arrives in 17.5)

The client mirrors the backend's rules so the user gets immediate feedback, but
**the backend stays the authority**. The strategy fixed now:

1. **Shape** is checked against the same published JSON Schema at runtime
   (`workflow-definition.v1.json`), so shape errors match the contract.
2. **Graph/semantic rules** (cycles forbidden except marked loop edges, dangling
   edge endpoints, unreachable nodes, join config sanity, required config
   present, loop-edge ancestry) are hand-mirrored in `graphdef/validate`,
   reporting the backend's `ValidationCode` vocabulary and its path grammar
   (`steps[3].config.model`) so a client verdict and a server verdict name the
   same problem in the same place.
3. **Parity is proven by a shared corpus.** The backend's
   `internal/dag/testdata/{valid,invalid,invalid_structural}` and
   `examples/definitions` are run through both validators and their accept/reject
   verdicts compared (17.5's DoD). Where the client cannot reproduce a rule
   exactly (CEL evaluation, plugin-config schemas), it defers to the backend and
   the round-trip surfaces the server's issues mapped onto nodes by path.
4. **Config-level rules** come from the plugin JSON Schemas served by
   `GET /v1/plugins` (17.4), not hard-coded per-plugin.

## Consequences

- **The serialization boundary is a single, testable leaf.** The whole
  frontend's data correctness is pinned in one place, against the backend's own
  fixtures, with no UI in the loop. A React import inside `graphdef` fails lint
  *and* would fail to resolve (React is not a dependency of the package).
- **Forward compatibility is structural.** Copy-then-overwrite means a field the
  build has never seen cannot be dropped. The cost is that the builder cannot
  *edit* an unknown field (no generated UI for it) until a regenerate — acceptable
  and expected.
- **Losslessness is value-level on the browser side.** A definition imported,
  opened, and re-exported is canonical, not byte-identical to the arbitrary input
  — by design, since export is canonical. Byte-for-byte identity is asserted
  against *canonical* input in 17.6.
- **Templates and agents are not on the canvas in M17.** They are document-level
  libraries edited through panels. A `map` step references a template by name; the
  template's sub-graph is not a nested canvas yet. Deferred: a template
  sub-canvas, if the flows warrant it (M17 backlog).
- **Edge identity is synthesized, not stored.** The definition has no edge ids, so
  the canvas' notion of "this edge" is a deterministic function of `(from, to,
  duplicate-ordinal)`. Reordering or de-duplicating edges in the store changes
  ids; that is a user action, and the round-trip is defined for the order the
  document has.

## Alternatives considered

- **Reuse `api-client`'s generated `Definition`/`Step` types.** Rejected: the
  `oneOf` renders `config` as `Record<string, never>` on the step base, so a typed
  config is unreachable. The builder needs `switch (type)`-narrowed config, which
  a purpose-built emitter over the JSON Schema gives directly. The two generators
  target different consumers (REST payloads vs. a config editor) and are both
  Go-CI-diffed, so there is no drift risk in keeping them separate.
- **Depend on `@xyflow/react` types in `graphdef`.** Rejected: it would pull a
  React-ecosystem dependency into the pure module and couple the definition
  mapping to a specific canvas library's version. `graphdef` defines its own
  `FlowNode`/`FlowEdge` shapes that are *structurally compatible* with React
  Flow's `Node`/`Edge` (`id`, `type`, `position`, `data`, `source`, `target`,
  handles), so the store adapts them at zero cost without the coupling.
- **Store layout outside the definition** (a separate positions store keyed by
  definition id). Rejected by ADR-003 already: layout would drift from the
  definition it describes, and "open in builder" from a stored definition would
  lose positions. The `ui` block keeps layout and logic in one versioned document.
- **Normalize or prune orphan `ui.nodes` and unknown fields in `graphdef`.**
  Rejected: it breaks passthrough. Pruning a `ui.nodes` entry whose step was
  deleted, or dropping an unknown field, is a *builder* action a user takes
  deliberately (and 17.6's export can offer it), not a silent serialization side
  effect.
- **Use a schema-to-TS library (`json-schema-to-typescript`) for the generated
  types.** Rejected for the same reason `engine-client` hand-rolls its emitter: a
  codegen library's formatting can flake a CI byte-diff, and the definition schema
  uses a small, known vocabulary a ~150-line deterministic walker covers exactly.
