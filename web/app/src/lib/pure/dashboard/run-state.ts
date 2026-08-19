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
  /** How many times this step's lease was reclaimed by another worker (18.2):
   * the count of `step_reclaimed` events — the visible signal of the
   * crash-recovery demo (a killed worker's step is taken over). */
  reclaims: number;
  /** The type of the event that last moved this step, and its seq — the cue the
   * live DAG keys its status skin + enter/transition animation on (18.2). */
  lastEventType?: EventType;
  lastEventSeq?: number;
  /** The last-known full step projection (from a snapshot); absent for a step
   * injected mid-run by a `graph_expanded` event before the next snapshot. */
  view?: StepView;
  /** The run's `event_seq` when `view` was captured (ticket 18.3). A refetched
   * detail body replaces `view` only when its `event_seq` is at least this — so
   * a stale body never clobbers a fresher one, mirroring the `asOf` guard. */
  viewSeq?: number;
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
      // A snapshot does not carry the reclaim count; it is accumulated live
      // from step_reclaimed events (the transport failure count is a separate,
      // outcome-based measure). A reconnect re-seeds from a fresh snapshot, so
      // this resets to 0 and is re-accumulated from the re-backfilled tail —
      // the seq guard keeps that idempotent.
      reclaims: 0,
      view: s,
      viewSeq: snapshot.run.event_seq,
    });
  }
  return { run: { ...snapshot.run }, steps, asOf: snapshot.run.event_seq };
}

/**
 * Fold a refetched run-detail body into the state's step views (ticket 18.3):
 * the inspector fetches `GET /v1/runs/{id}` to pick up new attempts/verdicts.
 * A step's `view` is replaced only when the body's `event_seq` is at least the
 * captured `viewSeq` (never regress to a stale body), and the live
 * event-derived fields (`status`, `attempt`, `reclaims`, `lastEvent*`) are
 * preserved — the detail body is authoritative for `view` only, the event feed
 * for live status. Steps present live but absent from the body (injected after
 * the body was read) keep their event-derived state.
 */
export function mergeRunResponse(
  state: RunState,
  body: { run: RunView; steps: StepView[] },
): RunState {
  const bodySeq = body.run.event_seq;
  // Fold the run-level projection (cost/budget/status/park_reason) only when the
  // body is at least as fresh as the derived state — so a refetch after a budget
  // raise or unpark reconciles those fields, but a stale body never regresses the
  // event-derived live status/totals (mirrors the applyEvent asOf guard).
  const run: RunView = bodySeq >= state.asOf ? { ...state.run, ...body.run } : state.run;
  const steps = new Map(state.steps);
  for (const s of body.steps) {
    const prev = steps.get(s.id);
    if (prev && prev.viewSeq !== undefined && prev.viewSeq > bodySeq) continue;
    steps.set(s.id, {
      id: s.id,
      type: prev?.type || s.type,
      status: prev?.status ?? (s.status as DisplayStepStatus),
      attempt: prev?.attempt ?? s.attempt_count,
      reclaims: prev?.reclaims ?? 0,
      lastEventType: prev?.lastEventType,
      lastEventSeq: prev?.lastEventSeq,
      origin: prev?.origin,
      view: s,
      viewSeq: bodySeq,
    });
  }
  return { ...state, run, steps };
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
  // The running totals are non-decreasing in seq order by construction (10.5),
  // so folding them here keeps the header meter live with no cost refetch.
  if (env.type === "cost_updated") {
    run.cost = {
      ...run.cost,
      spent_nano_usd: env.payload.run_spent_nano_usd,
      saved_nano_usd: env.payload.run_saved_nano_usd,
      // A cost_updated on a budgeted run carries the run's budget after the
      // bump; carry it so a fresh subscriber's meter shows the cap at once.
      ...(env.payload.budget_nano_usd !== undefined
        ? { budget_nano_usd: env.payload.budget_nano_usd }
        : {}),
    };
  }
  // A budget raise (18.4 PATCH) carries the new total on the payload — the
  // meter's budget bar reflects it live, no snapshot poll needed.
  if (env.type === "run_budget_updated") {
    run.cost = { ...run.cost, budget_nano_usd: env.payload.budget_nano_usd };
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
        reclaims: 0,
        lastEventType: env.type,
        lastEventSeq: env.seq,
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
    const reclaims = (prev?.reclaims ?? 0) + (env.type === "step_reclaimed" ? 1 : 0);
    steps.set(stepId, {
      id: stepId,
      type: prev?.type ?? "",
      ...(prev ?? {}),
      status: nextStatus,
      attempt,
      reclaims,
      lastEventType: env.type,
      lastEventSeq: env.seq,
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
