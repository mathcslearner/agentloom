/**
 * Pure event presentation for the timeline strip (ticket 18.1). Maps a
 * normalized event envelope onto a display category (for filter chips) and a
 * one-line human summary. The `summarize` switch is exhaustive over `EventType`
 * — a new backend event type (added to `docs/schema/events.v1.json`, regenerated
 * into `@agentloom/engine-client`) fails typecheck here until it is handled, so
 * the timeline can never silently drop an event kind.
 *
 * No React/UI imports — this lives under the app's `pure/` eslint boundary.
 */
import type { EventEnvelope, EventType } from "@agentloom/engine-client";

/** Coarse buckets the timeline filters by. */
export type EventCategory = "run" | "step" | "cost" | "graph" | "context" | "approval" | "guard";

export const EVENT_CATEGORIES: readonly EventCategory[] = [
  "run",
  "step",
  "cost",
  "graph",
  "context",
  "approval",
  "guard",
];

const CATEGORY_BY_TYPE: Record<EventType, EventCategory> = {
  run_created: "run",
  run_succeeded: "run",
  run_failed: "run",
  run_resumed: "run",
  run_parked: "run",
  run_unparked: "run",
  run_cancelling: "run",
  run_cancelled: "run",
  step_ready: "step",
  step_claimed: "step",
  step_succeeded: "step",
  step_failed: "step",
  step_skipped: "step",
  step_reclaimed: "step",
  step_retry_scheduled: "step",
  step_throttled: "step",
  step_semantic_retry_scheduled: "step",
  step_dead_lettered: "step",
  step_cancelled: "step",
  step_collected: "step",
  step_requeued: "step",
  step_revived: "step",
  cost_updated: "cost",
  cost_unknown_model: "cost",
  budget_exceeded: "cost",
  run_budget_updated: "cost",
  model_downgraded: "cost",
  blackboard_updated: "context",
  context_assembled: "context",
  context_revision: "context",
  graph_expanded: "graph",
  loop_exhausted: "graph",
  loop_no_progress: "graph",
  guard_tripped: "guard",
  approval_requested: "approval",
  approval_cancelled: "approval",
  approval_decided: "approval",
  approval_expired: "approval",
  approval_notified: "approval",
  approval_notification_failed: "approval",
};

/** The display category for an event type. */
export function eventCategory(type: EventType): EventCategory {
  return CATEGORY_BY_TYPE[type];
}

/**
 * A one-line summary of an event. Exhaustive over `EventType` (the `never` in
 * the default arm is the compile-time guard).
 */
export function summarizeEvent(env: EventEnvelope): string {
  switch (env.type) {
    case "run_created":
      return `run created from "${env.payload.name}" (${env.payload.steps_total} steps)`;
    case "run_succeeded":
      return "run succeeded";
    case "run_failed":
      return "run failed";
    case "run_resumed":
      return "run resumed";
    case "run_parked":
      return `run parked (${env.payload.reason})`;
    case "run_unparked":
      return "run unparked";
    case "run_cancelling":
      return "run cancelling";
    case "run_cancelled":
      return "run cancelled";
    case "step_ready":
      return `${env.payload.step_id} ready`;
    case "step_claimed":
      return `${env.payload.step_id} claimed (attempt ${env.payload.attempt})`;
    case "step_succeeded":
      return `${env.payload.step_id} succeeded (attempt ${env.payload.attempt})`;
    case "step_failed":
      return `${env.payload.step_id} attempt ${env.payload.attempt} failed`;
    case "step_skipped":
      return `${env.payload.step_id} skipped`;
    case "step_reclaimed":
      return `${env.payload.step_id} reclaimed (attempt ${env.payload.attempt})`;
    case "step_retry_scheduled":
      return `${env.payload.step_id} retry scheduled (${env.payload.class})`;
    case "step_throttled":
      return `${env.payload.step_id} throttled on ${env.payload.resource}`;
    case "step_semantic_retry_scheduled":
      return `${env.payload.step_id} semantic retry ${env.payload.semantic_attempt}/${env.payload.max_attempts} (${env.payload.issue_count} issues)`;
    case "step_dead_lettered":
      return `${env.payload.step_id} dead-lettered (${env.payload.source})`;
    case "step_cancelled":
      return `${env.payload.step_id} cancelled`;
    case "step_collected":
      return `${env.payload.step_id} collected (tolerated failure)`;
    case "step_requeued":
      return `${env.payload.step_id} requeued`;
    case "step_revived":
      return `${env.payload.step_id} revived`;
    case "cost_updated":
      return `cost +${nano(env.payload.cost_nano_usd)} (run ${nano(env.payload.run_spent_nano_usd)})`;
    case "cost_unknown_model":
      return `unknown model "${env.payload.model}" priced at fallback`;
    case "budget_exceeded":
      return `budget exceeded on ${env.payload.step_id} (${env.payload.action})`;
    case "run_budget_updated":
      return "run budget updated";
    case "model_downgraded":
      return `${env.payload.step_id} downgraded ${env.payload.from_model} → ${env.payload.to_model}`;
    case "blackboard_updated":
      return `blackboard "${env.payload.key}" v${env.payload.version}`;
    case "context_assembled":
      return `${env.payload.step_id} context assembled (${env.payload.context_tokens} tok)`;
    case "context_revision":
      return `${env.payload.step_id} context ${env.payload.strategy} (${env.payload.tokens_before}→${env.payload.tokens_after})`;
    case "graph_expanded":
      return `graph expanded by ${env.payload.origin_step} (${env.payload.origin_kind}) → v${env.payload.to_version}`;
    case "loop_exhausted":
      return `loop ${env.payload.loop_source_step} exhausted (${env.payload.action})`;
    case "loop_no_progress":
      return `loop ${env.payload.loop_source_step} no progress (${env.payload.action})`;
    case "guard_tripped":
      return `guard ${env.payload.guard} tripped (${env.payload.current}/${env.payload.cap} ${env.payload.unit})`;
    case "approval_requested":
      return `approval requested: ${env.payload.title}`;
    case "approval_cancelled":
      return `approval cancelled (${env.payload.reason})`;
    case "approval_decided":
      return `approval ${env.payload.decision} by ${env.payload.decided_by}`;
    case "approval_expired":
      return `approval expired (${env.payload.action})`;
    case "approval_notified":
      return `approval notified (${env.payload.status_code})`;
    case "approval_notification_failed":
      return `approval notification failed (${env.payload.reason})`;
    default: {
      const exhaustive: never = env;
      return `unknown event ${(exhaustive as { type?: string }).type ?? ""}`;
    }
  }
}

/** Render an integer nano-USD amount as a short $ string. */
function nano(n: number | undefined): string {
  if (!n) return "$0";
  return `$${(n / 1e9).toFixed(6).replace(/0+$/, "").replace(/\.$/, "")}`;
}

/** The step id an event concerns, if any (the lifted envelope `step_id`). */
export function eventStepId(env: EventEnvelope): string | undefined {
  return env.step_id;
}
