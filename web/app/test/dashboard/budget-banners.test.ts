import { describe, expect, it } from "vitest";
import { deriveBudgetBanners, PARKED_BANNER_KEY } from "@/lib/pure/dashboard/budget-banners";
import { makeEnv, makeRun } from "./helpers";

describe("deriveBudgetBanners", () => {
  it("derives a downgrade banner naming from/to models and the trigger", () => {
    const banners = deriveBudgetBanners(
      [
        makeEnv("model_downgraded", 3, {
          step_id: "draft",
          attempt: 1,
          from_model: "mock/sim-1",
          to_model: "mock/cheap",
          from_resource: "mock:sim-1",
          to_resource: "mock:cheap",
          trigger: "budget_threshold",
          threshold_fraction: 0.5,
        }),
      ],
      makeRun(),
    );
    const d = banners.find((b) => b.kind === "downgrade")!;
    expect(d).toBeTruthy();
    expect(d.from).toBe("mock/sim-1");
    expect(d.to).toBe("mock/cheap");
    expect(d.trigger).toBe("budget_threshold");
    expect(d.detail).toContain("budget threshold 50%");
  });

  it("a projection-triggered downgrade names the limit", () => {
    const [b] = deriveBudgetBanners(
      [
        makeEnv("model_downgraded", 1, {
          step_id: "s",
          attempt: 1,
          from_model: "a",
          to_model: "b",
          trigger: "budget_projection",
          limit: "run",
        }),
      ],
      makeRun(),
    );
    expect(b!.detail).toContain("budget projection (run)");
  });

  it("budget_exceeded park vs fail", () => {
    const parked = deriveBudgetBanners(
      [makeEnv("budget_exceeded", 1, { step_id: "s", attempt: 1, limit: "run", action: "park" })],
      makeRun(),
    );
    expect(parked[0]!.kind).toBe("budget_exceeded");
    const failed = deriveBudgetBanners(
      [makeEnv("budget_exceeded", 1, { step_id: "s", attempt: 1, limit: "run", action: "fail" })],
      makeRun(),
    );
    expect(failed[0]!.kind).toBe("budget_failed");
    expect(failed[0]!.severity).toBe("error");
  });

  it("a budget raise banner shows previous → new", () => {
    const [b] = deriveBudgetBanners(
      [makeEnv("run_budget_updated", 1, { previous_nano_usd: 100_000_000, budget_nano_usd: 1_000_000_000 })],
      makeRun(),
    );
    expect(b!.kind).toBe("budget_raised");
    expect(b!.detail).toBe("$0.1 → $1");
  });

  it("the parked-for-budget affordance is present only while parked for budget, pinned first, and actionable", () => {
    const parkedRun = makeRun({
      status: "parked",
      park_reason: "budget_exceeded",
      cost: { ...makeRun().cost, spent_nano_usd: 50, budget_nano_usd: 100 },
    });
    const banners = deriveBudgetBanners(
      [makeEnv("budget_exceeded", 1, { step_id: "s", attempt: 1, limit: "run", action: "park" })],
      parkedRun,
    );
    expect(banners[0]!.kind).toBe("parked_for_budget");
    expect(banners[0]!.key).toBe(PARKED_BANNER_KEY);
    expect(banners[0]!.actionable).toBe(true);

    // Once unparked (running), the affordance is gone.
    const running = deriveBudgetBanners(
      [makeEnv("budget_exceeded", 1, { step_id: "s", attempt: 1, limit: "run", action: "park" })],
      makeRun({ status: "running" }),
    );
    expect(running.find((b) => b.kind === "parked_for_budget")).toBeUndefined();
  });

  it("banners are keyed by event seq (dedupe across a re-backfill) and newest-first", () => {
    const events = [
      makeEnv("model_downgraded", 2, { step_id: "s", attempt: 1, from_model: "a", to_model: "b", trigger: "x" }),
      makeEnv("run_budget_updated", 5, { previous_nano_usd: 1, budget_nano_usd: 2 }),
    ];
    const banners = deriveBudgetBanners(events, makeRun());
    expect(banners.map((b) => b.key)).toEqual([5, 2]); // newest-first
  });
});
