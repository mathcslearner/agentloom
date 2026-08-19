# @agentloom/api-client

The typed REST client for the agentloom control-plane API (ticket 17.1).

- **Types** are generated from `api/openapi.yaml` into `src/generated/openapi.ts`
  by `scripts/gen-openapi-types.ts` (`pnpm generate`). The spec is the
  hand-maintained contract that `internal/api` implements and CI keeps in
  lockstep with the routes; this package's types cannot drift from it — CI
  regenerates and fails on any diff (the same two-layer discipline as
  `@agentloom/engine-client`'s event types).
- **Runtime** is a thin [`openapi-fetch`](https://openapi-ts.dev/openapi-fetch/)
  client. The only additions are optional bearer-key injection and the
  `problem()` error-envelope helper.

## Usage

```ts
import { createApiClient, problem } from "@agentloom/api-client";

// Server-side (Node): the key stays on the server.
const api = createApiClient({ baseUrl: "http://127.0.0.1:8080", apiKey: process.env.AGENTLOOM_API_KEY });

const { data, error } = await api.GET("/v1/definitions");
if (error) throw new Error(problem(error)?.message ?? "request failed");
console.log(data.definitions);

// Browser: no key — talk to the same-origin proxy, which injects it.
const browserApi = createApiClient({ baseUrl: "/api/agentloom" });
```

The API has no CORS, so the browser never calls it directly: a browser client
points at a same-origin proxy (the Next.js app's `/api/agentloom/**` route
handler) that holds the key server-side.

## Regenerating types

```bash
pnpm generate     # api/openapi.yaml -> src/generated/openapi.ts
```

After any change to the Go handlers' wire shapes, update `api/openapi.yaml`
(the route/operation tests enforce coverage), then regenerate here. CI fails if
the committed generated file is stale.

## Frame parity

`test/frame-parity.test.ts` asserts, at compile time, that
`@agentloom/engine-client`'s hand-written WebSocket wire frames stay
structurally compatible with this spec's `WS*Frame` schemas — closing the M16.5
residual without adding a runtime dependency to that pure library.
