/**
 * Server-side typed client — used by server components and the proxy. Holds the
 * API key (from the environment); this module is server-only.
 */
import "server-only";
import { createApiClient, type ApiClient } from "@agentloom/api-client";
import { serverConfig } from "@/lib/config";

/** Build a client pointed straight at the backend with the server-held key. */
export function serverApi(): ApiClient {
  const { apiUrl, apiKey } = serverConfig();
  return createApiClient(apiKey ? { baseUrl: apiUrl, apiKey } : { baseUrl: apiUrl });
}
