/**
 * Pure cost-meter derivation for the run header (ticket 18.4).
 *
 * The live meter is stateless: the running totals ride the `cost_updated` event
 * (`run_spent_nano_usd`/`run_saved_nano_usd`, non-decreasing in seq order by
 * construction — 10.5), and the budget rides `cost_updated`/`run_budget_updated`.
 * `run-state.ts` folds those into `run.cost` under the seq guard, so this module
 * only projects that folded `CostSummaryView` onto the meter's display model —
 * no accumulation, no history. `foldCostEvents` is provided for tests to prove
 * the totals a run of `cost_updated` events converges to equal the run's
 * cost summary (the 18.4 consistency assertion, pre-paid at the reducer level).
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { EventEnvelope } from "@agentloom/engine-client";
import type { RunView } from "@agentloom/api-client";

/** Meter threshold tier, drives the progress-bar colour. */
export type BudgetTier = "unbudgeted" | "ok" | "warn" | "danger" | "exceeded";

export interface CostMeter {
  spentNanoUsd: number;
  savedNanoUsd: number;
  /** The run's spend cap, or undefined when unbudgeted. */
  budgetNanoUsd?: number;
  /** The configured over-budget disposition (`park` | `fail`). */
  onBudgetExceeded: "park" | "fail";
  /** spent / budget in [0, 1+]; undefined when unbudgeted. */
  fraction?: number;
  tier: BudgetTier;
  /** True when the run is parked specifically because it hit its budget. */
  parkedForBudget: boolean;
}

const WARN_AT = 0.75;
const DANGER_AT = 0.9;

/** Project the run's folded cost summary onto the meter display model. */
export function deriveCostMeter(run: RunView): CostMeter {
  const cost = run.cost;
  const spentNanoUsd = cost.spent_nano_usd;
  const savedNanoUsd = cost.saved_nano_usd;
  const budgetNanoUsd = cost.budget_nano_usd;
  const parkedForBudget = run.status === "parked" && run.park_reason === "budget_exceeded";

  if (budgetNanoUsd === undefined || budgetNanoUsd <= 0) {
    return {
      spentNanoUsd,
      savedNanoUsd,
      budgetNanoUsd,
      onBudgetExceeded: cost.on_budget_exceeded,
      tier: parkedForBudget ? "exceeded" : "unbudgeted",
      parkedForBudget,
    };
  }

  const fraction = spentNanoUsd / budgetNanoUsd;
  let tier: BudgetTier;
  if (parkedForBudget || fraction >= 1) tier = "exceeded";
  else if (fraction >= DANGER_AT) tier = "danger";
  else if (fraction >= WARN_AT) tier = "warn";
  else tier = "ok";

  return {
    spentNanoUsd,
    savedNanoUsd,
    budgetNanoUsd,
    onBudgetExceeded: cost.on_budget_exceeded,
    fraction,
    tier,
    parkedForBudget,
  };
}

/**
 * Fold a sequence of live events onto a running {spent, saved, budget},
 * mirroring what `run-state.applyEvent` does for the meter (used by tests to
 * prove convergence to the cost summary; not used by the app at runtime).
 */
export function foldCostEvents(
  events: readonly EventEnvelope[],
  seed: { spentNanoUsd: number; savedNanoUsd: number; budgetNanoUsd?: number } = {
    spentNanoUsd: 0,
    savedNanoUsd: 0,
  },
): { spentNanoUsd: number; savedNanoUsd: number; budgetNanoUsd?: number } {
  let { spentNanoUsd, savedNanoUsd, budgetNanoUsd } = seed;
  for (const env of events) {
    if (env.type === "cost_updated") {
      spentNanoUsd = env.payload.run_spent_nano_usd;
      savedNanoUsd = env.payload.run_saved_nano_usd;
      if (env.payload.budget_nano_usd !== undefined) budgetNanoUsd = env.payload.budget_nano_usd;
    } else if (env.type === "run_budget_updated") {
      budgetNanoUsd = env.payload.budget_nano_usd;
    }
  }
  return { spentNanoUsd, savedNanoUsd, budgetNanoUsd };
}

/** Render integer nano-USD as a trimmed USD string ("$0.0042", "$0"). */
export function formatUsd(nano: number): string {
  const usd = nano / 1e9;
  const s = usd.toFixed(6).replace(/0+$/, "").replace(/\.$/, "");
  return `$${s === "" || s === "-0" ? "0" : s}`;
}

/** Nano-USD → US dollars as a number (for prefilling the raise-budget input). */
export function nanoToUsd(nano: number): number {
  return nano / 1e9;
}
