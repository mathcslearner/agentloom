import { describe, expect, it } from "vitest";
import { nanoUsd, stepCost } from "@/lib/pure/dashboard/inspector-cost";
import { runCostFixture } from "./inspector-fixtures";

describe("stepCost (over the Go cost golden)", () => {
  it("filters entries to the step and splits overhead", () => {
    const c = stepCost(runCostFixture, "draft");
    // draft has attempt 1, attempt 2 productive, and a judge:0 overhead row.
    expect(c.rows).toHaveLength(3);
    expect(c.overheadNanoUsd).toBeGreaterThan(0);
    expect(c.rows.some((r) => r.overhead && r.entry === "judge:0")).toBe(true);
  });

  it("surfaces cache savings on a cache-hit row", () => {
    const c = stepCost(runCostFixture, "summarize");
    const hit = c.rows.find((r) => r.cacheHit);
    expect(hit).toBeDefined();
    expect(hit?.spentNanoUsd).toBe(0);
    expect(c.savedNanoUsd).toBeGreaterThan(0);
  });

  it("returns empty for a step with no cost", () => {
    const c = stepCost(runCostFixture, "crunch");
    expect(c.rows).toHaveLength(0);
  });

  it("returns empty when cost is undefined", () => {
    expect(stepCost(undefined, "draft").rows).toHaveLength(0);
  });

  it("nanoUsd trims trailing zeros", () => {
    expect(nanoUsd(1000000)).toBe("$0.001");
    expect(nanoUsd(0)).toBe("$0");
  });
});
