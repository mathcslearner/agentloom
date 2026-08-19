"use client";

/**
 * Budget actions for the run header (ticket 18.4). Raising a run's budget is a
 * PATCH; resuming a run parked at its cap is the documented raise-then-unpark
 * sequence (ADR-012). The park→resume is reflected live through the event feed
 * (`run_budget_updated` + `run_unparked`), so this hook does not hold optimistic
 * state — it drives the two requests, tracks pending/error, and asks the
 * controller to reconcile the detail body as belt-and-braces.
 */
import { useCallback, useState } from "react";
import type { RunController } from "@/lib/dashboard/run-controller";
import { setRunBudget, unparkRun, type BudgetActionOutcome } from "@/lib/dashboard/streams";

export interface RaiseBudgetArgs {
  budgetUsd: number;
  /** Unpark the run after the raise (the resume half). */
  resume: boolean;
}

export interface BudgetActionsState {
  pending: boolean;
  error?: string;
  raiseBudget: (args: RaiseBudgetArgs) => Promise<boolean>;
}

function message(o: BudgetActionOutcome): string {
  return o.kind === "ok" ? "" : o.message;
}

export function useBudgetActions(runId: string, controller: RunController): BudgetActionsState {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string>();

  const raiseBudget = useCallback(
    async ({ budgetUsd, resume }: RaiseBudgetArgs): Promise<boolean> => {
      setPending(true);
      setError(undefined);
      try {
        const raised = await setRunBudget(runId, budgetUsd);
        if (raised.kind !== "ok") {
          setError(message(raised));
          return false;
        }
        if (resume) {
          const unparked = await unparkRun(runId);
          // A run that already resumed (a concurrent decision, or it was not
          // parked) returns a 409 — the budget raise still succeeded, so treat
          // it as a soft outcome rather than a hard failure.
          if (unparked.kind !== "ok" && unparked.kind !== "conflict") {
            setError(message(unparked));
            void controller.refreshViews();
            return false;
          }
        }
        // Reconcile the detail body; the live feed is the real reflection.
        void controller.refreshViews();
        return true;
      } finally {
        setPending(false);
      }
    },
    [runId, controller],
  );

  return { pending, error, raiseBudget };
}
