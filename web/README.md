# agentloom web

The agentloom frontend workspace. A [pnpm](https://pnpm.io) workspace managed
via Corepack (`packageManager` pins the exact version).

## Packages

- **`lib/engine-client`** — the typed event-feed client (M16.5). Event types are
  generated from the backend's committed `docs/schema/events.v1.json`; the client
  implements ticket auth, snapshot → backfill → live-tail, seq dedupe, resume,
  and reconnection backoff. Usable from Node for headless tailing.
- **`lib/api-client`** — the typed REST client (M17.1). Types are generated from
  `api/openapi.yaml` (`openapi-typescript`, CI-diffed); the runtime is a thin
  `openapi-fetch` wrapper with optional bearer-key injection and the `problem()`
  error-envelope helper.
- **`app`** — the Next.js visual builder + live dashboard (M17.1 onward). App
  Router, TS strict, Tailwind + shadcn/ui. Talks to the backend through a
  same-origin proxy that holds the API key server-side. See `app/README.md`.

Both `lib/*` packages are pure TypeScript with no React/UI imports.

## Setup

```bash
corepack enable            # once, to activate the pinned pnpm
cd web && pnpm install
```

## Common tasks (run from `web/`)

```bash
pnpm generate    # regenerate types from the backend schemas + OpenAPI spec
pnpm lint        # eslint (app) + tsc (libs) across all packages
pnpm typecheck   # tsc --noEmit across all packages
pnpm test        # vitest across all packages
pnpm build       # build all packages (Next app + lib type builds)
pnpm dev         # run the Next.js app in dev mode
pnpm e2e         # Playwright smoke for the app
```

Or from the repo root: `make web-test`, `make web-build`, `make web-dev`,
`make web-e2e`.

After changing any Go wire shape, regenerate both sides:

```bash
make generate        # (repo root) Go structs -> docs/schema/*.json + openapi.yaml stays hand-maintained
cd web && pnpm generate   # docs/schema/events.v1.json -> engine-client TS,
                          # api/openapi.yaml            -> api-client TS
```

CI fails if the committed generated TS is stale against the schemas or spec.
