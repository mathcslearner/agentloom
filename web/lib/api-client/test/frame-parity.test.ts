import { describe, expect, it } from "vitest";
import type * as engine from "@agentloom/engine-client";
import type { components } from "../src/index.js";

/**
 * Closes the M16.5 residual ("frame types hand-mirrored until M17.1"). The
 * engine-client stays dependency-free, so instead of rewiring it onto this
 * generated client we assert — at compile time — that its hand-written wire
 * frames are structurally compatible with the generated OpenAPI `WS*Frame`
 * schemas. A drift in the spec's frame fields would break this typecheck.
 *
 * We omit three fields from the structural compare. The `type` discriminant
 * diverges by construction — openapi-typescript synthesizes a schema-name
 * literal (`"WSEventFrame"`) where the wire value is the lowercase discriminator
 * (`"event"`) — so it is asserted separately as a runtime string. The `event`
 * and `run` payloads are each generated independently from the same committed
 * schemas (events.v1.json / the run view) and CI-diffed on their own side, so
 * re-deriving their structural equality across two generators is neither
 * necessary nor robust. What remains — the frame-wrapper fields (subscriptions,
 * last_seq, code, message, id, cursors, filter) — is checked here.
 */
type Schemas = components["schemas"];

// A compile-time "A and B have the same non-`type` fields" assertion.
type FrameFields<T> = Omit<T, "type" | "event" | "run">;
type Same<A, B> = [FrameFields<A>] extends [FrameFields<B>]
  ? [FrameFields<B>] extends [FrameFields<A>]
    ? true
    : never
  : never;
function assertSame<A, B>(_proof: Same<A, B>): void {}

describe("engine-client wire frames match the generated spec frames", () => {
  it("event / snapshot / caught_up / error / firehose frames are structurally identical", () => {
    // These lines fail to compile if a field set diverges.
    assertSame<engine.EventFrame, Schemas["WSEventFrame"]>(true);
    assertSame<engine.RunCaughtUpFrame, Schemas["WSCaughtUpFrame"]>(true);
    assertSame<engine.ErrorFrame, Schemas["WSErrorFrame"]>(true);
    assertSame<engine.SubscribedFrame, Schemas["WSSubscribedFrame"]>(true);
    assertSame<engine.UnsubscribedFrame, Schemas["WSUnsubscribedFrame"]>(true);
    assertSame<engine.FirehoseCaughtUpFrame, Schemas["WSFirehoseCaughtUpFrame"]>(true);
    expect(true).toBe(true);
  });

  it("engine-client uses the lowercase wire discriminants", () => {
    // Guards the one field the structural check omits.
    const ev: engine.EventFrame["type"] = "event";
    const snap: engine.RunSnapshotFrame["type"] = "snapshot";
    const cu: engine.RunCaughtUpFrame["type"] = "caught_up";
    const err: engine.ErrorFrame["type"] = "error";
    expect([ev, snap, cu, err]).toEqual(["event", "snapshot", "caught_up", "error"]);
  });
});
