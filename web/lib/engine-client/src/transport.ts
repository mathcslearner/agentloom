/**
 * Transport seams — everything the client touches that isn't pure logic:
 * the WebSocket, the timers, and `fetch`. All injectable so the client is
 * testable with an in-memory fake transport and a fake clock (the repo's
 * "time is injectable" invariant), and portable across Node and the browser
 * (both expose a global `WebSocket` and `fetch`; no `ws` dependency).
 */

/**
 * The minimal WebSocket surface the client uses. The global `WebSocket` (Node
 * >= 22 and every browser) satisfies it structurally. Only text frames are
 * used, so `message.data` is always a string.
 */
export interface WebSocketLike {
  readonly readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onopen: ((ev: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: { code: number; reason: string; wasClean: boolean }) => void) | null;
  onerror: ((ev: unknown) => void) | null;
}

/** Opens a WebSocket to `url`. Default uses the global `WebSocket`. */
export type WebSocketFactory = (url: string) => WebSocketLike;

export const defaultWebSocketFactory: WebSocketFactory = (url) => {
  const Ctor = (globalThis as { WebSocket?: new (url: string) => unknown }).WebSocket;
  if (!Ctor) {
    throw new Error(
      "no global WebSocket available; pass a webSocketFactory (Node < 22 or a custom transport)",
    );
  }
  return new Ctor(url) as unknown as WebSocketLike;
};

export type TimerHandle = { readonly __timer: unique symbol } | ReturnType<typeof setTimeout>;

/** The clock + timer seam. Default wraps the host timers. */
export interface Scheduler {
  setTimeout(fn: () => void, ms: number): TimerHandle;
  clearTimeout(handle: TimerHandle): void;
  now(): number;
}

export const systemScheduler: Scheduler = {
  setTimeout: (fn, ms) => setTimeout(fn, ms) as TimerHandle,
  clearTimeout: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
  now: () => Date.now(),
};

/** A structured logger. Default discards everything. */
export interface Logger {
  debug(msg: string, meta?: Record<string, unknown>): void;
  info(msg: string, meta?: Record<string, unknown>): void;
  warn(msg: string, meta?: Record<string, unknown>): void;
  error(msg: string, meta?: Record<string, unknown>): void;
}

export const noopLogger: Logger = {
  debug: () => {},
  info: () => {},
  warn: () => {},
  error: () => {},
};

export type FetchLike = typeof fetch;

export function resolveFetch(f: FetchLike | undefined): FetchLike {
  const impl = f ?? (globalThis as { fetch?: FetchLike }).fetch;
  if (!impl) throw new Error("no global fetch available; pass fetchImpl");
  return impl;
}

/** Derive the WebSocket origin from an http(s) base URL. */
export function toWebSocketUrl(baseUrl: string, path: string, query: URLSearchParams): string {
  const u = new URL(path, baseUrl);
  u.protocol = u.protocol === "https:" ? "wss:" : "ws:";
  u.search = query.toString();
  return u.toString();
}
