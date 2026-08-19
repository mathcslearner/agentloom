/**
 * Pure run-state reducer for the run-detail page (ticket 18.1).
 *
 * `fromSnapshot` seeds state from a `GET /v1/runs/{id}` body (the WS snapshot);
 * `applyEvent` folds one live event over it. The `event_seq` on the snapshot's
 * run view is the *as-of* cursor: an event is applied to derived state only when
 * its seq exceeds it, so replaying the backfill over a fresher snapshot is a
 * no-op, and re-applying an already-seen event (a suffix after a reconnect) is
 * idempotent — every transition sets an absolute status, never an increment.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { EventEnvelope, EventType } from "@agentloom/engine-client";
import type { RunView, StepView } from "@agentloom/api-client";

/** Display step status: the store vocabulary plus the derived `throttled` skin. */
export type DisplayStepStatus =
  | "pending"
  | "ready"
  | "running"
  | "succeeded"
  | "failed"
  | "skipped"
  | "retrying"
  | "throttled"
  | "dead_lettered"
  | "cancelled"
  | "collected"
  | "awaiting_human";

export interface StepState {
  id: string;
  type: string;
  status: DisplayStepStatus;
  attempt: number;
  /** The last-known full step projection (from a snapshot); absent for a step
   * injected mid-run by a `graph_expanded` event before the next snapshot. */
  view?: StepView;
  /** Provenance for an injected step (origin step id + kind), for 18.2 badges. */
  origin?: { step: string; kind: string };
}

export interface RunState {
  run: RunView;
  steps: Map<string, StepState>;
  /** Highest event seq reflected in derived state (the resume cursor). */
  asOf: number;
}

/** Seed run state from a snapshot / REST run body. */
export function fromSnapshot(snapshot: { run: RunView; steps: StepView[] }): RunState {
  const steps = new Map<string, StepState>();
  for (const s of snapshot.steps) {
    steps.set(s.id, {
      id: s.id,
      type: s.type,
      status: s.status as DisplayStepStatus,
      attempt: s.attempt_count,
      view: s,
    });
  }
  return { run: { ...snapshot.run }, steps, asOf: snapshot.run.event_seq };
}

/** Map a run-lifecycle event to a run status; undefined ⇒ no run-status change. */
function runStatusForEvent(type: EventType): RunView["status"] | undefined {
  switch (type) {
    case "run_created":
      return "running";
    case "run_succeeded":
      return "succeeded";
    case "run_failed":
      return "failed";
    case "run_resumed":
    case "run_unparked":
      return "running";
    case "run_parked":
      return "parked";
    case "run_cancelling":
      return "cancelling";
    case "run_cancelled":
      return "cancelled";
    default:
      return undefined;
  }
}

/** Map a step event to a step status; undefined ⇒ no step-status change. */
function stepStatusForEvent(type: EventType): DisplayStepStatus | undefined {
  switch (type) {
    case "step_ready":
    case "step_requeued":
      return "ready";
    case "step_claimed":
    case "step_reclaimed":
      return "running";
    case "step_succeeded":
      return "succeeded";
    case "step_failed":
      return "failed";
    case "step_retry_scheduled":
    case "step_semantic_retry_scheduled":
      return "retrying";
    case "step_throttled":
      return "throttled";
    case "step_dead_lettered":
      return "dead_lettered";
    case "step_cancelled":
      return "cancelled";
    case "step_collected":
      return "collected";
    case "step_skipped":
      return "skipped";
    case "step_revived":
      return "pending";
    case "approval_requested":
      return "awaiting_human";
    default:
      return undefined;
  }
}

/**
 * Fold one event over the run state. Returns a new state object (referentially
 * new) when the event advanced the cursor, or the same object when the event
 * was already reflected (seq ≤ asOf) — callers can rely on identity to skip
 * re-renders.
 */
export function applyEvent(state: RunState, env: EventEnvelope): RunState {
  if (env.seq <= state.asOf) return state;

  const run: RunView = { ...state.run, event_seq: env.seq };
  const steps = new Map(state.steps);

  const newRunStatus = runStatusForEvent(env.type);
  if (newRunStatus) run.status = newRunStatus;
  if (env.type === "run_parked") run.park_reason = env.payload.reason as RunView["park_reason"];
  if (env.type === "run_unparked" || env.type === "run_resumed") delete run.park_reason;
  if (env.type === "run_cancelling" || env.type === "run_cancelled") {
    // cancel_reason isn't on the event payload; leave any snapshot value.
  }

  // Cost totals ride the cost_updated event (the 18.4 meter reads them live).
  if (env.type === "cost_updated") {
    run.cost = {
      ...run.cost,
      spent_nano_usd: env.payload.run_spent_nano_usd,
      saved_nano_usd: env.payload.run_saved_nano_usd,
    };
  }
  if (env.type === "run_budget_updated" && typeof env.payload === "object") {
    // budget total isn't on the payload; a snapshot poll reconciles it.
  }

  // Injected steps from a graph expansion.
  if (env.type === "graph_expanded") {
    const readied = new Set(env.payload.readied ?? []);
    for (const s of env.payload.delta.steps) {
      if (steps.has(s.id)) continue;
      steps.set(s.id, {
        id: s.id,
        type: s.type,
        status: readied.has(s.id) ? "ready" : "pending",
        attempt: 0,
        origin: { step: env.payload.origin_step, kind: env.payload.origin_kind },
      });
    }
  }

  // Step-status transitions.
  const stepId = env.step_id;
  const nextStatus = stepStatusForEvent(env.type);
  if (stepId && nextStatus) {
    const prev = steps.get(stepId);
    const attempt = attemptFromEvent(env) ?? prev?.attempt ?? 0;
    steps.set(stepId, {
      id: stepId,
      type: prev?.type ?? "",
      ...(prev ?? {}),
      status: nextStatus,
      attempt,
    });
  }

  run.steps_total = Math.max(run.steps_total, steps.size);
  recountFromSteps(run, steps);

  return { run, steps, asOf: env.seq };
}

/** The attempt number an event carries, if any. */
function attemptFromEvent(env: EventEnvelope): number | undefined {
  const p = env.payload as { attempt?: unknown };
  return typeof p.attempt === "number" ? p.attempt : undefined;
}

/** Recompute run counters from the step map (idempotent; never incremented). */
function recountFromSteps(run: RunView, steps: Map<string, StepState>): void {
  let succeeded = 0;
  let failed = 0;
  let skipped = 0;
  let cancelled = 0;
  let collected = 0;
  for (const s of steps.values()) {
    switch (s.status) {
      case "succeeded":
        succeeded++;
        break;
      case "failed":
      case "dead_lettered":
        failed++;
        break;
      case "skipped":
        skipped++;
        break;
      case "cancelled":
        cancelled++;
        break;
      case "collected":
        collected++;
        break;
    }
  }
  run.steps_succeeded = succeeded;
  run.steps_failed = failed;
  run.steps_skipped = skipped;
  run.steps_cancelled = cancelled;
  run.steps_collected = collected;
}
