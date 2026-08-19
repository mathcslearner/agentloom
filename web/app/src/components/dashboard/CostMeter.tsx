"use client";

import type { RunView } from "@agentloom/api-client";
import { cn } from "@/lib/utils";
import { deriveCostMeter, formatUsd, type BudgetTier } from "@/lib/pure/dashboard/cost-meter";

const BAR_CLASS: Record<Exclude<BudgetTier, "unbudgeted">, string> = {
  ok: "bg-emerald-500",
  warn: "bg-amber-500",
  danger: "bg-orange-500",
  exceeded: "bg-red-500",
};

const TEXT_CLASS: Record<Exclude<BudgetTier, "unbudgeted">, string> = {
  ok: "text-emerald-600 dark:text-emerald-400",
  warn: "text-amber-600 dark:text-amber-400",
  danger: "text-orange-600 dark:text-orange-400",
  exceeded: "text-red-600 dark:text-red-400",
};

/**
 * The live run-header cost meter (ticket 18.4): a spend ticker, a saved-by-cache
 * indicator, and — on a budgeted run — a budget progress bar with threshold
 * colouring. Totals ride `cost_updated`/`run_budget_updated` (folded into
 * `run.cost` by the run-state reducer), so the meter is stateless and updates
 * live with no cost refetch.
 */
export function CostMeter({ run }: { run: RunView }) {
  const m = deriveCostMeter(run);
  const budgeted = m.budgetNanoUsd !== undefined && m.budgetNanoUsd > 0;
  const pct = m.fraction !== undefined ? Math.min(100, Math.round(m.fraction * 100)) : 0;
  const barClass = m.tier === "unbudgeted" ? "bg-muted-foreground/40" : BAR_CLASS[m.tier];
  const textClass = m.tier === "unbudgeted" ? "text-foreground" : TEXT_CLASS[m.tier];

  return (
    <div
      className="flex items-center gap-2"
      data-testid="cost-meter"
      data-tier={m.tier}
      data-spent-nano={m.spentNanoUsd}
      data-saved-nano={m.savedNanoUsd}
      data-budget-nano={m.budgetNanoUsd ?? ""}
    >
      <span className={cn("text-sm font-medium tabular-nums", textClass)} data-testid="cost-spent">
        {formatUsd(m.spentNanoUsd)}
        {budgeted ? (
          <span className="text-muted-foreground"> / {formatUsd(m.budgetNanoUsd!)}</span>
        ) : null}
      </span>

      {budgeted ? (
        <div
          className="h-1.5 w-24 overflow-hidden rounded-full bg-muted"
          role="progressbar"
          aria-label="budget used"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={pct}
          data-testid="budget-bar"
          data-pct={pct}
        >
          <div className={cn("h-full transition-[width] duration-300", barClass)} style={{ width: `${pct}%` }} />
        </div>
      ) : null}

      {m.parkedForBudget ? (
        <span className="text-xs font-medium text-amber-600 dark:text-amber-400" data-testid="cost-parked">
          parked at cap
        </span>
      ) : null}

      {m.savedNanoUsd > 0 ? (
        <span
          className="text-xs text-emerald-700 dark:text-emerald-400 tabular-nums"
          data-testid="cost-saved"
          title="Saved by response cache"
        >
          saved {formatUsd(m.savedNanoUsd)}
        </span>
      ) : null}
    </div>
  );
}
