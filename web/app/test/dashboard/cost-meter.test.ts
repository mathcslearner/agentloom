import { describe, expect, it } from "vitest";
import {
  deriveCostMeter,
  foldCostEvents,
  formatUsd,
  nanoToUsd,
} from "@/lib/pure/dashboard/cost-meter";
import { runCostFixture } from "./inspector-fixtures";
import { makeEnv, makeRun } from "./helpers";

describe("formatUsd", () => {
  it("trims trailing zeros and renders $0", () => {
    expect(formatUsd(0)).toBe("$0");
    expect(formatUsd(4_200_000)).toBe("$0.0042");
    expect(formatUsd(1_000_000_000)).toBe("$1");
    expect(nanoToUsd(1_500_000_000)).toBe(1.5);
  });
});

describe("deriveCostMeter", () => {
  it("reports unbudgeted when there is no budget", () => {
    const m = deriveCostMeter(makeRun({ cost: { ...makeRun().cost, spent_nano_usd: 5 } }));
    expect(m.tier).toBe("unbudgeted");
    expect(m.fraction).toBeUndefined();
    expect(m.budgetNanoUsd).toBeUndefined();
  });

  it("tiers ok/warn/danger/exceeded by fraction of budget", () => {
    const at = (spent: number, budget = 100) =>
      deriveCostMeter(
        makeRun({ cost: { ...makeRun().cost, spent_nano_usd: spent, budget_nano_usd: budget } }),
      ).tier;
    expect(at(10)).toBe("ok");
    expect(at(74)).toBe("ok");
    expect(at(75)).toBe("warn");
    expect(at(89)).toBe("warn");
    expect(at(90)).toBe("danger");
    expect(at(99)).toBe("danger");
    expect(at(100)).toBe("exceeded");
    expect(at(120)).toBe("exceeded");
  });

  it("marks exceeded and parkedForBudget when the run parked at its cap", () => {
    const m = deriveCostMeter(
      makeRun({
        status: "parked",
        park_reason: "budget_exceeded",
        cost: { ...makeRun().cost, spent_nano_usd: 50, budget_nano_usd: 100 },
      }),
    );
    expect(m.parkedForBudget).toBe(true);
    expect(m.tier).toBe("exceeded");
  });

  it("a manual park is not a budget park", () => {
    const m = deriveCostMeter(
      makeRun({ status: "parked", park_reason: "manual", cost: { ...makeRun().cost, budget_nano_usd: 100 } }),
    );
    expect(m.parkedForBudget).toBe(false);
    expect(m.tier).toBe("ok");
  });
});

describe("foldCostEvents (consistency: converges to the cost summary)", () => {
  it("the folded totals equal a cost_updated's running totals", () => {
    const events = [
      makeEnv("cost_updated", 1, {
        run_spent_nano_usd: 1000,
        run_saved_nano_usd: 100,
        budget_nano_usd: 100_000_000,
      }),
      makeEnv("cost_updated", 2, {
        run_spent_nano_usd: 4_200_000,
        run_saved_nano_usd: 300_000,
        budget_nano_usd: 100_000_000,
      }),
    ];
    const folded = foldCostEvents(events);
    // The last cost_updated's totals match the golden cost summary — the 18.4
    // consistency assertion (the meter reads live off the stream; the summary is
    // the same running total).
    expect(folded.spentNanoUsd).toBe(runCostFixture.summary.spent_nano_usd);
    expect(folded.savedNanoUsd).toBe(runCostFixture.summary.saved_nano_usd);
    expect(folded.budgetNanoUsd).toBe(runCostFixture.summary.budget_nano_usd);
  });

  it("run_budget_updated raises the tracked budget", () => {
    const folded = foldCostEvents([
      makeEnv("cost_updated", 1, { run_spent_nano_usd: 50, run_saved_nano_usd: 0, budget_nano_usd: 100 }),
      makeEnv("run_budget_updated", 2, { previous_nano_usd: 100, budget_nano_usd: 500 }),
    ]);
    expect(folded.budgetNanoUsd).toBe(500);
    expect(folded.spentNanoUsd).toBe(50);
  });
});
