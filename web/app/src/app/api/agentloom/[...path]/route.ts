/**
 * Same-origin API proxy (ticket 17.1).
 *
 * The agentloom API has no CORS and its key must never reach the browser, so
 * browser clients call this same-origin route instead of the backend directly.
 * The handler injects `Authorization: Bearer <key>` server-side (the only place
 * the key is used in the request path) and forwards everything else verbatim:
 * method, query string, body, and the response status/body/rate-limit headers.
 *
 * Security properties, each tested:
 *   - The key is read from the server environment and never echoed to the client.
 *   - Only `v1/**` and `healthz` are proxied; anything else is a 404 (no open
 *     relay to arbitrary backend paths).
 *   - Hop-by-hop and client-supplied Authorization headers are dropped inbound;
 *     the proxy sets the one Authorization header itself.
 */
import { NextResponse } from "next/server";
import { serverConfig } from "@/lib/config";

// Node runtime: this reads process.env and holds the key; never the Edge runtime.
export const runtime = "nodejs";
// The key and backend state make every response dynamic.
export const dynamic = "force-dynamic";

/** Only these top-level path segments are proxied. */
const ALLOWED_PREFIXES = ["v1", "healthz"];

/** Response headers worth surfacing to the browser client verbatim. */
const PASSTHROUGH_RESPONSE_HEADERS = [
  "content-type",
  "x-ratelimit-limit",
  "x-ratelimit-remaining",
  "x-ratelimit-reset",
  "retry-after",
  "www-authenticate",
];

/** Request headers safe to forward to the backend. */
const FORWARD_REQUEST_HEADERS = ["content-type", "accept", "idempotency-key", "if-match"];

function envelope(code: string, message: string, status: number): NextResponse {
  return NextResponse.json({ error: { code, message } }, { status });
}

async function proxy(req: Request, path: string[]): Promise<Response> {
  const top = path[0];
  if (!top || !ALLOWED_PREFIXES.includes(top)) {
    return envelope("not_found", "no such proxied route", 404);
  }

  const { apiUrl, apiKey } = serverConfig();
  if (!apiKey) {
    return envelope(
      "unauthorized",
      "the web server has no AGENTLOOM_API_KEY configured; see web/app/.env.example",
      503,
    );
  }

  const incoming = new URL(req.url);
  const target = new URL(path.map(encodeSegment).join("/"), ensureTrailingSlash(apiUrl));
  target.search = incoming.search;

  const headers = new Headers();
  for (const name of FORWARD_REQUEST_HEADERS) {
    const value = req.headers.get(name);
    if (value) headers.set(name, value);
  }
  // The one place the key is used. A client-supplied Authorization header was
  // never copied above, so it cannot override this.
  headers.set("authorization", `Bearer ${apiKey}`);

  const method = req.method.toUpperCase();
  const hasBody = method !== "GET" && method !== "HEAD";
  const init: RequestInit = {
    method,
    headers,
    ...(hasBody ? { body: await req.arrayBuffer(), duplex: "half" } : {}),
  } as RequestInit;

  let upstream: Response;
  try {
    upstream = await fetch(target, init);
  } catch {
    // Deliberately no error detail (could reference the target host/key path).
    return envelope("internal", "upstream request failed", 502);
  }

  const respHeaders = new Headers();
  for (const name of PASSTHROUGH_RESPONSE_HEADERS) {
    const value = upstream.headers.get(name);
    if (value) respHeaders.set(name, value);
  }
  return new NextResponse(upstream.body, { status: upstream.status, headers: respHeaders });
}

function ensureTrailingSlash(u: string): string {
  return u.endsWith("/") ? u : `${u}/`;
}

/**
 * Percent-encode a path segment while preserving a literal `:` verb suffix.
 *
 * Some routes carry a colon verb on the id segment, e.g.
 * `approvals/<uuid>:decide`. A blanket `encodeURIComponent` turns the `:` into
 * `%3A`, which the chi router (matching on the raw path, with `:` as its param
 * delimiter) will not match — the request 404s before the handler. So we encode
 * each colon-delimited part independently and rejoin with a literal `:`.
 */
function encodeSegment(segment: string): string {
  return segment.split(":").map(encodeURIComponent).join(":");
}

interface RouteContext {
  params: Promise<{ path?: string[] }>;
}

async function handle(req: Request, ctx: RouteContext): Promise<Response> {
  const { path } = await ctx.params;
  return proxy(req, path ?? []);
}

export const GET = handle;
export const POST = handle;
export const PATCH = handle;
export const PUT = handle;
export const DELETE = handle;
