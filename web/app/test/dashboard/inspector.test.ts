import { describe, expect, it } from "vitest";
import {
  attemptTimeline,
  claimHistory,
  effectivePrompts,
  hasPromptDiff,
  hasValidation,
  modelHistory,
  promptDiff,
  verdictRows,
  workerIds,
} from "@/lib/pure/dashboard/inspector";
import { diffStats } from "@/lib/pure/dashboard/diff";
import { fixtureStep, runDetailFixture } from "./inspector-fixtures";
import { makeEnv } from "./helpers";

describe("inspector derivations (over the Go golden fixture)", () => {
  it("attemptTimeline marks the reclaimed (lost) attempt", () => {
    const rows = attemptTimeline(fixtureStep("crunch"));
    expect(rows).toHaveLength(2);
    expect(rows[0]?.reclaimed).toBe(true);
    expect(rows[1]?.reclaimed).toBe(false);
    expect(rows[0]?.durationMs).toBeGreaterThan(0);
  });

  it("claimHistory + workerIds show both workers of a reclaim (DoD-3)", () => {
    const step = fixtureStep("crunch");
    const ids = workerIds(step, []);
    expect(ids).toContain("worker-alpha");
    expect(ids).toContain("worker-bravo");
    const history = claimHistory(step, []);
    expect(history.find((r) => r.displaced)?.workerId).toBe("worker-alpha");
  });

  it("claimHistory folds a live claim event not yet in the attempts", () => {
    const step = fixtureStep("draft"); // attempts 1,2
    const ev = makeEnv("step_reclaimed", 99, { attempt: 3, worker_id: "worker-live", claim_id: "c3" }, "draft");
    const rows = claimHistory(step, [ev]);
    expect(rows.some((r) => r.source === "event" && r.workerId === "worker-live")).toBe(true);
  });

  it("modelHistory names authored + downgrade from the event feed", () => {
    const step = fixtureStep("summarize");
    const ev = makeEnv(
      "model_downgraded",
      50,
      { from_model: "mock/expensive", to_model: "mock/cheap", from_resource: "mock:expensive", to_resource: "mock:cheap", trigger: "budget_threshold", attempt: 1 },
      "summarize",
    );
    const m = modelHistory(step, [ev]);
    expect(m.authored).toBe("mock/expensive");
    expect(m.served).toBe("mock/cheap");
    expect(m.downgrades[0]).toMatchObject({ toModel: "mock/cheap", trigger: "budget_threshold" });
  });

  it("effectivePrompts appends feedback; promptDiff is pure additions (DoD-2)", () => {
    const step = fixtureStep("draft");
    const prompts = effectivePrompts(step);
    expect(prompts).toHaveLength(2);
    // Attempt 1 has no feedback; attempt 2 appends it.
    expect(prompts[0]?.feedbackText).toBeUndefined();
    expect(prompts[1]?.text).toContain("attempt 2 of 3");
    expect(hasPromptDiff(step)).toBe(true);
    const stats = diffStats(promptDiff(step, 1, 2));
    expect(stats.del).toBe(0);
    expect(stats.add).toBeGreaterThan(0);
  });

  it("verdictRows + hasValidation reflect the semantic-retry chain", () => {
    const step = fixtureStep("draft");
    expect(hasValidation(step)).toBe(true);
    const rows = verdictRows(step);
    expect(rows.map((r) => r.status)).toEqual(["fail", "pass"]);
    expect(rows[0]?.issues[0]?.validator).toBe("contains");
  });

  it("a non-validated step has no validation and no prompt diff", () => {
    const step = fixtureStep("crunch");
    expect(hasValidation(step)).toBe(false);
    expect(hasPromptDiff(step)).toBe(false);
  });

  it("every step carries an idempotency key", () => {
    for (const s of runDetailFixture.steps ?? []) {
      expect(s.idempotency_key).toMatch(/^[0-9a-f-]{36}$/);
    }
  });
});
