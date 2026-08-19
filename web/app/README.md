# @agentloom/app

The agentloom web app — the visual DAG builder (M17) and live execution
dashboard (M18). Next.js App Router, TypeScript strict, Tailwind v4 +
shadcn/ui.

M17.1 ships the scaffold: the typed API client wiring, a same-origin proxy that
holds the key server-side, and definition/run list pages that read the compose
backend through the typed client. **M17.3** adds the visual DAG builder at
`/builder` — a React Flow canvas over the `@agentloom/graphdef` serialization
boundary, with a node palette, typed connection handles, and undo/redo.

## Backend access (proxy mode)

The agentloom API has **no CORS**, and its key must never reach the browser. So:

- **Server-side** (server components, and the proxy) hold the key from the
  environment and talk to the backend directly.
- **The browser** talks only to a same-origin route handler at
  `/api/agentloom/**` (`src/app/api/agentloom/[...path]/route.ts`), which injects
  `Authorization: Bearer <key>` and forwards the request. Only `v1/**` and
  `healthz` are proxied.

Config (server-only; never prefix with `NEXT_PUBLIC_`):

| Env | Meaning | Default |
|-----|---------|---------|
| `AGENTLOOM_API_URL` | Backend origin | `http://127.0.0.1:8080` |
| `AGENTLOOM_API_KEY` | Bearer key (scopes `read` + `submit`) | — (required) |

Copy `.env.example` to `.env.local` and set the key. For the compose stack use
the `AGENTLOOM_API_ROOT_KEY` you booted with, or mint a scoped key:

```bash
AGENTLOOM_API_KEY=<root> ctl keys create --scopes read,submit
```

## Develop

```bash
# from web/app (or `make web-dev` from the repo root)
pnpm dev            # http://localhost:3000
pnpm lint           # eslint (flat config; graphdef boundary rule seeded for 17.2)
pnpm typecheck      # tsc --noEmit
pnpm test           # vitest (jsdom): proxy + page rendering
pnpm build          # next build (also runs lint + typecheck)
pnpm build:verify   # build with a sentinel key; assert it's absent from .next/static
pnpm e2e            # Playwright smoke (needs a running app + backend; see below)
```

## End-to-end smoke

`scripts/web-e2e-smoke.sh` (repo root, `make web-e2e`) boots the compose app
profile, builds the app, and runs the Playwright smoke against it: the app lists
definitions and runs through the typed client and the proxy, and the smoke
asserts no browser request carries an Authorization header. In CI this is the
`web-e2e` job.

## Structure

```
src/
  app/                    App Router pages
    (site)/               centred-column list pages (route group; URLs unchanged)
      page.tsx            home — backend health / setup
      definitions/page.tsx  server-rendered definition list (direct client)
      runs/page.tsx       client-rendered run list (proxy client)
    (builder)/builder/    full-bleed visual DAG builder (M17.3)
    api/agentloom/[...path]/route.ts   the same-origin proxy
  components/
    ui/                   shadcn-style primitives (badge, button, card, table)
    builder/              canvas, palette, node/edge components, inspector shell
  lib/
    api/{server,browser}.ts   the two typed-client factories
    config.ts             server-only env
    status.ts             run-status → badge variant
    builder/              zustand+zundo store, adapter, keyboard shortcuts
    pure/builder/         pure canvas helpers (catalog, ports, steps) — no React
```

The typed clients come from the workspace libraries `@agentloom/api-client`
(REST) and `@agentloom/engine-client` (event WebSocket); the visual builder maps
canvas state ⇄ definition JSON through `@agentloom/graphdef` (the serialization
boundary, ADR-019).

## Visual builder (M17.3)

`/builder` is a React Flow canvas over `@agentloom/graphdef`. The palette adds
any catalog step type (drag-to-create, or click / Enter); nodes carry typed
connection handles (`in`; `out`/`loop`; `approve`/`reject` on approval steps);
the store (zustand + zundo) gives undo/redo with drag-coalescing. A new node
starts with an empty config. The presentational `StepNodeView` is reused by the
M18 dashboard with run-status skins.

## Schema-driven config panels (M17.4)

Selecting a step opens a config panel whose form is **generated from the
plugin's JSON Schema** (`GET /v1/plugins`, fetched once through the proxy into
`catalog-store.ts`, with the published-schema fallback when the catalog is
unavailable) — no per-plugin hardcoded forms. `SchemaForm` renders a widget per
field (model picker, tool/retriever/agent/template picker, prompt editor with
upstream-only `${{ }}` autocomplete, enum select, number/boolean, string/object
lists, JSON-editor fallback for raw-JSON fields). Required-ness and specialized
widgets come from a small hand-maintained `hints.ts` (the schema encodes
neither). Invalid config marks the node (a destructive ring + a problem count)
and the toolbar shows the total error count; the client validator lives in
`@agentloom/graphdef` and reports the backend's issue codes/paths (proven
against the Go verdict golden). The autocomplete offers exactly the upstream
steps' output paths (ancestors over normal edges only, mirroring the backend)
and declared run params.

## Client-side graph validation (M17.5)

`useProblems` runs `@agentloom/graphdef`'s full `validateDefinition` over the
live canvas and maps every issue onto its node or edge by array index. A hoisted
`ProblemsProvider` (inside `ReactFlowProvider`) runs that one validation and
provides per-element error counts, highlight state, and `focusIssue`. The
**Problems panel** (`ProblemsPanel`, in the Inspector) lists every problem
(errors first, warnings after), and **clicking a row selects the element the
issue's own path points at** — a cycle's `edges[i]` opens the minimal edge
inspector (`EdgePanel`) where it's fixed by marking the edge a loop — and
`fitView`s to it; hovering previews the highlight. Invalid/highlighted nodes and
edges get destructive/amber strokes. The toolbar shows `N errors · M warnings`
and disables Save/Submit while errors block them.

## Import, export, save & submit (M17.6)

The toolbar's action cluster (`BuilderActions`) owns the persistence flows:

- **Import** (`ImportDialog`): load a definition from a file or pasted JSON.
  Parse and `toFlow` shape errors are surfaced; a `validateDefinition` summary is
  shown before loading; an invalid-but-well-shaped definition can still be
  imported (the Problems panel then reports it). Importing clears the source.
- **Export**: downloads `canonicalize(toDefinition(...))` as `<name>.json` — the
  byte-for-byte canonical form the backend stores (`@agentloom/graphdef`'s
  `canonicalize`, pinned against a Go golden).
- **Save** (`SaveDialog`): creates version 1 for a fresh name, or appends the
  next version for a canvas opened from a stored definition, guarded by an
  `If-Match: <opened-version>` precondition. A name clash offers "append a
  version instead"; a `409 version_conflict` (someone appended since) opens a
  reconcile prompt with "Save anyway"; a 400 surfaces the backend's issues and
  selects the first offending step.
- **Submit** (`SubmitDialog`): a params modal (typed, required-enforced fields
  from the definition's `params`), submitting by `definition_id` when the canvas
  is clean and saved, else inline, with a fresh `Idempotency-Key`.
- **Open in builder**: the definitions list links to `/builder?definition=<id>`,
  which loads the stored spec client-side through the proxy.
- **Unsaved-changes guard** (`NavigationGuard`): a dirty flag (`selectIsDirty`)
  drives a `beforeunload` warning and an in-app confirm on internal navigation,
  plus a discard prompt before Import.

Dialogs use `@radix-ui/react-dialog`; a tiny zustand `toast` surface reports
outcomes. E2e coverage: `e2e/import-export.spec.ts` (client-only — byte-for-byte
export through the DOM, the nav guard) and `e2e/save-submit.spec.ts` (compose —
the full import → edit → save v1/v2 → version-conflict → save anyway → submit
loop, and open-in-builder).

## Live execution dashboard (M18.1–18.4)

The dashboard watches runs execute live over the event feed
(`@agentloom/engine-client`, ADR-018). 18.1 shipped the run list and the
run-detail scaffold; 18.2 replaced the steps pane with the live DAG canvas; 18.3
filled in the tabbed step inspector; 18.4 added the live cost meter and budget
controls. Later M18 tickets add the approval inbox and ops views.

- **`/runs`** — a runs table with live status chips over the multi-run firehose,
  status/definition/time filters (synced to the URL), keyset pagination, a
  connection pill, and per-row links into the detail page. A run submitted while
  the page is open appears live (via `run_created`).
- **`/runs/{id}`** — the run-detail view: a header (status, counters, cost,
  connection), the live **DAG canvas** (18.2 — builder node components with
  run-status skins, animated active edges, sticky elkjs layout honoring `ui`
  hints, planner/map/loop injections animating in with a provenance badge, and
  loop/map instances grouped into collapsible containers), the tabbed **step
  inspector** (18.3), and the collapsible **event timeline strip** (category
  filtering, click-to-select a step). State loads snapshot → backfill →
  live-tail through the 16.5 client and resumes across reconnects with no gaps.

### The cost meter & budget UX (18.4)

The run header carries a live **cost meter** (`CostMeter`): a spend ticker, a
saved-by-cache indicator, and — on a budgeted run — a budget progress bar with
threshold colouring (`ok`/`warn`/`danger`/`exceeded` at 75/90/100%). It is
**stateless off the event feed** — the running totals ride `cost_updated`
(non-decreasing in seq, 10.5) and the budget rides `run_budget_updated`, both
folded into `run.cost` by the run-state reducer under the seq guard, so the
meter updates live with no cost refetch. Downgrades, budget-exceeded parks/fails,
and budget raises surface as dismissible **banners** (`BudgetBanners`); a live
"parked at cap" banner carries a **Raise budget** action. The
`RaiseBudgetDialog` does the documented resume path (ADR-012): `PATCH
.../budget` then optional `POST .../unpark`, with park→resume reflected live via
`run_budget_updated`+`run_unparked`. Pure derivations live in
`src/lib/pure/dashboard/{cost-meter,budget-banners}.ts` (unit-tested, incl. a
`foldCostEvents` convergence check against the Go cost golden). E2e:
`e2e/dashboard-cost.spec.ts` climbs the meter, parks at cap, raises + resumes,
asserts a downgrade banner, and checks the meter total equals `GET
/v1/runs/{id}/cost` at completion.

### The step inspector (18.3)

Selecting a node opens a five-tab pane
(`src/components/dashboard/inspector/`): **Overview** (timings, attempt
timeline, model chain incl. downgrades, idempotency key, claim/worker history),
**Output** (a dependency-free JSON tree viewer), **Logs** (per-attempt via the
7.4 API, level filter, follow mode — logs are poll-based in v1), **Validation**
(per-attempt verdicts + issues, and the **semantic-retry prompt diff** — the
killer demo: the effective prompts of two attempts diffed, showing the feedback
augmentation as pure additions), and **Cost** (per-attempt ledger rows incl.
`llm_judge` overhead and cache savings). The tab content is derived by pure
functions under `src/lib/pure/dashboard/{inspector,inspector-cost,logs,diff}.ts`,
unit-tested against the committed Go goldens `internal/api/testdata/run_{detail,cost}_fixture.json`
+ `step_logs_fixture.json`. The full `StepView` (attempts/verdicts) is refetched
from `GET /v1/runs/{id}` as the selected step advances (`useStepInspector`), so
new attempts appear without a reload. E2e: `e2e/dashboard-inspector.spec.ts`
walks every tab against a compose semantic-retry run and asserts the prompt
diff; the gated `dashboard-crash.spec.ts` asserts the reclaimed step's claim
history names both workers.

### The live DAG (18.2)

The DAG is built from three additive sources unioned together (expansion is
additive-only, so the merge is order-independent): the WS snapshot topology, the
`GET /v1/runs/{id}/graph` read (provenance, depth, `ui` positions, edge
`when`/`decision`), and live `graph_expanded` deltas. Status comes from the
seq-guarded step map, layout is **sticky** (placed nodes never move — only new
injected blocks are laid out and anchored next to their origin), and the pure
reducers (`src/lib/pure/dashboard/{graph-topology,skins,layout,groups,projection}.ts`)
are unit-tested without a canvas. The **crash-recovery e2e** (`dashboard-crash.spec.ts`)
is destructive (it SIGKILLs a compose worker) so it is gated behind
`AGENTLOOM_E2E_CRASH=1`; a plain `pnpm e2e` never touches the fleet.

### WebSocket connectivity

A WebSocket upgrade cannot be forwarded through the same-origin proxy, so the
browser dials the API's `/ws` endpoints **directly** at `AGENTLOOM_API_PUBLIC_URL`
(defaults to `AGENTLOOM_API_URL`). The API key never rides the upgrade — the
browser mints a short-lived ws-ticket through the proxy (so the key stays
server-side) and passes it as `?ticket=`. Because the app and API are different
origins, the API must allowlist the app's origin via `AGENTLOOM_API_WS_ORIGINS`
(compose defaults it to the local dev origins).

Pure reducers live under `src/lib/pure/dashboard/` (the no-React eslint
boundary), unit-tested in `test/dashboard/`; the stream glue and hooks under
`src/lib/dashboard/`. E2e: `e2e/dashboard-runs.spec.ts` (a submitted run appears
and flips status live) and `e2e/dashboard-run-detail.spec.ts` (mid-run reconnect
resumes gap-free; timeline category filtering).
