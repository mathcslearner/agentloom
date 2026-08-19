/**
 * @agentloom/api-client — the typed REST client for the agentloom control-plane
 * API (ticket 17.1).
 *
 * Types are generated from `api/openapi.yaml` into `./generated/openapi.ts`
 * (`pnpm generate`, CI-diffed). The runtime is a thin `openapi-fetch` client
 * whose only additions are (1) optional bearer-key injection via a request
 * middleware and (2) the `problem()` helper that narrows the API's error
 * envelope. Everything else — path/query/body typing, response narrowing — is
 * `openapi-fetch` over the generated `paths`.
 *
 * Two intended callers:
 *   - server-side (Node): `createApiClient({ baseUrl, apiKey })` — the key stays
 *     on the server (the Next.js proxy / server components, ticket 17.1).
 *   - browser: `createApiClient({ baseUrl: "/api/agentloom" })` — no key; the
 *     same-origin proxy injects it. The API has no CORS, so the browser never
 *     talks to it directly.
 */
import createOpenApiClient from "openapi-fetch";
import type { Client, Middleware } from "openapi-fetch";
import type { components, paths } from "./generated/openapi.js";

export type { paths, components, operations } from "./generated/openapi.js";

/** Shorthand for a generated component schema (e.g. `Schema<"RunView">`). */
export type Schema<K extends keyof components["schemas"]> = components["schemas"][K];

/** The most-used response/view shapes, re-exported for ergonomic imports. */
export type RunView = Schema<"RunView">;
export type StepView = Schema<"StepView">;
export type RunResponse = Schema<"RunResponse">;
export type ListRunsResponse = Schema<"ListRunsResponse">;
export type RunStatus = Schema<"RunStatus">;
export type DefinitionView = Schema<"DefinitionView">;
export type ListDefinitionsResponse = Schema<"ListDefinitionsResponse">;
export type EdgeView = Schema<"EdgeView">;
export type PluginInfoView = Schema<"PluginInfo">;
export type ListPluginsResponse = Schema<"ListPluginsResponse">;
// The run-graph introspection view (ticket 13.6/18.2, ADR-015) — the dashboard's
// live-DAG topology + provenance + expansion deltas.
export type RunGraphResponse = Schema<"RunGraphResponse">;
export type GraphNodeView = Schema<"GraphNodeView">;
export type GraphEdgeView = Schema<"GraphEdgeView">;
export type GraphExpansionView = Schema<"GraphExpansionView">;
export type GraphOriginView = Schema<"GraphOriginView">;
export type GraphPositionView = Schema<"GraphPositionView">;
export type ErrorEnvelope = Schema<"Error">;
export type ErrorDetail = Schema<"ErrorDetail">;
export type ErrorCode = Schema<"ErrorCode">;
export type Issue = Schema<"Issue">;

/** The typed client surface (`.GET`, `.POST`, … over the generated `paths`). */
export type ApiClient = Client<paths>;

export interface ApiClientOptions {
  /** Base URL — the backend origin server-side, or the proxy path in a browser. */
  baseUrl: string;
  /**
   * Bearer API key. Server-side only — never pass this in browser code; the
   * same-origin proxy holds it. When omitted, no `Authorization` header is set.
   */
  apiKey?: string;
  /** Injectable fetch (tests, custom agents). Defaults to the global `fetch`. */
  fetch?: typeof fetch;
}

/**
 * Build a typed client. When `apiKey` is set, a request middleware attaches
 * `Authorization: Bearer <key>` to every call (only place the key is used).
 */
export function createApiClient(opts: ApiClientOptions): ApiClient {
  const client = createOpenApiClient<paths>({
    baseUrl: opts.baseUrl,
    ...(opts.fetch ? { fetch: opts.fetch } : {}),
  });
  if (opts.apiKey) {
    const key = opts.apiKey;
    const auth: Middleware = {
      onRequest({ request }) {
        request.headers.set("authorization", `Bearer ${key}`);
        return request;
      },
    };
    client.use(auth);
  }
  return client;
}

/**
 * Narrow an `openapi-fetch` error to the API's `{ error: { code, message,
 * issues? } }` envelope. Returns the `ErrorDetail` when the value matches, else
 * `null` (a transport error, or a non-conforming body). Callers switch on
 * `.code` (a stable machine string) and render `.issues` for definition errors.
 */
export function problem(error: unknown): ErrorDetail | null {
  if (error === null || typeof error !== "object") return null;
  const env = error as { error?: unknown };
  if (env.error === null || typeof env.error !== "object") return null;
  const detail = env.error as { code?: unknown; message?: unknown };
  if (typeof detail.code !== "string" || typeof detail.message !== "string") return null;
  return env.error as ErrorDetail;
}
