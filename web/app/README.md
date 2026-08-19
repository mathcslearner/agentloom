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
the store (zustand + zundo) gives undo/redo with drag-coalescing. Import/export,
save & submit arrive in 17.6; a new node starts with an empty config. The
presentational `StepNodeView` is reused by the M18 dashboard with run-status
skins.

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
`@agentloom/graphdef` (`validateStepConfigs`) and reports the backend's issue
codes/paths (proven against the Go verdict golden). The autocomplete offers
exactly the upstream steps' output paths (ancestors over normal edges only,
mirroring the backend) and declared run params. Client-side graph validation and
the submit flow are 17.5/17.6.
