"use client";

import { useEffect, useState } from "react";
import type { RunView } from "@agentloom/api-client";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { deriveCostMeter, formatUsd, nanoToUsd } from "@/lib/pure/dashboard/cost-meter";
import type { RunController } from "@/lib/dashboard/run-controller";
import { useBudgetActions } from "@/lib/dashboard/useBudgetActions";

/**
 * Raise a run's spend budget (ticket 18.4). Prefills a suggested new cap, warns
 * (never blocks) when the entered budget would not clear the current spend
 * (the run would re-park immediately), and — when the run is parked at its cap
 * — offers to resume it (raise-then-unpark, ADR-012). The park→resume is
 * reflected live through the event feed, so the dialog just closes on success.
 */
export function RaiseBudgetDialog({
  runId,
  run,
  controller,
  open,
  onOpenChange,
}: {
  runId: string;
  run: RunView;
  controller: RunController;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const m = deriveCostMeter(run);
  const spentUsd = nanoToUsd(m.spentNanoUsd);
  const currentBudgetUsd = m.budgetNanoUsd !== undefined ? nanoToUsd(m.budgetNanoUsd) : undefined;
  const suggested = Math.max(currentBudgetUsd ? currentBudgetUsd * 2 : 0, spentUsd * 2, spentUsd + 0.01);

  const { pending, error, raiseBudget } = useBudgetActions(runId, controller);
  const [value, setValue] = useState<string>(() => suggested.toFixed(4));
  const [resume, setResume] = useState<boolean>(m.parkedForBudget);

  // Reset the form each time the dialog opens.
  useEffect(() => {
    if (open) {
      setValue(suggested.toFixed(4));
      setResume(m.parkedForBudget);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const parsed = Number(value);
  const valid = Number.isFinite(parsed) && parsed > 0;
  const belowSpend = valid && parsed <= spentUsd;

  async function onSubmit() {
    if (!valid) return;
    const ok = await raiseBudget({ budgetUsd: parsed, resume });
    if (ok) onOpenChange(false);
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-testid="raise-budget-dialog">
        <DialogHeader>
          <DialogTitle>Raise budget</DialogTitle>
          <DialogDescription>
            Spent {formatUsd(m.spentNanoUsd)}
            {m.budgetNanoUsd !== undefined ? ` of ${formatUsd(m.budgetNanoUsd)}` : ""}.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          <div className="space-y-1">
            <Label htmlFor="budget-usd">New budget (USD)</Label>
            <Input
              id="budget-usd"
              type="number"
              min="0"
              step="0.0001"
              inputMode="decimal"
              value={value}
              data-testid="budget-input"
              onChange={(e) => setValue(e.target.value)}
            />
            {belowSpend ? (
              <p className="text-xs text-amber-600 dark:text-amber-400" data-testid="budget-below-spend">
                Below the current spend — the run would park again immediately.
              </p>
            ) : null}
          </div>

          <label className="flex items-center gap-2 text-sm">
            <Checkbox
              checked={resume}
              data-testid="budget-resume"
              onChange={(e) => setResume(e.target.checked)}
            />
            Resume the run after raising
          </label>

          {error ? (
            <p className="text-sm text-destructive" data-testid="budget-error">
              {error}
            </p>
          ) : null}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={pending}>
            Cancel
          </Button>
          <Button onClick={onSubmit} disabled={!valid || pending} data-testid="budget-confirm">
            {pending ? "Raising…" : resume ? "Raise & resume" : "Raise budget"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
