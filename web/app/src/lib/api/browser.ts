/**
 * Browser-side typed client — used by client components. It carries no key and
 * targets the same-origin proxy at `/api/agentloom`, which injects the key
 * server-side. The API has no CORS, so the browser never calls it directly.
 */
import { createApiClient, type ApiClient } from "@agentloom/api-client";

/** The proxy mount point (a Next.js route handler). */
export const PROXY_BASE = "/api/agentloom";

let cached: ApiClient | undefined;

export function browserApi(): ApiClient {
  cached ??= createApiClient({ baseUrl: PROXY_BASE });
  return cached;
}
