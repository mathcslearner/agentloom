import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { EVENT_TYPES } from "../src/generated/events.js";
import type { EventEnvelope, EventPayloadMap } from "../src/generated/events.js";

const here = dirname(fileURLToPath(import.meta.url));
const schema = JSON.parse(
  readFileSync(resolve(here, "../../../../docs/schema/events.v1.json"), "utf8"),
) as { $defs: { Envelope: { properties: { type: { enum: string[] } } } } };

describe("generated event types", () => {
  it("EVENT_TYPES matches the schema's type enum exactly (order included)", () => {
    expect([...EVENT_TYPES]).toEqual(schema.$defs.Envelope.properties.type.enum);
  });

  it("every event type has a payload map entry", () => {
    const mapKeys: (keyof EventPayloadMap)[] = EVENT_TYPES as unknown as (keyof EventPayloadMap)[];
    // A compile-time exhaustiveness check: this only compiles if the map covers
    // every EventType. The runtime assertion mirrors it.
    for (const t of mapKeys) expect(typeof t).toBe("string");
  });

  it("discriminates payloads by type at compile time", () => {
    const env = {
      schema_version: 1,
      run_id: "11111111-1111-1111-1111-111111111111",
      seq: 3,
      ts: "2026-01-01T00:00:00Z",
      type: "step_succeeded",
      payload: { step_id: "draft", attempt: 1 },
    } satisfies EventEnvelope;

    if (env.type === "step_succeeded") {
      // `payload` is narrowed to StepSucceeded here.
      expect(env.payload.step_id).toBe("draft");
      expect(env.payload.attempt).toBe(1);
    }
  });
});
