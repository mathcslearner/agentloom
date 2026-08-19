import { describe, expect, it } from "vitest";
import { applyEvent, fromSnapshot, type DisplayStepStatus, type RunState } from "@/lib/pure/dashboard/run-state";
import { edgeVisual, skinFor } from "@/lib/pure/dashboard/skins";
import type { TopoEdge } from "@/lib/pure/dashboard/graph-topology";
import { makeEnv, makeRun, makeStep } from "./helpers";

// DoD-1: "every status skin driven by live events". A scripted event sequence
// walks one step through the full lifecycle, and skinFor produces a distinct,
// correct skin at every state. The switch in skins.ts is exhaustive over
// DisplayStepStatus, so a new backend status fails typecheck until skinned.

function seed(): RunState {
  return fromSnapshot({
    run: makeRun({ event_seq: 0, steps_total: 1 }),
    steps: [makeStep({ id: "s", type: "llm", status: "pending", attempt_count: 0 })],
  });
}

describe("skinFor — status walk", () => {
  it("visits pending → ready → running → retrying → running → throttled → running → succeeded", () => {
    let s = seed();
    const skin0 = skinFor(s.steps.get("s")!);
    expect(skin0.tone).toBe("neutral");
    expect(skin0.pulse).toBe(false);

    const walk: Array<[Parameters<typeof makeEnv>[0], number, Record<string, unknown>, DisplayStepStatus, string]> = [
      ["step_ready", 1, {}, "ready", "ready"],
      ["step_claimed", 2, { attempt: 1 }, "running", "active"],
      ["step_retry_scheduled", 3, { attempt: 1 }, "retrying", "warn"],
      ["step_claimed", 4, { attempt: 2 }, "running", "active"],
      ["step_throttled", 5, { attempt: 2 }, "throttled", "warn"],
      ["step_claimed", 6, { attempt: 2 }, "running", "active"],
      ["step_succeeded", 7, { attempt: 2 }, "succeeded", "success"],
    ];
    for (const [type, seq, payload, wantStatus, wantTone] of walk) {
      s = applyEvent(s, makeEnv(type, seq, payload, "s"));
      const step = s.steps.get("s")!;
      expect(step.status, `${type}`).toBe(wantStatus);
      expect(skinFor(step).tone, `${type} tone`).toBe(wantTone);
    }
    const final = skinFor(s.steps.get("s")!);
    expect(final.summary).toContain("attempt 2");
    expect(final.pulse).toBe(false);
  });

  it("running pulses and reports the attempt", () => {
    let s = seed();
    s = applyEvent(s, makeEnv("step_claimed", 1, { attempt: 2 }, "s"));
    const skin = skinFor(s.steps.get("s")!);
    expect(skin.pulse).toBe(true);
    expect(skin.summary).toBe("attempt 2");
  });

  it("a reclaimed step surfaces the reclaim marker", () => {
    let s = seed();
    s = applyEvent(s, makeEnv("step_claimed", 1, { attempt: 1 }, "s"));
    s = applyEvent(s, makeEnv("step_reclaimed", 2, { attempt: 2 }, "s"));
    const step = s.steps.get("s")!;
    expect(step.reclaims).toBe(1);
    expect(skinFor(step).marker).toBe("↻ reclaimed ×1");
  });

  it("covers the terminal / branch statuses", () => {
    const cases: Array<[DisplayStepStatus, string]> = [
      ["failed", "danger"],
      ["dead_lettered", "danger"],
      ["cancelled", "muted"],
      ["skipped", "muted"],
      ["collected", "muted"],
      ["awaiting_human", "attention"],
    ];
    for (const [status, tone] of cases) {
      const step = { id: "s", type: "llm", status, attempt: 1, reclaims: 0 };
      expect(skinFor(step).tone, status).toBe(tone);
      expect(skinFor(step).badge, status).toBeTruthy();
    }
  });
});

describe("edgeVisual", () => {
  const edge: TopoEdge = { from: "a", to: "b", type: "normal", resolution: "unresolved", graphVersion: 1, origin: { kind: "definition" } };

  it("marks an active hand-off when the source completed and the target runs", () => {
    const src = { id: "a", type: "llm", status: "succeeded" as const, attempt: 1, reclaims: 0 };
    const dst = { id: "b", type: "llm", status: "running" as const, attempt: 1, reclaims: 0 };
    const v = edgeVisual(edge, src, dst);
    expect(v.active).toBe(true);
    expect(v.resolution).toBe("fired");
  });

  it("derives skipped from a skipped target", () => {
    const src = { id: "a", type: "llm", status: "succeeded" as const, attempt: 1, reclaims: 0 };
    const dst = { id: "b", type: "llm", status: "skipped" as const, attempt: 0, reclaims: 0 };
    expect(edgeVisual(edge, src, dst).resolution).toBe("skipped");
  });

  it("honors a stored fired resolution", () => {
    const fired: TopoEdge = { ...edge, resolution: "fired" };
    expect(edgeVisual(fired).resolution).toBe("fired");
  });
});
