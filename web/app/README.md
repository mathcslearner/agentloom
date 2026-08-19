# @agentloom/app

The agentloom web app — the visual DAG builder (M17) and live execution
dashboard (M18). Next.js App Router, TypeScript strict, Tailwind v4 +
shadcn/ui.

M17.1 ships the scaffold: the typed API client wiring, a same-origin proxy that
holds the key server-side, and definition/run list pages that read the compose
backend through the typed client.

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
    page.tsx              home — backend health / setup
    definitions/page.tsx  server-rendered definition list (direct client)
    runs/page.tsx         client-rendered run list (proxy client)
    api/agentloom/[...path]/route.ts   the same-origin proxy
  components/ui/          shadcn-style primitives (badge, button, card, table)
  lib/
    api/{server,browser}.ts   the two typed-client factories
    config.ts             server-only env
    status.ts             run-status → badge variant
```

The typed clients come from the workspace libraries `@agentloom/api-client`
(REST) and `@agentloom/engine-client` (event WebSocket).
