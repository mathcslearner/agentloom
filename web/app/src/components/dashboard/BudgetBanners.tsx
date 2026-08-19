"use client";

import { useMemo, useState } from "react";
import { X } from "lucide-react";
import type { EventEnvelope } from "@agentloom/engine-client";
import type { RunView } from "@agentloom/api-client";
import { cn } from "@/lib/utils";
import { deriveBudgetBanners, type BannerSeverity, type BudgetBanner } from "@/lib/pure/dashboard/budget-banners";

const SEVERITY_CLASS: Record<BannerSeverity, string> = {
  info: "border-blue-500/40 bg-blue-500/10 text-blue-700 dark:text-blue-300",
  warn: "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300",
  error: "border-red-500/40 bg-red-500/10 text-red-700 dark:text-red-300",
  success: "border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
};

/**
 * The run header's budget/downgrade banner stack (ticket 18.4). Downgrades,
 * budget-exceeded parks/fails, and budget raises are surfaced from the event
 * feed; a live "parked at cap" affordance carries the Raise action. Each banner
 * is dismissible (a local set of dismissed keys); the parked affordance
 * reappears if the run parks again.
 */
export function BudgetBanners({
  events,
  run,
  onRaise,
}: {
  events: EventEnvelope[];
  run: RunView | undefined;
  onRaise: () => void;
}) {
  const [dismissed, setDismissed] = useState<Set<number>>(new Set());
  const banners = useMemo(() => deriveBudgetBanners(events, run), [events, run]);
  const visible = banners.filter((b) => !dismissed.has(b.key));
  if (visible.length === 0) return null;

  return (
    <div className="flex flex-col gap-1.5 px-6 pt-2" data-testid="budget-banners">
      {visible.map((b) => (
        <Banner key={b.key} banner={b} onRaise={onRaise} onDismiss={() => setDismissed((s) => new Set(s).add(b.key))} />
      ))}
    </div>
  );
}

function Banner({ banner: b, onRaise, onDismiss }: { banner: BudgetBanner; onRaise: () => void; onDismiss: () => void }) {
  return (
    <div
      className={cn("flex items-center gap-3 rounded-md border px-3 py-1.5 text-sm", SEVERITY_CLASS[b.severity])}
      data-testid={`banner-${b.kind}`}
      data-from={b.from ?? ""}
      data-to={b.to ?? ""}
      data-trigger={b.trigger ?? ""}
    >
      <div className="min-w-0 flex-1">
        <span className="font-medium">{b.title}</span>
        <span className="ml-2 text-xs opacity-90">{b.detail}</span>
      </div>
      {b.actionable ? (
        <button
          type="button"
          onClick={onRaise}
          data-testid="banner-raise"
          className="shrink-0 rounded border border-current px-2 py-0.5 text-xs font-medium hover:opacity-80"
        >
          Raise budget
        </button>
      ) : null}
      <button type="button" onClick={onDismiss} aria-label="Dismiss" className="shrink-0 opacity-60 hover:opacity-100">
        <X className="h-3.5 w-3.5" />
      </button>
    </div>
  );
}
