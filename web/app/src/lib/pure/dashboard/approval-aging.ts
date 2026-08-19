/**
 * Aging + deadline derivations for an approval (ticket 18.5). Pure, `now`
 * injected — the inbox colours a row by how long it has waited, and shows a
 * countdown to its timeout deadline.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { ApprovalRecord } from "./approvals";

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

export type AgeTier = "fresh" | "aging" | "stale";

export interface Aging {
  ageMs: number;
  tier: AgeTier;
  label: string;
}

/** How long a pending approval has waited, tiered fresh (<1h) / aging (<24h) /
 * stale (≥24h). For a decided row the age is frozen at the decision time. */
export function deriveAging(rec: ApprovalRecord, now: number): Aging {
  const created = Date.parse(rec.created_at);
  const end = rec.decided_at ? Date.parse(rec.decided_at) : now;
  const ageMs = Math.max(0, end - created);
  const tier: AgeTier = ageMs >= DAY ? "stale" : ageMs >= HOUR ? "aging" : "fresh";
  return { ageMs, tier, label: humanizeDuration(ageMs) };
}

export interface Deadline {
  /** ms until the timeout fires; negative if already overdue. */
  remainingMs: number;
  overdue: boolean;
  label: string;
}

/** The countdown to a pending approval's `timeout_at`, if it has one. */
export function deriveDeadline(rec: ApprovalRecord, now: number): Deadline | undefined {
  if (!rec.timeout_at) return undefined;
  const at = Date.parse(rec.timeout_at);
  if (Number.isNaN(at)) return undefined;
  const remainingMs = at - now;
  const overdue = remainingMs < 0;
  return {
    remainingMs,
    overdue,
    label: overdue
      ? `overdue by ${humanizeDuration(-remainingMs)}`
      : `expires in ${humanizeDuration(remainingMs)}`,
  };
}

/** A compact human duration: "3d", "5h", "12m", "just now". */
export function humanizeDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return s <= 1 ? "just now" : `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  const d = Math.floor(h / 24);
  return `${d}d`;
}
