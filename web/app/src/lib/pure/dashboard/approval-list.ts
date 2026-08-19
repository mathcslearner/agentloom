/**
 * Pure approvals-inbox logic (ticket 18.5): the inbox row model, its
 * live-event fold, filter predicates, and the filter ⇄ URL-search-params codec.
 * Mirrors run-list.ts but for approvals — a row is an `ApprovalView` (the
 * `GET /v1/approvals` shape), keyed by id, folded absolutely by status (an
 * approval has no counters), and the list is oldest-first (append on arrival),
 * the opposite of the newest-first runs list.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { EventEnvelope, EventType } from "@agentloom/engine-client";
import type { ApprovalView } from "@agentloom/api-client";

export type InboxStatus = ApprovalView["status"];

/** An inbox row: the REST view plus a per-row event cursor (idempotence). */
export interface InboxRow extends ApprovalView {
  /** Highest event seq folded into this row. */
  lastSeq: number;
}

/** The approval events the inbox firehose subscription needs. */
export const INBOX_EVENT_TYPES: readonly EventType[] = [
  "approval_requested",
  "approval_decided",
  "approval_expired",
  "approval_cancelled",
];

/** Fold one approval event over a row (same object when already reflected). */
export function applyInboxEvent(row: InboxRow, env: EventEnvelope): InboxRow {
  if (env.seq <= row.lastSeq) return row;
  const next: InboxRow = { ...row, lastSeq: env.seq };
  switch (env.type) {
    case "approval_requested":
      next.status = "pending";
      break;
    case "approval_decided":
      next.status = env.payload.decision === "approve" ? "approved" : "rejected";
      next.decision = env.payload.decision as ApprovalView["decision"];
      next.comment = env.payload.comment;
      next.decided_by = env.payload.decided_by;
      next.decision_source = env.payload.source as ApprovalView["decision_source"];
      break;
    case "approval_expired":
      if (env.payload.action === "run_parked") {
        next.status = "pending";
        next.expired_at = env.payload.timeout_at ?? next.expired_at ?? env.ts;
      } else {
        next.status = "expired";
        next.decision = env.payload.decision as ApprovalView["decision"];
        next.expired_at = env.payload.timeout_at ?? env.ts;
      }
      break;
    case "approval_cancelled":
      next.status = "cancelled";
      break;
  }
  return next;
}

export interface InboxFilters {
  /** Undefined ⇒ all statuses; the page defaults to `pending`. */
  status?: InboxStatus;
  runId?: string;
}

/** Does an inbox row satisfy the active filters? */
export function matchesInboxFilters(row: ApprovalView, f: InboxFilters): boolean {
  if (f.status && row.status !== f.status) return false;
  if (f.runId && row.run_id !== f.runId) return false;
  return true;
}

/** The query params `GET /v1/approvals` accepts for the active filters. */
export function inboxFiltersToQuery(f: InboxFilters): { status?: InboxStatus; run_id?: string } {
  const q: { status?: InboxStatus; run_id?: string } = {};
  if (f.status) q.status = f.status;
  if (f.runId) q.run_id = f.runId;
  return q;
}

/** Serialize filters to URL search params (empty values omitted). */
export function inboxFiltersToSearchParams(f: InboxFilters): URLSearchParams {
  const p = new URLSearchParams();
  if (f.status) p.set("status", f.status);
  if (f.runId) p.set("run", f.runId);
  return p;
}

const INBOX_STATUSES: readonly InboxStatus[] = [
  "pending",
  "approved",
  "rejected",
  "expired",
  "cancelled",
];

/** Parse filters from URL search params. When `status` is absent the caller
 * decides the default (the page defaults to `pending`). */
export function inboxFiltersFromSearchParams(p: URLSearchParams): InboxFilters {
  const f: InboxFilters = {};
  const status = p.get("status");
  if (status === "all") {
    // explicit all ⇒ no status filter
  } else if (status && (INBOX_STATUSES as readonly string[]).includes(status)) {
    f.status = status as InboxStatus;
  } else if (status === null) {
    f.status = "pending";
  }
  const run = p.get("run");
  if (run) f.runId = run;
  return f;
}

/** Insert/replace a row in an oldest-first list, keeping it sorted by
 * created_at then id (the `GET /v1/approvals` order). */
export function upsertRow(rows: InboxRow[], row: InboxRow): InboxRow[] {
  const out = rows.filter((r) => r.id !== row.id);
  out.push(row);
  out.sort((a, b) => (a.created_at < b.created_at ? -1 : a.created_at > b.created_at ? 1 : a.id < b.id ? -1 : 1));
  return out;
}
