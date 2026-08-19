import { beforeEach, describe, expect, it } from "vitest";

import type { AuthProvider } from "../src/auth.js";
import type { EventEnvelope, EventType } from "../src/generated/events.js";
import { RunStream } from "../src/run-stream.js";
import type { RunStreamState } from "../src/run-stream.js";
import { FakeScheduler, FakeTransport, fakeTicketFetch, flush } from "./helpers.js";

const RUN = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

function eventFrame(seq: number, type: EventType = "step_ready"): object {
  const env: EventEnvelope = {
    schema_version: 1,
    run_id: RUN,
    seq,
    ts: "2026-01-01T00:00:00Z",
    type,
    payload: { step_id: `s${seq}` } as EventEnvelope["payload"],
  } as EventEnvelope;
  return { type: "event", event: env };
}

interface Harness {
  stream: RunStream;
  transport: FakeTransport;
  scheduler: FakeScheduler;
  ticketCalls: () => { url: string; authorization: string | undefined }[];
  events: number[];
  snapshots: unknown[];
  states: RunStreamState[];
  errors: Error[];
}

function makeStream(
  over: {
    auth?: AuthProvider;
    fetch?: ReturnType<typeof fakeTicketFetch>;
    lastSeq?: number;
    closeOnTerminal?: boolean;
  } = {},
): Harness {
  const transport = new FakeTransport();
  const scheduler = new FakeScheduler();
  const tf = over.fetch ?? fakeTicketFetch();
  const events: number[] = [];
  const snapshots: unknown[] = [];
  const states: RunStreamState[] = [];
  const errors: Error[] = [];

  const stream = new RunStream({
    baseUrl: "http://api.test",
    runId: RUN,
    auth: over.auth ?? { apiKey: "sk_test" },
    ...(over.lastSeq !== undefined ? { lastSeq: over.lastSeq } : {}),
    ...(over.closeOnTerminal !== undefined ? { closeOnTerminal: over.closeOnTerminal } : {}),
    scheduler,
    webSocketFactory: transport.factory,
    fetchImpl: tf.fetch,
    rng: () => 0, // full jitter -> 0 delay: deterministic reconnect timing
    handlers: {
      onEvent: (e) => events.push(e.seq),
      onSnapshot: (r) => snapshots.push(r),
      onState: (s) => states.push(s),
      onError: (e) => errors.push(e),
    },
  });
  return { stream, transport, scheduler, ticketCalls: () => tf.calls, events, snapshots, states, errors };
}

/** Drive a fresh connection through snapshot → backfill(seqs) → caught_up. */
async function connectAndCatchUp(h: Harness, backfill: number[]): Promise<void> {
  await flush();
  const sock = h.transport.last();
  sock.open();
  sock.emit({ type: "snapshot", run: { id: RUN } });
  for (const s of backfill) sock.emit(eventFrame(s));
  sock.emit({ type: "caught_up", last_seq: backfill.at(-1) ?? 0 });
}

describe("RunStream", () => {
  let h: Harness;
  beforeEach(() => {
    h = makeStream();
  });

  it("mints a ticket, snapshots, backfills, and live-tails in order", async () => {
    h.stream.start();
    await connectAndCatchUp(h, [1, 2, 3]);
    // Live tail.
    h.transport.last().emit(eventFrame(4));
    h.transport.last().emit(eventFrame(5));

    expect(h.events).toEqual([1, 2, 3, 4, 5]);
    expect(h.snapshots).toHaveLength(1);
    expect(h.stream.cursor).toBe(5);
    expect(h.stream.state).toBe("live");
    // Ticket minted once, with the bearer key, to the run ws-ticket route.
    const calls = h.ticketCalls();
    expect(calls).toHaveLength(1);
    expect(calls[0]!.url).toContain(`/v1/runs/${RUN}/ws-ticket`);
    expect(calls[0]!.authorization).toBe("Bearer sk_test");
    // No last_seq on a fresh connect.
    expect(h.transport.last().url).not.toContain("last_seq");
  });

  it("dedupes events at or below the cursor", async () => {
    h.stream.start();
    await connectAndCatchUp(h, [1, 2]);
    const sock = h.transport.last();
    sock.emit(eventFrame(2)); // duplicate
    sock.emit(eventFrame(1)); // duplicate
    sock.emit(eventFrame(3)); // new
    expect(h.events).toEqual([1, 2, 3]);
    expect(h.stream.cursor).toBe(3);
  });

  it("resumes with last_seq and re-mints a ticket after a server close (no gaps/dupes)", async () => {
    h.stream.start();
    await connectAndCatchUp(h, [1, 2, 3]);
    h.transport.last().emit(eventFrame(4));

    // Server drops the connection mid-stream.
    h.transport.last().serverClose(1006, "abnormal");
    expect(h.stream.state).toBe("reconnecting");
    h.scheduler.advance(1); // fire the (0ms) reconnect
    await flush();

    // A second socket is opened, resuming from the cursor with a fresh ticket.
    expect(h.transport.count()).toBe(2);
    const sock2 = h.transport.last();
    expect(sock2.url).toContain("last_seq=4");
    expect(h.ticketCalls()).toHaveLength(2);

    // Reconnect: snapshot again, backfill re-delivers 4 (dropped as dup), then 5.
    sock2.open();
    sock2.emit({ type: "snapshot", run: { id: RUN } });
    sock2.emit(eventFrame(4)); // dup across the reconnect
    sock2.emit(eventFrame(5));
    sock2.emit({ type: "caught_up", last_seq: 5 });

    expect(h.events).toEqual([1, 2, 3, 4, 5]); // union, no dupes
    expect(h.snapshots).toHaveLength(2); // snapshot re-emitted on reconnect
    expect(h.stream.cursor).toBe(5);
  });

  it("fast-resumes immediately on a 4001 slow-consumer close", async () => {
    h.stream.start();
    await connectAndCatchUp(h, [1, 2]);
    h.transport.last().serverClose(4001, "slow consumer");
    h.scheduler.advance(0); // delay-0 reconnect
    await flush();
    expect(h.transport.count()).toBe(2);
    expect(h.transport.last().url).toContain("last_seq=2");
  });

  it("stops reconnecting after user close()", async () => {
    h.stream.start();
    await connectAndCatchUp(h, [1]);
    h.stream.close();
    expect(h.stream.state).toBe("closed");
    // A late server close must not schedule a reconnect.
    h.transport.last().serverClose(1006);
    h.scheduler.advance(10_000);
    await flush();
    expect(h.transport.count()).toBe(1);
  });

  it("backs off and retries when the ticket mint fails, then succeeds", async () => {
    // Fail the first mint, succeed the second.
    let n = 0;
    const fetch = (async () => {
      n++;
      if (n === 1) return { ok: false, status: 503, json: async () => ({}) } as unknown as Response;
      return { ok: true, status: 200, json: async () => ({ ticket: "t2" }) } as unknown as Response;
    }) as typeof globalThis.fetch;
    h = makeStream({ fetch: { fetch, calls: [] } });
    h.stream.start();
    await flush();
    // No socket yet — mint failed; an error surfaced and a reconnect is queued.
    expect(h.transport.count()).toBe(0);
    expect(h.errors.length).toBeGreaterThanOrEqual(1);
    expect(h.scheduler.pending()).toBe(1);
    // Fire the backoff retry; the second mint succeeds and a socket opens.
    h.scheduler.advance(1);
    await flush();
    expect(h.transport.count()).toBe(1);
    expect(h.transport.last().url).toContain("ticket=t2");
  });

  it("uses a caller-supplied ticket minter (browser proxy path) — no fetch", async () => {
    let minted = 0;
    const auth: AuthProvider = {
      mintTicket: async () => {
        minted++;
        return `proxy-ticket-${minted}`;
      },
    };
    h = makeStream({ auth });
    h.stream.start();
    await connectAndCatchUp(h, [1]);
    expect(minted).toBe(1);
    expect(h.transport.last().url).toContain("ticket=proxy-ticket-1");
    expect(h.ticketCalls()).toHaveLength(0); // fetch never called
  });

  it("closes on a terminal event when closeOnTerminal is set", async () => {
    h = makeStream({ closeOnTerminal: true });
    h.stream.start();
    await connectAndCatchUp(h, [1]);
    h.transport.last().emit(eventFrame(2, "run_succeeded"));
    expect(h.events).toEqual([1, 2]);
    expect(h.stream.state).toBe("closed");
  });

  it("ignores unparseable frames without closing", async () => {
    h.stream.start();
    await connectAndCatchUp(h, [1]);
    h.transport.last().emitRaw("not json");
    h.transport.last().emitRaw(JSON.stringify({ no: "type" }));
    h.transport.last().emit(eventFrame(2));
    expect(h.events).toEqual([1, 2]);
    expect(h.stream.state).toBe("live");
  });
});
