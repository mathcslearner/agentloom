/**
 * Server-only configuration. This module must never be imported by a client
 * component — it reads the API key from the environment, which stays on the
 * server. The proxy (src/app/api/agentloom/[...path]/route.ts) and server
 * components use it; browser code talks to the same-origin proxy instead.
 *
 * Env names are the same `ctl` uses (see .env.example):
 *   AGENTLOOM_API_URL         — the backend origin (default http://127.0.0.1:8080)
 *   AGENTLOOM_API_KEY         — the bearer key (never sent to the browser)
 *   AGENTLOOM_API_PUBLIC_URL  — the browser-reachable backend origin, used ONLY
 *                               for the dashboard's WebSocket connection (M18.1).
 *                               A WebSocket upgrade cannot be forwarded through a
 *                               Next.js route handler, so the browser dials the
 *                               API directly for the live event feed; the key
 *                               never rides along (a proxy-minted ws-ticket does).
 *                               Defaults to AGENTLOOM_API_URL. This is a public
 *                               value (an origin, no secret) — safe to expose to
 *                               the browser via the runtime-config provider.
 */
import "server-only";

export interface ServerConfig {
  apiUrl: string;
  apiKey: string | undefined;
  apiPublicUrl: string;
}

export function serverConfig(): ServerConfig {
  const apiUrl = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
  return {
    apiUrl,
    apiKey: process.env.AGENTLOOM_API_KEY || undefined,
    apiPublicUrl: process.env.AGENTLOOM_API_PUBLIC_URL || apiUrl,
  };
}

/** True when a backend key is configured. Pages show a setup card otherwise. */
export function isConfigured(): boolean {
  return Boolean(serverConfig().apiKey);
}
