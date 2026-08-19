"use client";

/**
 * Run-control actions for the run header (ticket 18.6): cancel, park, unpark,
 * and dead-lettered-step requeue. The state change is reflected live through
 * the event feed (`run_cancelling`/`run_parked`/`run_unparked`/`step_requeued`),
 * so this hook holds no optimistic state — it drives the request, tracks
 * pending/error, and asks the controller to reconcile the detail body (which
 * also pulls in `cancel_reason`, absent from the event payload).
 */
import { useCallback, useState } from "react";
import type { RunController } from "@/lib/dashboard/run-controller";
import {
  cancelRun,
  parkRun,
  unparkRun,
  requeueStep,
  type RunActionOutcome,
} from "@/lib/dashboard/streams";

export interface RunControlsState {
  pending: boolean;
  error?: string;
  cancel: () => Promise<boolean>;
  park: () => Promise<boolean>;
  unpark: () => Promise<boolean>;
  requeue: (stepId: string) => Promise<boolean>;
}

export function useRunControls(runId: string, controller: RunController): RunControlsState {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string>();

  const run = useCallback(
    async (action: () => Promise<RunActionOutcome>): Promise<boolean> => {
      setPending(true);
      setError(undefined);
      try {
        const outcome = await action();
        // A wrong-state op (a run already terminal, a step already requeued) is
        // a soft 409 — the feed already reflects the real state; reconcile and
        // move on rather than surfacing a hard error.
        if (outcome.kind !== "ok" && outcome.kind !== "conflict") {
          setError(outcome.message);
          void controller.refreshViews();
          return false;
        }
        void controller.refreshViews();
        return true;
      } finally {
        setPending(false);
      }
    },
    [controller],
  );

  return {
    pending,
    error,
    cancel: useCallback(() => run(() => cancelRun(runId)), [run, runId]),
    park: useCallback(() => run(() => parkRun(runId)), [run, runId]),
    unpark: useCallback(() => run(() => unparkRun(runId)), [run, runId]),
    requeue: useCallback((stepId: string) => run(() => requeueStep(runId, stepId)), [run, runId]),
  };
}
