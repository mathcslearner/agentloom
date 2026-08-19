import { describe, expect, it } from "vitest";
import { applyEvent, fromSnapshot } from "@/lib/pure/dashboard/run-state";
import { makeEnv, makeRun, makeStep } from "./helpers";

function seed(eventSeq = 0) {
  return fromSnapshot({
    run: makeRun({ event_seq: eventSeq }),
    steps: [makeStep({ id: "a", status: "running", attempt_count: 1 }), makeStep({ id: "b" })],
  });
}

describe("fromSnapshot", () => {
  it("seeds the step map and as-of cursor from the run view", () => {
    const s = seed(7);
    expect(s.asOf).toBe(7);
    expect(s.steps.get("a")?.status).toBe("running");
    expect(s.steps.size).toBe(2);
  });
});

describe("applyEvent", () => {
  it("ignores an event at or below the as-of cursor (idempotent)", () => {
    const s = seed(5);
    const same = applyEvent(s, makeEnv("step_succeeded", 5, { attempt: 1 }, "a"));
    expect(same).toBe(s); // identity — no re-render
    expect(same.steps.get("a")?.status).toBe("running");
  });

  it("advances step status and recomputes counters from the map", () => {
    let s = seed(0);
    s = applyEvent(s, makeEnv("step_succeeded", 1, { attempt: 1 }, "a"));
    expect(s.asOf).toBe(1);
    expect(s.steps.get("a")?.status).toBe("succeeded");
    expect(s.run.steps_succeeded).toBe(1);
    // Re-applying the same succeed (a reconnect suffix) is idempotent.
    const before = s.run.steps_succeeded;
    s = applyEvent(s, makeEnv("step_succeeded", 1, { attempt: 1 }, "a"));
    expect(s.run.steps_succeeded).toBe(before);
  });

  it("maps run lifecycle events onto run status + park reason", () => {
    let s = seed(0);
    s = applyEvent(s, makeEnv("run_parked", 1, { reason: "budget_exceeded" }));
    expect(s.run.status).toBe("parked");
    expect(s.run.park_reason).toBe("budget_exceeded");
    s = applyEvent(s, makeEnv("run_unparked", 2, {}));
    expect(s.run.status).toBe("running");
    expect(s.run.park_reason).toBeUndefined();
  });

  it("adds injected steps from a graph_expanded delta with provenance", () => {
    let s = seed(0);
    s = applyEvent(
      s,
      makeEnv("graph_expanded", 1, {
        origin_step: "plan",
        origin_kind: "planner",
        to_version: 2,
        readied: ["work_a"],
        delta: {
          schema_version: 1,
          steps: [
            { id: "work_a", type: "llm" },
            { id: "work_b", type: "llm" },
          ],
        },
      }),
    );
    expect(s.steps.get("work_a")?.status).toBe("ready");
    expect(s.steps.get("work_b")?.status).toBe("pending");
    expect(s.steps.get("work_a")?.origin).toEqual({ step: "plan", kind: "planner" });
    expect(s.run.steps_total).toBeGreaterThanOrEqual(4);
  });

  it("updates cost totals from cost_updated", () => {
    let s = seed(0);
    s = applyEvent(
      s,
      makeEnv("cost_updated", 1, { run_spent_nano_usd: 12345, run_saved_nano_usd: 6 }, "a"),
    );
    expect(s.run.cost.spent_nano_usd).toBe(12345);
    expect(s.run.cost.saved_nano_usd).toBe(6);
  });
});
