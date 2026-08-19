/**
 * Ticket auth (ADR-018, 16.3/16.4). A browser cannot set an `Authorization`
 * header on a WebSocket, so it mints a short-lived signed ticket over REST and
 * passes it as a `?ticket=` query parameter. The ticket TTL is ~60s, so a fresh
 * one is minted on every (re)connect.
 *
 * Two auth modes:
 *   - `{ apiKey }`      — the client mints tickets itself with a `read` bearer.
 *                         Suitable for Node / server-side; the key stays server-side.
 *   - `{ mintTicket }`  — the caller supplies tickets (e.g. a browser calling its
 *                         own server-side proxy, so the API key never reaches the
 *                         browser). This is the M17 dashboard path.
 */
import type { FetchLike } from "./transport.js";
import { resolveFetch } from "./transport.js";

export type AuthProvider =
  | { readonly apiKey: string }
  | { readonly mintTicket: () => Promise<string> };

/** The audience a ticket is minted for; the routes are audience-split. */
export type TicketAudience = "run" | "firehose";

export interface MintOptions {
  baseUrl: string;
  fetchImpl?: FetchLike;
  /** Required for the `run` audience; ignored for `firehose`. */
  runId?: string;
}

/** The ws-ticket mint path for an audience. */
export function ticketPath(audience: TicketAudience, runId?: string): string {
  if (audience === "run") {
    if (!runId) throw new Error("run ticket requires a runId");
    return `/v1/runs/${encodeURIComponent(runId)}/ws-ticket`;
  }
  return `/v1/events/ws-ticket`;
}

/** Thrown when a ticket mint fails (surfaced so the caller can back off/retry). */
export class TicketError extends Error {
  constructor(
    message: string,
    readonly status?: number,
  ) {
    super(message);
    this.name = "TicketError";
  }
}

/**
 * Resolve a ticket for one (re)connect. With `mintTicket` the caller owns it;
 * with `apiKey` we POST the ws-ticket route and read `{ ticket }`.
 */
export async function resolveTicket(
  auth: AuthProvider,
  audience: TicketAudience,
  opts: MintOptions,
): Promise<string> {
  if ("mintTicket" in auth) {
    return auth.mintTicket();
  }
  const fetchImpl = resolveFetch(opts.fetchImpl);
  const url = new URL(ticketPath(audience, opts.runId), opts.baseUrl).toString();
  let res: Response;
  try {
    res = await fetchImpl(url, {
      method: "POST",
      headers: { authorization: `Bearer ${auth.apiKey}`, accept: "application/json" },
    });
  } catch (err) {
    throw new TicketError(`ws-ticket request failed: ${(err as Error).message}`);
  }
  if (!res.ok) {
    throw new TicketError(`ws-ticket request returned ${res.status}`, res.status);
  }
  const body = (await res.json()) as { ticket?: unknown };
  if (typeof body.ticket !== "string" || body.ticket === "") {
    throw new TicketError("ws-ticket response missing `ticket`");
  }
  return body.ticket;
}
