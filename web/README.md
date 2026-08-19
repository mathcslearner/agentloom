# agentloom web

The agentloom frontend workspace. A [pnpm](https://pnpm.io) workspace managed
via Corepack (`packageManager` pins the exact version).

## Packages

- **`lib/engine-client`** — the typed event-feed client (M16.5). Event types are
  generated from the backend's committed `docs/schema/events.v1.json`; the client
  implements ticket auth, snapshot → backfill → live-tail, seq dedupe, resume,
  and reconnection backoff. Usable from Node for headless tailing. This is the
  exact client M17/M18 build on.

The Next.js visual builder + dashboard (M17/M18) are added as further packages.

## Setup

```bash
corepack enable            # once, to activate the pinned pnpm
cd web && pnpm install
```

## Common tasks (run from `web/`)

```bash
pnpm generate    # regenerate types from the backend JSON Schemas
pnpm typecheck   # tsc --noEmit across all packages
pnpm test        # vitest across all packages
pnpm build       # tsc build across all packages
```

After changing any Go event payload, regenerate both sides:

```bash
make generate        # (repo root) Go structs -> docs/schema/*.json
cd web && pnpm generate   # docs/schema/events.v1.json -> generated TS
```

CI fails if the committed generated TS is stale against the schema.
