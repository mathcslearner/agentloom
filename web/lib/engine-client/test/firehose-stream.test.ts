import { beforeEach, describe, expect, it } from "vitest";

import type { EventEnvelope, EventType } from "../src/generated/events.js";
import { FirehoseStream } from "../src/firehose-stream.js";
import type { EventFilter, SubscribeMessage } from "../src/frames.js";
import { FakeScheduler, FakeTransport, fakeTicketFetch, flush } from "./helpers.js";

function eventFrame(runId: string, seq: number, subs: string[], type: EventType = "step_ready"): object {
  const env: EventEnvelope = {
    schema_version: 1,
    run_id: runId,
    seq,
    ts: "2026-01-01T00:00:00Z",
    type,
    payload: { step_id: `s${seq}` } as EventEnvelope["payload"],
  } as EventEnvelope;
  return { type: "event", event: env, subscriptions: subs };
}

interface Harness {
  fh: FirehoseStream;
  transport: FakeTransport;
  scheduler: FakeScheduler;
  ticketCalls: () => { url: string }[];
  events: { runId: string; seq: number; subs: string[] }[];
  subscribed: { id: string; filter: EventFilter }[];
  errors: Error[];
}

function make(): Harness {
  const transport = new FakeTransport();
  const scheduler = new FakeScheduler();
  const tf = fakeTicketFetch();
  const events: Harness["events"] = [];
  const subscribed: Harness["subscribed"] = [];
  const errors: Error[] = [];
  const fh = new FirehoseStream({
    baseUrl: "http://api.test",
    auth: { apiKey: "sk_test" },
    scheduler,
    webSocketFactory: transport.factory,
    fetchImpl: tf.fetch,
    rng: () => 0,
    handlers: {
      onEvent: (e, subs) => events.push({ runId: e.run_id, seq: e.seq, subs }),
      onSubscribed: (id, filter) => subscribed.push({ id, filter }),
      onError: (e) => errors.push(e),
    },
  });
  return { fh, transport, scheduler, ticketCalls: () => tf.calls, events, subscribed, errors };
}

const R1 = "11111111-1111-1111-1111-111111111111";
const R2 = "22222222-2222-2222-2222-222222222222";

describe("FirehoseStream", () => {
  let h: Harness;
  beforeEach(() => {
    h = make();
  });

  it("mints a firehose ticket and sends queued subscriptions on open", async () => {
    h.fh.subscribe("list", { types: ["run_created"] });
    h.fh.start();
    await flush();
    const sock = h.transport.last();
    sock.open();
    // Subscription sent on open.
    const sub = sock.sentMessages()[0] as SubscribeMessage;
    expect(sub.type).toBe("subscribe");
    expect(sub.id).toBe("list");
    expect(sub.filter).toEqual({ types: ["run_created"] });
    expect(h.ticketCalls()[0]!.url).toContain("/v1/events/ws-ticket");

    sock.emit({ type: "subscribed", id: "list", filter: { types: ["run_created"] } });
    expect(h.subscribed).toEqual([{ id: "list", filter: { types: ["run_created"] } }]);
  });

  it("delivers filtered events tagged with matched subscriptions, deduped per run", async () => {
    h.fh.subscribe("a", {});
    h.fh.start();
    await flush();
    const sock = h.transport.last();
    sock.open();
    sock.emit(eventFrame(R1, 1, ["a"]));
    sock.emit(eventFrame(R2, 1, ["a"]));
    sock.emit(eventFrame(R1, 2, ["a"]));
    sock.emit(eventFrame(R1, 2, ["a"])); // duplicate for R1
    sock.emit(eventFrame(R1, 1, ["a"])); // duplicate for R1

    expect(h.events).toEqual([
      { runId: R1, seq: 1, subs: ["a"] },
      { runId: R2, seq: 1, subs: ["a"] },
      { runId: R1, seq: 2, subs: ["a"] },
    ]);
  });

  it("re-subscribes with tracked cursors after a reconnect", async () => {
    h.fh.subscribe("a", { types: ["step_ready"] });
    h.fh.start();
    await flush();
    let sock = h.transport.last();
    sock.open();
    sock.emit(eventFrame(R1, 3, ["a"]));
    sock.emit(eventFrame(R2, 5, ["a"]));

    // Drop the connection.
    sock.serverClose(1006);
    h.scheduler.advance(1);
    await flush();
    expect(h.transport.count()).toBe(2);
    sock = h.transport.last();
    sock.open();
    // Re-subscribe carries the tracked high-water cursors.
    const resub = sock.sentMessages()[0] as SubscribeMessage;
    expect(resub.id).toBe("a");
    expect(resub.cursors).toEqual({ [R1]: 3, [R2]: 5 });
    // Re-delivered backfill events (<= cursor) are deduped.
    sock.emit(eventFrame(R1, 3, ["a"]));
    sock.emit(eventFrame(R1, 4, ["a"]));
    expect(h.events.map((e) => `${e.runId}:${e.seq}`)).toEqual([
      `${R1}:3`,
      `${R2}:5`,
      `${R1}:4`,
    ]);
  });

  it("surfaces an in-band error frame without closing", async () => {
    h.fh.start();
    await flush();
    const sock = h.transport.last();
    sock.open();
    sock.emit({ type: "error", code: "filter_invalid", message: "bad filter", id: "x" });
    expect(h.errors).toHaveLength(1);
    expect(h.errors[0]!.message).toContain("filter_invalid");
    expect(h.fh.state).toBe("live"); // still open
  });

  it("unsubscribe removes a subscription and stops re-sending it", async () => {
    h.fh.subscribe("a", {});
    h.fh.subscribe("b", {});
    h.fh.start();
    await flush();
    let sock = h.transport.last();
    sock.open();
    h.fh.unsubscribe("a");
    const msgs = sock.sentMessages();
    expect(msgs.some((m) => (m as { type: string }).type === "unsubscribe")).toBe(true);

    // On reconnect only "b" is re-sent.
    sock.serverClose(1006);
    h.scheduler.advance(1);
    await flush();
    sock = h.transport.last();
    sock.open();
    const resubIds = sock.sentMessages().map((m) => (m as SubscribeMessage).id);
    expect(resubIds).toEqual(["b"]);
  });

  it("bounds the resume cursor map, preferring non-terminal runs", async () => {
    const transport = new FakeTransport();
    const scheduler = new FakeScheduler();
    const tf = fakeTicketFetch();
    const fh = new FirehoseStream({
      baseUrl: "http://api.test",
      auth: { apiKey: "k" },
      scheduler,
      webSocketFactory: transport.factory,
      fetchImpl: tf.fetch,
      rng: () => 0,
      maxCursorRuns: 1,
    });
    fh.subscribe("a", {});
    fh.start();
    await flush();
    let sock = transport.last();
    sock.open();
    // R1 terminates; R2 stays live.
    sock.emit(eventFrame(R1, 1, ["a"], "run_succeeded"));
    sock.emit(eventFrame(R2, 1, ["a"]));

    sock.serverClose(1006);
    scheduler.advance(1);
    await flush();
    sock = transport.last();
    sock.open();
    const resub = sock.sentMessages()[0] as SubscribeMessage;
    // Only one cursor fits; the non-terminal run wins.
    expect(resub.cursors).toEqual({ [R2]: 1 });
  });
});
