import { describe, expect, it } from "vitest";
import { applyEvent, fromSnapshot, mergeRunResponse, type RunBody } from "@/lib/pure/dashboard/run-state";
import { runDetailFixture } from "./inspector-fixtures";
import { makeEnv } from "./helpers";

// The 18.3 run-detail golden already carries a dead_letters array (step `boom`).
const body = runDetailFixture as RunBody;

describe("run-state dead letters", () => {
  it("folds the body's dead_letters into a step-keyed map", () => {
    const state = fromSnapshot(body);
    expect(state.deadLetters.size).toBeGreaterThanOrEqual(1);
    const boom = state.deadLetters.get("boom");
    expect(boom).toBeDefined();
    expect(boom![0]!.source).toBeDefined();
  });

  it("preserves deadLetters across a live event", () => {
    const state = fromSnapshot(body);
    const next = applyEvent(state, makeEnv("step_succeeded", state.asOf + 1, { step_id: "boom" }, "boom"));
    expect(next.deadLetters).toBe(state.deadLetters);
  });

  it("adopts a refetched body's dead_letters", () => {
    const state = fromSnapshot({ ...body, dead_letters: [] });
    expect(state.deadLetters.size).toBe(0);
    const fresher: RunBody = {
      ...body,
      run: { ...body.run, event_seq: body.run.event_seq + 5 },
    };
    const merged = mergeRunResponse(state, fresher);
    expect(merged.deadLetters.get("boom")).toBeDefined();
  });
});
