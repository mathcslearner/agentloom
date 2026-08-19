/**
 * Pure run-control availability (ticket 18.6): which lifecycle controls a run's
 * current state admits, and which step is requeueable. Availability derives only
 * from status — the server enforces the transition (a wrong-state op is a 409),
 * so this just avoids offering an action the run cannot take.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { RunView, StepStatus } from "@agentloom/api-client";
import type { DisplayStepStatus } from "./run-state.js";

export interface RunControlAvailability {
  /** Cancel is offered while the run is still doing work. */
  cancel: boolean;
  /** Park pauses a running run's dispatch. */
  park: boolean;
  /** Unpark resumes a parked run. */
  unpark: boolean;
}

/**
 * Derive which run controls apply. A terminal or already-cancelling run offers
 * nothing; a running run can be cancelled or parked; a parked run can be
 * cancelled or unparked.
 */
export function runControls(status: RunView["status"]): RunControlAvailability {
  switch (status) {
    case "running":
      return { cancel: true, park: true, unpark: false };
    case "parked":
      return { cancel: true, park: false, unpark: true };
    default:
      // succeeded / failed / cancelling / cancelled — nothing to steer.
      return { cancel: false, park: false, unpark: false };
  }
}

/** Is a step requeueable — dead-lettered, and its run not cancelled? A requeue
 * of a cancelled run's step is refused by the server (409). */
export function requeueable(stepStatus: StepStatus | DisplayStepStatus, runStatus: RunView["status"]): boolean {
  return stepStatus === "dead_lettered" && runStatus !== "cancelled";
}
