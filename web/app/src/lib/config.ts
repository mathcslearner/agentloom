/**
 * Server-only configuration. This module must never be imported by a client
 * component — it reads the API key from the environment, which stays on the
 * server. The proxy (src/app/api/agentloom/[...path]/route.ts) and server
 * components use it; browser code talks to the same-origin proxy instead.
 *
 * Env names are the same `ctl` uses (see .env.example):
 *   AGENTLOOM_API_URL  — the backend origin (default http://127.0.0.1:8080)
 *   AGENTLOOM_API_KEY  — the bearer key (never sent to the browser)
 */
import "server-only";

export interface ServerConfig {
  apiUrl: string;
  apiKey: string | undefined;
}

export function serverConfig(): ServerConfig {
  return {
    apiUrl: process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080",
    apiKey: process.env.AGENTLOOM_API_KEY || undefined,
  };
}

/** True when a backend key is configured. Pages show a setup card otherwise. */
export function isConfigured(): boolean {
  return Boolean(serverConfig().apiKey);
}
