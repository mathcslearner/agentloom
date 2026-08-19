/**
 * Pure run-list logic (ticket 18.1): fold a firehose event over a run row's
 * counters, filter predicates, and the filter ⇄ URL-search-params codec.
 *
 * A list row carries only counters (no per-step map), so — unlike the detail
 * reducer — terminal step events *increment* the counters. That is safe because
 * the per-row `event_seq` guard means each event is applied at most once
 * (`applyRunListEvent` is a no-op for seq ≤ the row's cursor), so a redelivered
 * event never double-counts.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { EventEnvelope, EventType } from "@agentloom/engine-client";
import type { RunView, RunStatus } from "@agentloom/api-client";

/** The event types the run-list firehose subscription needs. */
export const RUN_LIST_EVENT_TYPES: readonly EventType[] = [
  "run_created",
  "run_succeeded",
  "run_failed",
  "run_parked",
  "run_unparked",
  "run_resumed",
  "run_cancelling",
  "run_cancelled",
  "step_succeeded",
  "step_skipped",
  "step_dead_lettered",
  "step_cancelled",
  "step_collected",
  "graph_expanded",
  "cost_updated",
];

/** Fold one firehose event over a run row. Returns the same object when the
 * event was already reflected (seq ≤ row.event_seq). */
export function applyRunListEvent(row: RunView, env: EventEnvelope): RunView {
  if (env.seq <= row.event_seq) return row;
  const next: RunView = { ...row, event_seq: env.seq };

  switch (env.type) {
    case "run_succeeded":
      next.status = "succeeded";
      break;
    case "run_failed":
      next.status = "failed";
      break;
    case "run_parked":
      next.status = "parked";
      next.park_reason = env.payload.reason as RunView["park_reason"];
      break;
    case "run_unparked":
    case "run_resumed":
      next.status = "running";
      delete next.park_reason;
      break;
    case "run_cancelling":
      next.status = "cancelling";
      break;
    case "run_cancelled":
      next.status = "cancelled";
      break;
    case "step_succeeded":
      next.steps_succeeded += 1;
      break;
    case "step_skipped":
      next.steps_skipped += 1;
      break;
    case "step_dead_lettered":
      next.steps_failed += 1;
      break;
    case "step_cancelled":
      next.steps_cancelled += 1;
      break;
    case "step_collected":
      next.steps_collected += 1;
      break;
    case "graph_expanded":
      next.steps_total += env.payload.delta.steps.length;
      break;
    case "cost_updated":
      next.cost = {
        ...next.cost,
        spent_nano_usd: env.payload.run_spent_nano_usd,
        saved_nano_usd: env.payload.run_saved_nano_usd,
      };
      break;
  }
  return next;
}

export interface RunFilters {
  status?: RunStatus;
  definitionId?: string;
  createdAfter?: string; // RFC 3339
  createdBefore?: string;
}

/** Does a run row satisfy the active filters? Used to place a newly-discovered
 * run and to keep the list consistent. */
export function matchesFilters(row: RunView, f: RunFilters): boolean {
  if (f.status && row.status !== f.status) return false;
  if (f.definitionId && row.definition_id !== f.definitionId) return false;
  if (f.createdAfter && !(row.created_at > f.createdAfter)) return false;
  if (f.createdBefore && !(row.created_at < f.createdBefore)) return false;
  return true;
}

/** The query params `GET /v1/runs` accepts for the active filters. */
export function filtersToQuery(f: RunFilters): {
  status?: RunStatus;
  definition_id?: string;
  created_after?: string;
  created_before?: string;
} {
  const q: {
    status?: RunStatus;
    definition_id?: string;
    created_after?: string;
    created_before?: string;
  } = {};
  if (f.status) q.status = f.status;
  if (f.definitionId) q.definition_id = f.definitionId;
  if (f.createdAfter) q.created_after = f.createdAfter;
  if (f.createdBefore) q.created_before = f.createdBefore;
  return q;
}

/** Serialize filters to URL search params (empty values omitted). */
export function filtersToSearchParams(f: RunFilters): URLSearchParams {
  const p = new URLSearchParams();
  if (f.status) p.set("status", f.status);
  if (f.definitionId) p.set("definition", f.definitionId);
  if (f.createdAfter) p.set("after", f.createdAfter);
  if (f.createdBefore) p.set("before", f.createdBefore);
  return p;
}

const RUN_STATUSES: readonly RunStatus[] = [
  "running",
  "succeeded",
  "failed",
  "parked",
  "cancelling",
  "cancelled",
];

/** Parse filters from URL search params (inverse of filtersToSearchParams). */
export function filtersFromSearchParams(p: URLSearchParams): RunFilters {
  const f: RunFilters = {};
  const status = p.get("status");
  if (status && (RUN_STATUSES as readonly string[]).includes(status)) f.status = status as RunStatus;
  const def = p.get("definition");
  if (def) f.definitionId = def;
  const after = p.get("after");
  if (after) f.createdAfter = after;
  const before = p.get("before");
  if (before) f.createdBefore = before;
  return f;
}

/** Named time presets → a `createdAfter` cutoff computed from `now` (ms). */
export const TIME_PRESETS: Record<string, number | undefined> = {
  all: undefined,
  "15m": 15 * 60 * 1000,
  "1h": 60 * 60 * 1000,
  "24h": 24 * 60 * 60 * 1000,
  "7d": 7 * 24 * 60 * 60 * 1000,
};

/** Compute the `createdAfter` RFC-3339 cutoff for a preset relative to `now`. */
export function presetCutoff(preset: string, now: number): string | undefined {
  const ms = TIME_PRESETS[preset];
  if (!ms) return undefined;
  return new Date(now - ms).toISOString();
}
