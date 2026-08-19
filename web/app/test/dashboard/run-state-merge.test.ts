import { describe, expect, it } from "vitest";
import { applyEvent, fromSnapshot, mergeRunResponse } from "@/lib/pure/dashboard/run-state";
import { makeEnv, makeRun, makeStep } from "./helpers";

describe("mergeRunResponse (ticket 18.3)", () => {
  it("replaces a step view from a fresher body but keeps live status", () => {
    // Snapshot at seq 0, then a live event moves step a to succeeded (seq 5).
    let state = fromSnapshot({ run: makeRun({ event_seq: 0 }), steps: [makeStep({ id: "a", status: "running" })] });
    state = applyEvent(state, makeEnv("step_succeeded", 5, { attempt: 1 }, "a"));
    expect(state.steps.get("a")?.status).toBe("succeeded");

    // A refetched body at event_seq 5 brings the full step view (with attempts).
    const body = {
      run: makeRun({ event_seq: 5 }),
      steps: [makeStep({ id: "a", status: "succeeded", attempt_count: 1, attempts: [{ attempt: 1, claim_id: "c1", outcome: "succeeded" }] })],
    };
    const merged = mergeRunResponse(state, body);
    // The view is now populated…
    expect(merged.steps.get("a")?.view?.attempts).toHaveLength(1);
    // …but the event-derived status is preserved (not regressed to the body's).
    expect(merged.steps.get("a")?.status).toBe("succeeded");
    expect(merged.steps.get("a")?.viewSeq).toBe(5);
  });

  it("ignores a stale body (older event_seq than the captured view)", () => {
    const state = fromSnapshot({ run: makeRun({ event_seq: 10 }), steps: [makeStep({ id: "a", status: "running" })] });
    const staleBody = { run: makeRun({ event_seq: 3 }), steps: [makeStep({ id: "a", status: "pending", type: "CHANGED" })] };
    const merged = mergeRunResponse(state, staleBody);
    // viewSeq 10 > body 3 ⇒ no replacement.
    expect(merged.steps.get("a")?.view?.type).not.toBe("CHANGED");
    expect(merged.steps.get("a")?.viewSeq).toBe(10);
  });

  it("folds the run-level cost/budget from a fresh body (18.4)", () => {
    const state = fromSnapshot({
      run: makeRun({ event_seq: 3, cost: { ...makeRun().cost, budget_nano_usd: 100 } }),
      steps: [makeStep({ id: "a" })],
    });
    const body = {
      run: makeRun({
        event_seq: 3,
        cost: { ...makeRun().cost, spent_nano_usd: 40, budget_nano_usd: 1000 },
      }),
      steps: [makeStep({ id: "a" })],
    };
    const merged = mergeRunResponse(state, body);
    expect(merged.run.cost.budget_nano_usd).toBe(1000);
    expect(merged.run.cost.spent_nano_usd).toBe(40);
  });

  it("a stale body never regresses the run-level projection (18.4)", () => {
    const state = fromSnapshot({
      run: makeRun({ event_seq: 10, cost: { ...makeRun().cost, spent_nano_usd: 90, budget_nano_usd: 1000 } }),
      steps: [makeStep({ id: "a" })],
    });
    const stale = {
      run: makeRun({ event_seq: 4, cost: { ...makeRun().cost, spent_nano_usd: 5, budget_nano_usd: 100 } }),
      steps: [makeStep({ id: "a" })],
    };
    const merged = mergeRunResponse(state, stale);
    expect(merged.run.cost.spent_nano_usd).toBe(90);
    expect(merged.run.cost.budget_nano_usd).toBe(1000);
  });

  it("fills a view for an injected step that had none", () => {
    let state = fromSnapshot({ run: makeRun({ event_seq: 0 }), steps: [] });
    state = applyEvent(
      state,
      makeEnv("graph_expanded", 4, {
        origin_step: "plan",
        origin_kind: "planner",
        readied: ["work#1"],
        delta: { steps: [{ id: "work#1", type: "llm" }], edges: [] },
      }),
    );
    expect(state.steps.get("work#1")?.view).toBeUndefined();
    const body = { run: makeRun({ event_seq: 4 }), steps: [makeStep({ id: "work#1", type: "llm", status: "ready" })] };
    const merged = mergeRunResponse(state, body);
    expect(merged.steps.get("work#1")?.view).toBeDefined();
    // The origin provenance from the event is preserved.
    expect(merged.steps.get("work#1")?.origin?.kind).toBe("planner");
  });
});
