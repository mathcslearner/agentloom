/**
 * In-memory fakes for the transport seams, so the streaming clients are tested
 * deterministically with no real sockets, network, or wall-clock timers.
 */
import type {
  FetchLike,
  Scheduler,
  TimerHandle,
  WebSocketFactory,
  WebSocketLike,
} from "../src/transport.js";

/** A fake WebSocket the test drives (open / emit frames / server-close). */
export class FakeSocket implements WebSocketLike {
  readyState = 0; // CONNECTING
  readonly sent: string[] = [];
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: ((ev: { code: number; reason: string; wasClean: boolean }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  clientClose: { code?: number; reason?: string } | null = null;

  constructor(readonly url: string) {}

  send(data: string): void {
    this.sent.push(data);
  }

  close(code?: number, reason?: string): void {
    this.clientClose = { ...(code !== undefined ? { code } : {}), ...(reason !== undefined ? { reason } : {}) };
    this.readyState = 3; // CLOSED
  }

  // ── test drivers ──
  open(): void {
    this.readyState = 1; // OPEN
    this.onopen?.({});
  }

  emit(frame: unknown): void {
    this.onmessage?.({ data: JSON.stringify(frame) });
  }

  emitRaw(data: string): void {
    this.onmessage?.({ data });
  }

  /** Simulate the server closing the connection (not clean). */
  serverClose(code: number, reason = ""): void {
    this.readyState = 3;
    this.onclose?.({ code, reason, wasClean: false });
  }

  sentMessages(): unknown[] {
    return this.sent.map((s) => JSON.parse(s));
  }
}

/** A WebSocket factory that records every socket it opens. */
export class FakeTransport {
  readonly sockets: FakeSocket[] = [];

  readonly factory: WebSocketFactory = (url) => {
    const s = new FakeSocket(url);
    this.sockets.push(s);
    return s;
  };

  last(): FakeSocket {
    const s = this.sockets.at(-1);
    if (!s) throw new Error("no socket opened yet");
    return s;
  }

  count(): number {
    return this.sockets.length;
  }
}

interface Timer {
  handle: TimerHandle;
  at: number;
  fn: () => void;
}

/** A controllable scheduler: timers fire only when the test advances the clock. */
export class FakeScheduler implements Scheduler {
  private t = 0;
  private readonly timers: Timer[] = [];

  now(): number {
    return this.t;
  }

  setTimeout(fn: () => void, ms: number): TimerHandle {
    const handle = { __timer: Symbol() } as unknown as TimerHandle;
    this.timers.push({ handle, at: this.t + Math.max(0, ms), fn });
    return handle;
  }

  clearTimeout(handle: TimerHandle): void {
    const i = this.timers.findIndex((t) => t.handle === handle);
    if (i >= 0) this.timers.splice(i, 1);
  }

  /** Advance the clock by `ms` and fire every timer that becomes due, in order. */
  advance(ms: number): void {
    this.t += ms;
    for (;;) {
      const due = this.timers
        .filter((t) => t.at <= this.t)
        .sort((a, b) => a.at - b.at);
      const next = due[0];
      if (!next) break;
      this.timers.splice(this.timers.indexOf(next), 1);
      next.fn();
    }
  }

  pending(): number {
    return this.timers.length;
  }
}

/** A fetch that mints tickets, recording each call. Fails when `fail` is set. */
export function fakeTicketFetch(opts: { fail?: boolean; status?: number } = {}): {
  fetch: FetchLike;
  calls: { url: string; authorization: string | undefined }[];
} {
  const calls: { url: string; authorization: string | undefined }[] = [];
  const fetch: FetchLike = async (input, init) => {
    const url = typeof input === "string" ? input : input.toString();
    const headers = new Headers(init?.headers);
    calls.push({ url, authorization: headers.get("authorization") ?? undefined });
    if (opts.fail) {
      return { ok: false, status: opts.status ?? 503, json: async () => ({}) } as unknown as Response;
    }
    return {
      ok: true,
      status: 200,
      json: async () => ({ ticket: `ticket-${calls.length}` }),
    } as unknown as Response;
  };
  return { fetch, calls };
}

/** Flush pending microtasks/promises (the ticket-mint await chain). */
export async function flush(rounds = 5): Promise<void> {
  for (let i = 0; i < rounds; i++) {
    await new Promise<void>((resolve) => setImmediate(resolve));
  }
}
