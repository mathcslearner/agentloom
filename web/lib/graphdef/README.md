# @agentloom/graphdef

The **serialization boundary** (ADR-019, ticket 17.2): a pure, isolated TypeScript
module that maps the workflow definition JSON to and from the visual builder's
canvas state. It is a leaf — **zero React/UI imports** — so the whole frontend's
data correctness is pinned in one place, against the backend's own fixtures, with
no UI in the loop.

## What it does

```ts
import { toFlow, toDefinition, parseDefinition } from "@agentloom/graphdef";

const def = parseDefinition(jsonText);   // JSON.parse → GraphdefError on syntax error
const flow = toFlow(def);                // definition value → canvas state
// ... the builder edits flow.nodes / flow.edges / flow.doc / flow.ui ...
const back = toDefinition(flow);         // canvas state → definition value (lossless)
```

- `toFlow(input, opts?)` — shape-checks the document (throwing `GraphdefError`
  `{ code, path }` on a non-object, ill-formed steps/edges, a non-object `ui`, or a
  **duplicate step id**), lifts per-step positions out of the `ui` block, and
  carries everything else — configs, envelope blocks, unknown fields, residual
  `ui` keys, orphan `ui.nodes` entries — **verbatim**. It does *not* validate
  configs, edge endpoints, cycles, or CEL (that is the client validator, 17.5).
- `toDefinition(flow)` — the exact inverse at the JSON-value level.
- `Flow` is the canvas state model: `{ doc, nodes, edges, ui, uiPresent }`, with
  `FlowNode`/`FlowEdge` structurally compatible with React Flow's `Node`/`Edge`
  (no `@xyflow/*` dependency).

## Guarantees

- **Lossless round-trip** over the entire backend fixture corpus
  (`examples/definitions` + `internal/dag/testdata`), asserted directly
  (`test/roundtrip.test.ts`).
- **Unknown-field passthrough** — a field a newer backend added and this build has
  never heard of survives untouched (`test/passthrough.test.ts`, incl. a seeded
  property test that injects random `x_future_*` keys across the corpus).
- **Pure module** — no React/UI imports, enforced by `eslint.config.mjs`
  (`no-restricted-imports`) and `test/boundary.test.ts`, and by the package having
  no UI dependency at all.

## Generated types

`src/generated/definition.ts` is emitted from
`docs/schema/workflow-definition.v1.json` by `scripts/gen-definition-types.ts`
(`pnpm generate`). The Go `dag` structs are the source of truth (ADR-003);
`make generate` emits the schema, this script emits the TS, and CI fails on any
drift (`git diff` in the `web` job). We generate our own types rather than reuse
`@agentloom/api-client`'s `Definition`, because openapi-typescript renders the
`Step` `oneOf` with `config` collapsed to `Record<string, never>` — unusable for a
config editor. Here `Step` is a discriminated union so `switch (step.type)`
narrows `step.config`. The same script also emits `src/generated/definition.schema.ts`
(`DEFINITION_SCHEMA`), the published JSON Schema as a runtime constant.

## Client-side graph validation (17.5)

`validateDefinition(value)` ports the whole `internal/dag` validator to the
browser, reporting the backend's `ValidationCode` vocabulary and path grammar so
a client verdict and a server verdict name the same problem in the same place.
It runs the backend's two stages:

- **Decode** (`validate/decode.ts`) — a schema-driven walker over the published
  definition schema reproducing the strict codec's shape/enum/required findings
  (non-object root, `schema_version` short-circuit, unknown fields, wrong types,
  decode-enforced enums) as codeless issues. When it finds anything the validate
  stage does not run (exactly like the backend).
- **Validate** (`validate/definition.ts`) — every rule in the backend's order:
  document limits, run budget/expansion, per-step config + all seven envelope
  blocks, edges, approvals, graph degree rules, the templates/agents/maps
  libraries, and — behind the well-formed-graph gate — cycle detection
  (`graph.ts`, the ported three-colour DFS reporting the closing edge + cycle
  path), loop ancestry, and the step_output/template reference lint. CEL
  (`cel.ts`) and the `${{ }}` template lint (`template.ts`) are **lenient**
  client checkers: they report the backend's codes but treat anything
  unrecognised as valid, so the client never over-rejects.

Issues carry a client-only `related[]` (a cycle's nodes + closing edge) for
highlight, and client-only `advisory_*` warnings surface budget-sanity nudges
the backend does not model. `validateStepConfigs(def)` remains as a config-scoped
façade (17.4 API). Parity is proven against the Go golden
`internal/dag/testdata/verdicts.golden.json` (`TestVerdictsGolden`) in
`test/parity.test.ts`: **identical accept/reject over all 131 fixtures**, plus
exact (severity, code, path) equality over the validate stage. `cel.test.ts` /
`validate.test.ts` add focused tables.

## Scripts

| Script | What |
|---|---|
| `pnpm generate` | regenerate `src/generated/definition.ts` from the schema |
| `pnpm typecheck` | `tsc --noEmit` |
| `pnpm test` | vitest (round-trip, passthrough, mapping, boundary) |
| `pnpm lint` | eslint (incl. the no-React/UI boundary rule) |
| `pnpm build` | `tsc --build` → `dist/` |
