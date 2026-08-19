/**
 * Pure DLQ-list logic (ticket 18.6): the operator dead-letter row model, its
 * live-event fold, filter predicates, and the filter ⇄ URL-search-params codec.
 * Mirrors approval-list.ts — a row is a `DeadLetterListItem` (the
 * `GET /v1/dead-letters` shape) keyed by (run_id, step_id, seq), newest-first.
 *
 * The list goes live off the firehose: a `step_dead_lettered` event marks its
 * step (a fresh open row appears after a refetch that carries the error
 * context, like the 18.5 partial approval), and a `step_requeued` /
 * `step_revived` event closes the open row (the step is no longer dead-lettered).
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { EventEnvelope, EventType } from "@agentloom/engine-client";
import type { DeadLetterListItem } from "@agentloom/api-client";

/** A DLQ list row: the REST item plus a per-row event cursor (idempotence). */
export interface DlqRow extends DeadLetterListItem {
  /** Highest event seq folded into this row. */
  lastSeq: number;
}

/** A stable key for a death record (a step can die more than once). */
export function dlqKey(row: { run_id: string; step_id: string; seq: number }): string {
  return `${row.run_id}/${row.step_id}/${row.seq}`;
}

/** The events the DLQ firehose subscription needs. A step leaving the DLQ
 * (requeued or its ancestor revived) closes the open row. */
export const DLQ_EVENT_TYPES: readonly EventType[] = ["step_dead_lettered", "step_requeued"];

/** Fold one step event over a row (same object when already reflected). A
 * `step_requeued` for this row's step closes it (no longer open); a repeated
 * `step_dead_lettered` keeps it open. */
export function applyDlqEvent(row: DlqRow, env: EventEnvelope): DlqRow {
  if (env.seq <= row.lastSeq) return row;
  const stepId = (env.payload as { step_id?: string }).step_id;
  if (stepId !== row.step_id || env.run_id !== row.run_id) return row;
  const next: DlqRow = { ...row, lastSeq: env.seq };
  switch (env.type) {
    case "step_requeued":
      next.open = false;
      next.step_status = "ready";
      break;
    case "step_dead_lettered":
      next.open = true;
      next.step_status = "dead_lettered";
      break;
  }
  return next;
}

export type DlqStatusFilter = "open" | "all";
export type DlqSource = "retries_exhausted" | "permanent" | "poison";

export interface DlqFilters {
  /** "open" (default) = still requeueable; "all" = every historical death. */
  status: DlqStatusFilter;
  runId?: string;
  source?: DlqSource;
}

/** Does a row satisfy the active filters (client-side, for a live-arrived
 * row before a refetch)? */
export function matchesDlqFilters(row: DeadLetterListItem, f: DlqFilters): boolean {
  if (f.status === "open" && !row.open) return false;
  if (f.runId && row.run_id !== f.runId) return false;
  if (f.source && row.source !== f.source) return false;
  return true;
}

/** The query params `GET /v1/dead-letters` accepts for the active filters. */
export function dlqFiltersToQuery(f: DlqFilters): { status: DlqStatusFilter; run_id?: string; source?: DlqSource } {
  const q: { status: DlqStatusFilter; run_id?: string; source?: DlqSource } = { status: f.status };
  if (f.runId) q.run_id = f.runId;
  if (f.source) q.source = f.source;
  return q;
}

/** Serialize filters to URL search params (defaults omitted). */
export function dlqFiltersToSearchParams(f: DlqFilters): URLSearchParams {
  const p = new URLSearchParams();
  if (f.status === "all") p.set("status", "all");
  if (f.runId) p.set("run", f.runId);
  if (f.source) p.set("source", f.source);
  return p;
}

const DLQ_SOURCES: readonly DlqSource[] = ["retries_exhausted", "permanent", "poison"];

/** Parse filters from URL search params; status defaults to "open". */
export function dlqFiltersFromSearchParams(p: URLSearchParams): DlqFilters {
  const f: DlqFilters = { status: "open" };
  if (p.get("status") === "all") f.status = "all";
  const run = p.get("run");
  if (run) f.runId = run;
  const source = p.get("source");
  if (source && (DLQ_SOURCES as readonly string[]).includes(source)) f.source = source as DlqSource;
  return f;
}

/** Insert/replace a row in a newest-first list, sorted by created_at desc then
 * run/step/seq (the `GET /v1/dead-letters` order). */
export function upsertDlqRow(rows: DlqRow[], row: DlqRow): DlqRow[] {
  const key = dlqKey(row);
  const out = rows.filter((r) => dlqKey(r) !== key);
  out.push(row);
  out.sort((a, b) => {
    if (a.created_at !== b.created_at) return a.created_at < b.created_at ? 1 : -1;
    if (a.run_id !== b.run_id) return a.run_id < b.run_id ? 1 : -1;
    if (a.step_id !== b.step_id) return a.step_id < b.step_id ? 1 : -1;
    return b.seq - a.seq;
  });
  return out;
}
